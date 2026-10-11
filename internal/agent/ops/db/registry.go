package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/cloudground-it/cloudground/internal/site"
)

// The agent's own record of which database and which account belongs to
// which site (SPEC §2). Names are chosen by people now, so a name no longer
// says whose it is: the record does. It is root's (0600, beside the work
// directory, which the API's user cannot reach), written atomically on
// every change, and every handler that names a database or an account of a
// site checks it before any SQL runs. An install from before the record
// gets it rebuilt at first start from the server: every site_<id> database
// and account is site <id>'s, with all privileges on its database.

// registryVersion is the file's format.
const registryVersion = 1

// registryFile is the record on disk.
type registryFile struct {
	Version   int                     `json:"version"`
	Databases map[string]int64        `json:"databases"`
	Users     map[string]registryUser `json:"users"`
}

// registryUser is an account: its site and its grants (database → all or
// read).
type registryUser struct {
	Site   int64             `json:"site"`
	Grants map[string]string `json:"grants"`
}

// Registry is the record, loaded once.
type Registry struct {
	path string
	// scan lists the server's databases and accounts, for the rebuild.
	scan func(ctx context.Context) (dbs, users []string, err error)

	mu     sync.Mutex
	loaded bool
	data   registryFile
}

// errNoRecord is a registry that could not be loaded: nothing of a site is
// known, so every request naming a database is refused until it is.
var errNoRecord = errors.New("the agent's record of the sites' databases is not available")

// load reads the record, or rebuilds it when there is none. Called with mu
// held.
func (r *Registry) load(ctx context.Context) error {
	if r.loaded {
		return nil
	}
	f, rebuilt, err := r.read(ctx)
	if err != nil {
		return err
	}
	r.data = f
	if rebuilt {
		if err := r.save(); err != nil {
			return fmt.Errorf("%w: %v", errNoRecord, err)
		}
	}
	r.loaded = true
	return nil
}

// read returns the record on disk, or the one rebuilt from the server when
// there is none (rebuilt is then true); it writes nothing.
func (r *Registry) read(ctx context.Context) (f registryFile, rebuilt bool, err error) {
	raw, err := os.ReadFile(r.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &f); err != nil {
			return f, false, fmt.Errorf("%w: %s is damaged: %v", errNoRecord, r.path, err)
		}
		if f.Version != registryVersion {
			return f, false, fmt.Errorf("%w: %s has version %d", errNoRecord, r.path, f.Version)
		}
	case errors.Is(err, fs.ErrNotExist):
		if f, err = r.rebuild(ctx); err != nil {
			return f, false, fmt.Errorf("%w: %v", errNoRecord, err)
		}
		rebuilt = true
	default:
		return f, false, fmt.Errorf("%w: %v", errNoRecord, err)
	}
	if f.Databases == nil {
		f.Databases = map[string]int64{}
	}
	if f.Users == nil {
		f.Users = map[string]registryUser{}
	}
	return f, rebuilt, nil
}

// dryUpdate runs fn on a copy of the record and writes nothing, not even a
// record rebuilt because none was on disk (adopt-databases --dry-run).
func (r *Registry) dryUpdate(ctx context.Context, fn func(f *registryFile) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := cloneRecord(r.data)
	if !r.loaded {
		var err error
		if f, _, err = r.read(ctx); err != nil {
			return err
		}
	}
	return fn(&f)
}

// rebuild makes the record of an install from before it: the site_<id>
// databases and accounts on the server, each site <id>'s.
func (r *Registry) rebuild(ctx context.Context) (registryFile, error) {
	f := registryFile{Version: registryVersion, Databases: map[string]int64{}, Users: map[string]registryUser{}}
	if r.scan == nil {
		return f, nil
	}
	dbs, users, err := r.scan(ctx)
	if err != nil {
		return f, fmt.Errorf("list the server's databases: %w", err)
	}
	for _, d := range dbs {
		if id := site.PanelDefaultSite(d); id > 0 {
			f.Databases[d] = id
		}
	}
	for _, u := range users {
		id := site.PanelDefaultSite(u)
		if id == 0 {
			continue
		}
		ru := registryUser{Site: id, Grants: map[string]string{}}
		if f.Databases[u] == id {
			ru.Grants[u] = site.GrantAll
		}
		f.Users[u] = ru
	}
	return f, nil
}

// save writes the record atomically, root's alone. Called with mu held.
func (r *Registry) save() error {
	raw, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".databases-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.path)
}

// update runs fn on a copy of the record and saves the result; nothing
// changes when fn or the save fails.
func (r *Registry) update(ctx context.Context, fn func(f *registryFile) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.load(ctx); err != nil {
		return err
	}
	before := cloneRecord(r.data)
	if err := fn(&r.data); err != nil {
		r.data = before
		return err
	}
	if err := r.save(); err != nil {
		r.data = before
		return fmt.Errorf("record the change: %w", err)
	}
	return nil
}

// view runs fn on the loaded record.
func (r *Registry) view(ctx context.Context, fn func(f *registryFile)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.load(ctx); err != nil {
		return err
	}
	fn(&r.data)
	return nil
}

func cloneRecord(f registryFile) registryFile {
	out := registryFile{Version: f.Version, Databases: make(map[string]int64, len(f.Databases)), Users: make(map[string]registryUser, len(f.Users))}
	for k, v := range f.Databases {
		out.Databases[k] = v
	}
	for k, u := range f.Users {
		g := make(map[string]string, len(u.Grants))
		for d, l := range u.Grants {
			g[d] = l
		}
		out.Users[k] = registryUser{Site: u.Site, Grants: g}
	}
	return out
}

// DatabaseOwner returns the site a database is recorded for, or 0.
func (r *Registry) DatabaseOwner(ctx context.Context, name string) (int64, error) {
	var id int64
	err := r.view(ctx, func(f *registryFile) { id = f.Databases[name] })
	return id, err
}

// UserOwner returns the site an account is recorded for, or 0.
func (r *Registry) UserOwner(ctx context.Context, name string) (int64, error) {
	var id int64
	err := r.view(ctx, func(f *registryFile) { id = f.Users[name].Site })
	return id, err
}

// SiteRecord is what the record holds of one site.
type SiteRecord struct {
	Databases []string
	// Users maps each account to its grants (database → all or read).
	Users map[string]map[string]string
}

// Site returns what is recorded for the site.
func (r *Registry) Site(ctx context.Context, siteID int64) (SiteRecord, error) {
	rec := SiteRecord{Users: map[string]map[string]string{}}
	err := r.view(ctx, func(f *registryFile) {
		for d, id := range f.Databases {
			if id == siteID {
				rec.Databases = append(rec.Databases, d)
			}
		}
		for u, ru := range f.Users {
			if ru.Site == siteID {
				g := map[string]string{}
				for d, l := range ru.Grants {
					g[d] = l
				}
				rec.Users[u] = g
			}
		}
	})
	sort.Strings(rec.Databases)
	return rec, err
}

// AllUsers lists every recorded account.
func (r *Registry) AllUsers(ctx context.Context) ([]string, error) {
	var out []string
	err := r.view(ctx, func(f *registryFile) {
		for u := range f.Users {
			out = append(out, u)
		}
	})
	sort.Strings(out)
	return out, err
}
