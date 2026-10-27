package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"golang.org/x/sys/unix"
)

// Dump streams database (or only tables of it) into w.
func (o *Ops) Dump(ctx context.Context, database string, w io.Writer, tables ...string) error {
	if pg, ok, err := o.pgOfDatabase(ctx, database); err != nil {
		return err
	} else if ok {
		if len(tables) > 0 {
			return agentrpc.Invalid("a PostgreSQL database is dumped whole")
		}
		if err := pg.Dump(ctx, database, w); err != nil {
			return fmt.Errorf("dump %s: %w", database, err)
		}
		return nil
	}
	m, err := o.maria(ctx, database)
	if err != nil {
		return err
	}
	if err := m.Dump(ctx, database, w, tables...); err != nil {
		return fmt.Errorf("dump %s: %w", database, err)
	}
	return nil
}

// DumpFile dumps database (or only tables of it) into a new file at path,
// readable by root only, and syncs it.
func (o *Ops) DumpFile(ctx context.Context, database, path string, tables ...string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := o.Dump(ctx, database, f, tables...); err != nil {
		return err
	}
	return f.Sync()
}

// ImportFile feeds the SQL file at path to database through the sandboxed
// client, as an account scoped to that database.
func (o *Ops) ImportFile(ctx context.Context, database, path string) error {
	if pg, ok, err := o.pgOfDatabase(ctx, database); err != nil {
		return err
	} else if ok {
		return o.importPG(ctx, pg, database, ownerRole(database), path)
	}
	return o.importFileMaria(ctx, database, path)
}

func (o *Ops) importFileMaria(ctx context.Context, database, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := o.maria(ctx, database)
	if err != nil {
		return err
	}
	if err := m.Import(ctx, database, f, o.importAs(ctx, database)); err != nil {
		return fmt.Errorf("import into %s: %w", database, err)
	}
	return nil
}

// ImportMode is how Replace treats what the database holds.
type ImportMode int

const (
	// Over imports on top of the current contents.
	Over ImportMode = iota
	// Fresh empties the database first: afterwards it holds the dump and
	// nothing else.
	Fresh
)

// Replacement is an imported database with its rollback point.
type Replacement struct {
	o        *Ops
	database string
	safety   string
}

// RollbackPoint is the file holding the contents from before the import.
func (r *Replacement) RollbackPoint() string { return r.safety }

// Replace saves database's current contents, then imports the dump at
// path. A failed import is rolled back before Replace returns its error.
// The caller ends a successful one with Commit, or Rollback when a later
// step fails.
func (o *Ops) Replace(ctx context.Context, database, dump string, mode ImportMode) (*Replacement, error) {
	if !o.known(ctx, database) || scratchNameRe.MatchString(database) {
		return nil, agentrpc.Invalid("invalid database name")
	}
	dir := filepath.Join(o.WorkDir, "rollback")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, database+"-*.sql")
	if err != nil {
		return nil, err
	}
	f.Close()
	r := &Replacement{o: o, database: database, safety: f.Name()}
	if err := o.DumpFile(ctx, database, r.safety); err != nil {
		os.Remove(r.safety)
		return nil, fmt.Errorf("create the database's rollback point: %w", err)
	}
	if mode == Fresh {
		if err := o.empty(ctx, database); err != nil {
			return nil, r.Rollback(err)
		}
	}
	if err := o.ImportFile(ctx, database, dump); err != nil {
		return nil, r.Rollback(err)
	}
	return r, nil
}

// Rollback puts the saved contents back, exactly: the database is emptied
// first, so tables the import created do not stay. The rollback point is
// deleted only if that works; otherwise the error says where it is. It
// returns cause, with the rollback's failure joined.
func (r *Replacement) Rollback(cause error) error {
	// Its own deadline: a request that timed out must still be rolled back.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	err := r.o.empty(ctx, r.database)
	if err == nil {
		err = r.o.ImportFile(ctx, r.database, r.safety)
	}
	if err != nil {
		return fmt.Errorf("%w; rolling the database back ALSO failed (%v): the original contents are kept at %s", cause, err, r.safety)
	}
	os.Remove(r.safety)
	return cause
}

// Commit discards the rollback point.
func (r *Replacement) Commit() { os.Remove(r.safety) }

// charsetRe is a character set or collation name as information_schema
// reports one.
var charsetRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// charset is database's default character set and collation, utf8mb4 when
// it cannot be read as names.
func (o *Ops) charset(ctx context.Context, database string) (string, string, error) {
	cs, coll := "utf8mb4", "utf8mb4_unicode_ci"
	m, err := o.maria(ctx, database)
	if err != nil {
		return "", "", err
	}
	got, err := m.QueryStrings(ctx,
		"SELECT CONCAT(DEFAULT_CHARACTER_SET_NAME, ' ', DEFAULT_COLLATION_NAME) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", database)
	if err != nil {
		return "", "", fmt.Errorf("read the character set of %s: %w", database, err)
	}
	if len(got) == 1 {
		if f := strings.Fields(got[0]); len(f) == 2 && charsetRe.MatchString(f[0]) && charsetRe.MatchString(f[1]) {
			cs, coll = f[0], f[1]
		}
	}
	return cs, coll, nil
}

// empty drops and recreates database with the character set and collation
// it had (SPEC §6). Grants are on the name and survive.
func (o *Ops) empty(ctx context.Context, database string) error {
	if !ValidName(database) {
		return agentrpc.Invalid("invalid database name")
	}
	if pg, ok, err := o.pgOfDatabase(ctx, database); err != nil {
		return err
	} else if ok {
		return o.emptyPG(ctx, pg, database)
	}
	cs, coll, err := o.charset(ctx, database)
	if err != nil {
		return err
	}
	m, err := o.maria(ctx, database)
	if err != nil {
		return err
	}
	if err := m.Exec(ctx, "DROP DATABASE IF EXISTS `"+database+"`"); err != nil {
		return fmt.Errorf("empty %s: %w", database, err)
	}
	if err := m.Exec(ctx, "CREATE DATABASE `"+database+"` CHARACTER SET "+cs+" COLLATE "+coll); err != nil {
		return fmt.Errorf("recreate %s: %w", database, err)
	}
	return nil
}

// exportsDir is where exports go in a site's tree.
const exportsDir = "shared/exports"

// Export dumps the site's own database into a new file in shared/exports,
// created through the site's root (SPEC §4).
func (o *Ops) Export(ctx context.Context, p rpc.DBExportParams) (rpc.DBExportResult, error) {
	database, err := o.SiteDatabase(ctx, p.Site, p.Database)
	if err != nil {
		return rpc.DBExportResult{}, err
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBExportResult{}, err
	}
	defer r.Close()
	if fi, err := r.Lstat(exportsDir); errors.Is(err, fs.ErrNotExist) {
		if err := r.Mkdir(exportsDir, 0o750); err != nil {
			return rpc.DBExportResult{}, err
		}
	} else if err != nil {
		return rpc.DBExportResult{}, err
	} else if !fi.IsDir() {
		return rpc.DBExportResult{}, agentrpc.Invalid("%s is not a real directory; refusing to follow it", exportsDir)
	}
	f, rel, err := createExport(r, database, time.Now())
	if err != nil {
		return rpc.DBExportResult{}, err
	}
	if err := o.Dump(ctx, database, f); err != nil {
		f.Close()
		_ = r.Remove(rel)
		return rpc.DBExportResult{}, err
	}
	fi, err := f.Stat()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = r.Remove(rel)
		return rpc.DBExportResult{}, err
	}
	return rpc.DBExportResult{Path: path.Join(p.Site.RootPath, rel), SizeBytes: fi.Size()}, nil
}

// createExport makes a new export file named after database and now, with
// a counter when that name is taken.
func createExport(r *sitefs.Root, database string, now time.Time) (*os.File, string, error) {
	base := database + "-" + now.UTC().Format("20060102-150405")
	for i := 1; i <= 100; i++ {
		name := base + ".sql"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.sql", base, i)
		}
		rel := exportsDir + "/" + name
		f, err := r.Create(rel, 0o640)
		if err == nil {
			return f, rel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fileError(err)
		}
	}
	return nil, "", agentrpc.Conflict("too many exports named %s", base)
}

// Import replaces the site's own database from a .sql file in its tree,
// copied first into root's work directory (SPEC §5).
func (o *Ops) Import(ctx context.Context, p rpc.DBImportParams) (rpc.DBImportResult, error) {
	database, err := o.SiteDatabase(ctx, p.Site, p.Database)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	if !strings.HasSuffix(strings.ToLower(strings.TrimSpace(p.RelPath)), ".sql") {
		return rpc.DBImportResult{}, agentrpc.Invalid("the import file must use the .sql extension")
	}
	work, err := os.MkdirTemp(o.WorkDir, "db-import-")
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	defer os.RemoveAll(work)
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	staged, size, err := stage(r, p.RelPath, work)
	r.Close()
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	warnings, err := fileStatementWarnings(staged)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	for _, w := range warnings {
		o.log().Warn("database import: "+w, "site", p.Site.Domain, "file", p.RelPath)
	}
	tx, err := o.Replace(ctx, database, staged, Over)
	if err != nil {
		if len(warnings) > 0 {
			// The person is told why, not only that it failed.
			return rpc.DBImportResult{}, agentrpc.Wrap(agentrpc.CodeInvalid, "the database import failed: "+strings.Join(warnings, "; "), err)
		}
		return rpc.DBImportResult{}, fmt.Errorf("the database import failed: %w", err)
	}
	tx.Commit()
	return rpc.DBImportResult{Imported: p.RelPath, SizeBytes: size, Warnings: warnings}, nil
}

// stage copies the regular file rel of a site's tree, never following a
// symlink at its name, into dir.
func stage(r *sitefs.Root, rel, dir string) (string, int64, error) {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	in, err := r.OpenFile(rel, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, fileError(err)
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return "", 0, agentrpc.Invalid("%s must be a non-empty regular file", rel)
	}
	out, err := os.CreateTemp(dir, "input-*.sql")
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out.Name())
		return "", 0, err
	}
	return out.Name(), n, nil
}

// fileError classifies a site-tree error for the browser.
func fileError(err error) error {
	switch {
	case errors.Is(err, sitefs.ErrEscape):
		return agentrpc.Invalid("path escapes the site root")
	case errors.Is(err, sitefs.ErrSymlink):
		return agentrpc.Invalid("the path is a symlink; refusing to follow it")
	case errors.Is(err, sitefs.ErrNotRegular):
		return agentrpc.Invalid("not a regular file")
	case errors.Is(err, fs.ErrNotExist):
		return agentrpc.NotFound("no such file")
	}
	return err
}
