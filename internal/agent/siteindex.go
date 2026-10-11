package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The site index (internal/agentrpc SPEC.md §3.5): the agent's own record
// of which site id has which domain, so that a request cannot pair one
// site's id or user with another site's domain. The site user is always
// cg-site-<id>, so the record is id -> domain.

// createMethods may name a site the index does not know yet, and record it.
var createMethods = map[string]bool{
	agentrpc.CreateSite.Name():    true,
	agentrpc.CreateStaging.Name(): true,
}

// indexFile is the index on disk.
type indexFile struct {
	Version int               `json:"version"`
	Sites   map[string]string `json:"sites"` // id -> domain
}

// siteIndex is the loaded index. The zero value is unloaded.
type siteIndex struct {
	a      *Agent
	mu     sync.Mutex
	loaded bool
	byID   map[int64]string
	byDom  map[string]int64
}

// errMismatch is the refusal of a request whose site is not the one the
// agent provisioned.
func errMismatch(s state.Site, why string) error {
	return agentrpc.Wrap(agentrpc.CodeInvalid, "the site in the request does not match the site this server provisioned",
		fmt.Errorf("site %d %s (%s): %s", s.ID, s.Domain, s.SystemUser, why))
}

// userOf is the system user a site with this id has.
func userOf(id int64) string { return "cg-site-" + strconv.FormatInt(id, 10) }

// idOf reads the id out of a site user's name.
func idOf(u string) (int64, bool) {
	n, ok := strings.CutPrefix(u, "cg-site-")
	if !ok || !site.ValidSystemUser(u) {
		return 0, false
	}
	id, err := strconv.ParseInt(n, 10, 64)
	return id, err == nil && id > 0
}

// Check implements agentrpc.SiteRegistry.
func (x *siteIndex) Check(method string, s state.Site) error {
	if s.ID <= 0 || s.SystemUser != userOf(s.ID) {
		return errMismatch(s, "the system user is not the one named after the id")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.load(); err != nil {
		return err
	}
	dom, idKnown := x.byID[s.ID]
	id, domKnown := x.byDom[s.Domain]
	switch {
	case idKnown && dom != s.Domain:
		return errMismatch(s, "the agent provisioned this id as "+dom)
	case domKnown && id != s.ID:
		return errMismatch(s, fmt.Sprintf("the agent provisioned this domain as site %d", id))
	case idKnown:
		return nil
	}
	// Unknown: a new site, or one whose tree shows it is this one.
	if createMethods[method] || x.treeIs(s) {
		return x.put(s.ID, s.Domain)
	}
	return nil
}

// Done implements agentrpc.SiteRegistry: a deleted site, and a creation
// that failed and left no tree, leave the index.
func (x *siteIndex) Done(method string, sites []state.Site, err error) {
	forget := func(s state.Site) {
		x.mu.Lock()
		defer x.mu.Unlock()
		if x.load() != nil || x.byID[s.ID] != s.Domain {
			return
		}
		delete(x.byID, s.ID)
		delete(x.byDom, s.Domain)
		if werr := x.save(); werr != nil {
			x.a.log().Error("site index: save", "err", werr)
		}
	}
	switch {
	case method == agentrpc.DeleteSite.Name() && err == nil:
		for _, s := range sites {
			forget(s)
		}
	case createMethods[method] && err != nil:
		for _, s := range sites {
			if _, lerr := os.Lstat(s.RootPath); errors.Is(lerr, fs.ErrNotExist) {
				forget(s)
			}
		}
	}
}

// treeIs reports whether s's root is a real directory whose group is the
// site user's: the tree the agent made for that user.
func (x *siteIndex) treeIs(s state.Site) bool {
	fi, err := os.Lstat(s.RootPath)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	o, err := x.a.siteOwner(s.SystemUser)
	return err == nil && int(st.Gid) == o.GID
}

// put records id -> domain and saves.
func (x *siteIndex) put(id int64, domain string) error {
	x.byID[id], x.byDom[domain] = domain, id
	if err := x.save(); err != nil {
		delete(x.byID, id)
		delete(x.byDom, domain)
		return fmt.Errorf("site index: %w", err)
	}
	return nil
}

// load reads the index once; a missing file is built from the site trees
// (the upgrade from an agent without one) and saved.
func (x *siteIndex) load() error {
	if x.loaded {
		return nil
	}
	x.byID, x.byDom = map[int64]string{}, map[string]int64{}
	b, err := os.ReadFile(x.a.Paths.SiteIndex)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		x.rebuild()
		if err := x.save(); err != nil {
			return fmt.Errorf("site index: %w", err)
		}
	case err != nil:
		return fmt.Errorf("site index: %w", err)
	default:
		var f indexFile
		if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
			return fmt.Errorf("site index %s is unreadable; move it away to rebuild it from the site trees", x.a.Paths.SiteIndex)
		}
		for k, d := range f.Sites {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil || site.ValidateDomain(d) != nil {
				continue
			}
			x.byID[id], x.byDom[d] = d, id
		}
	}
	x.loaded = true
	return nil
}

// rebuild fills the index from the trees under SitesRoot: a directory named
// like a domain whose group is a site user's. Two trees of one user are
// both left out (and logged): neither can be trusted.
func (x *siteIndex) rebuild() {
	ents, err := os.ReadDir(x.a.Paths.SitesRoot)
	if err != nil {
		x.a.log().Warn("site index: no site trees to rebuild from", "err", err)
		return
	}
	seen := map[int64][]string{}
	for _, e := range ents {
		d := e.Name()
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 || site.ValidateDomain(d) != nil {
			continue
		}
		fi, err := os.Lstat(filepath.Join(x.a.Paths.SitesRoot, d))
		if err != nil || !fi.IsDir() {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		g, err := x.a.groupName(int(st.Gid))
		if err != nil {
			continue
		}
		if id, ok := idOf(g); ok {
			seen[id] = append(seen[id], d)
		}
	}
	for id, doms := range seen {
		if len(doms) != 1 {
			x.a.log().Error("site index: several trees belong to one site user; none recorded", "user", userOf(id), "trees", doms)
			continue
		}
		x.byID[id], x.byDom[doms[0]] = doms[0], id
	}
	x.a.log().Info("site index built from the site trees", "sites", len(x.byID))
}

// save writes the index atomically, root's, 0600.
func (x *siteIndex) save() error {
	f := indexFile{Version: 1, Sites: map[string]string{}}
	ids := make([]int64, 0, len(x.byID))
	for id := range x.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		f.Sites[strconv.FormatInt(id, 10)] = x.byID[id]
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(x.a.Paths.SiteIndex)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sites-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), x.a.Paths.SiteIndex)
}

// groupName resolves a gid; tests set LookupGroup.
func (a *Agent) groupName(gid int) (string, error) {
	if a.LookupGroup != nil {
		return a.LookupGroup(gid)
	}
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return "", err
	}
	return g.Name, nil
}
