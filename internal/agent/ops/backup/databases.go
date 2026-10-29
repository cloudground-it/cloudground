package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site has zero or more databases (Fase 9b): a backup dumps each into a
// snapshot of its own, tagged with the database's name (dbname:<name>), and
// the file snapshot names every one in a db:<id> tag (SPEC §4). A database
// snapshot from before, without a name, is the site's primary database.

// tagDBName names the database a database snapshot holds; tagTables how
// many tables (and views) it had when it was dumped.
const (
	tagDBName = "dbname:"
	tagTables = "tables:"
)

// dbPart is one database of a backup: its snapshot, the database's name and
// its table count at the dump (-1: not recorded, a backup from before).
type dbPart struct {
	ID     string
	Name   string
	Tables int
}

// siteDatabases lists the databases the agent records for the site: what a
// backup dumps and a restore may replace.
func (o *Ops) siteDatabases(ctx context.Context, s state.Site) ([]string, error) {
	return o.DB.BackedUpDatabases(ctx, s)
}

// databaseParts reads the database snapshots the file snapshot names
// (resolved by l: by ID, or by the ID they had before a copy, SPEC §5),
// each required to be one of the site's own made on this server (SR-T6).
// A backup from before R-0.2 names none.
func (o *Ops) databaseParts(ctx context.Context, l *siteSnaps, files host.Snapshot) ([]dbPart, error) {
	s := l.site
	var parts []dbPart
	for _, t := range files.Tags {
		dbID, ok := strings.CutPrefix(t, tagDB)
		if !ok {
			continue
		}
		if !ValidSnapshotID(dbID) {
			return nil, fmt.Errorf("snapshot %s names an invalid database snapshot", files.ID)
		}
		snap, err := l.find(ctx, dbID)
		if err != nil {
			return nil, err
		}
		if !ValidSnapshotID(snap.ID) {
			return nil, fmt.Errorf("the repository lists an invalid snapshot id")
		}
		if o.ours(snap, s, l.foreign) != "" || !slices.Contains(snap.Tags, tagDatabase) || snap.Hostname != files.Hostname {
			return nil, agentrpc.Invalid("snapshot %s is not a database backup of %s made on this server", dbID, s.Domain)
		}
		name, tables := "", -1
		for _, st := range snap.Tags {
			if v, ok := strings.CutPrefix(st, tagDBName); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(st, tagTables); ok {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 {
					tables = n
				}
			}
		}
		if name == "" {
			// One database per site, before: the primary.
			if !s.Config.Database.Enabled || s.Config.Database.External {
				return nil, agentrpc.Invalid("snapshot %s holds a database, and the site has no primary database to restore it into", dbID)
			}
			name = s.Config.Database.Name
		}
		if !db.ValidName(name) {
			return nil, agentrpc.Invalid("snapshot %s names an invalid database", dbID)
		}
		if slices.ContainsFunc(parts, func(p dbPart) bool { return p.Name == name }) {
			return nil, agentrpc.Invalid("snapshot %s holds the database %s twice", files.ID, name)
		}
		parts = append(parts, dbPart{ID: snap.ID, Name: name, Tables: tables})
	}
	return parts, nil
}

// renameParts imports each database of another server's backup into the
// database the panel made for it on the new site (SPEC §5: the primary is
// renamed with the site); without a foreign restore it changes nothing.
func renameParts(parts []dbPart, f *rpc.ForeignRestore) ([]dbPart, error) {
	if f == nil {
		return parts, nil
	}
	out := make([]dbPart, 0, len(parts))
	for _, p := range parts {
		if to, ok := f.Databases[p.Name]; ok {
			p.Name = to
		}
		if slices.ContainsFunc(out, func(q dbPart) bool { return q.Name == p.Name }) {
			return nil, agentrpc.Invalid("two databases of the backup would go into %s", p.Name)
		}
		out = append(out, p)
	}
	return out, nil
}

// dumpFile is a database's dump fetched into the work directory.
type dumpFile struct {
	Name   string // the database
	Path   string
	Tables int // its table count at the dump, -1 unknown
}

// fetchDumps writes each part's dump into a root-only file in dir.
func (o *Ops) fetchDumps(ctx context.Context, repo host.Repo, parts []dbPart, dir string) ([]dumpFile, error) {
	var out []dumpFile
	for _, p := range parts {
		path, err := o.fetchDump(ctx, repo, p.ID, filepath.Join(dir, p.Name+".sql"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		out = append(out, dumpFile{Name: p.Name, Path: path, Tables: p.Tables})
	}
	return out, nil
}

// restorePlan chooses which of a backup's databases a restore puts back:
// those asked for (each must be in the backup and still the site's), or
// every one the site still has. A database of the backup that the site no
// longer has is skipped, named in skipped.
func (o *Ops) restorePlan(ctx context.Context, s state.Site, parts []dbPart, asked []string) (keep []dbPart, skipped []string, err error) {
	for _, name := range asked {
		if !slices.ContainsFunc(parts, func(p dbPart) bool { return p.Name == name }) {
			return nil, nil, agentrpc.Invalid("the backup has no copy of the database %s", name)
		}
	}
	for _, p := range parts {
		if len(asked) > 0 && !slices.Contains(asked, p.Name) {
			continue
		}
		if _, err := o.DB.SiteDatabase(ctx, s, p.Name); err != nil {
			if len(asked) > 0 {
				return nil, nil, agentrpc.Invalid("%s is no longer a database of %s: create it again to restore it", p.Name, s.Domain)
			}
			skipped = append(skipped, p.Name)
			continue
		}
		keep = append(keep, p)
	}
	return keep, skipped, nil
}

// replaceAll replaces each database with its dump; a failure rolls back the
// ones already replaced. The caller commits or rolls back the result.
func (o *Ops) replaceAll(ctx context.Context, dumps []dumpFile) (replacements, error) {
	var done replacements
	for _, d := range dumps {
		tx, err := o.DB.Replace(ctx, d.Name, d.Path, db.Fresh)
		if err != nil {
			return nil, done.rollback(fmt.Errorf("%s: %w", d.Name, err))
		}
		done = append(done, tx)
	}
	return done, nil
}

// replacements are databases replaced together.
type replacements []*db.Replacement

// rollback puts every one back, the last first; it returns cause with any
// failure joined.
func (r replacements) rollback(cause error) error {
	for i := len(r) - 1; i >= 0; i-- {
		cause = r[i].Rollback(cause)
	}
	return cause
}

func (r replacements) commit() {
	for _, tx := range r {
		tx.Commit()
	}
}
