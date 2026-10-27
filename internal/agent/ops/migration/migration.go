// Package migration imports a site brought from another host: an archive of
// its tree and, optionally, a dump of its database. SPEC.md in this
// directory is the contract.
package migration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"golang.org/x/sys/unix"
)

// Releases builds and promotes releases (ops/release).
type Releases interface {
	Build(ctx context.Context, p rpc.DeployParams, roots []string) (rpc.DeployResult, error)
	Promote(ctx context.Context, p rpc.ReleaseParams) (map[string]string, error)
}

// WordPress rewrites URLs, reads and sets the site's table prefix, and makes
// out-of-band changes visible (ops/wp).
type WordPress interface {
	SearchReplace(ctx context.Context, s state.Site, from, to string) error
	Refresh(ctx context.Context, s state.Site) error
	TablePrefix(ctx context.Context, s state.Site) (string, error)
	SetTablePrefix(ctx context.Context, s state.Site, prefix string) error
}

// Staged is a dump imported beside the site's database, put in its place by
// renames and taken back the same way (ops/db's *Staged; SPEC §4.3).
type Staged interface {
	Tables() []string
	Value(ctx context.Context, table, column, key, value string) (string, error)
	Swap(ctx context.Context) error
	Retire(ctx context.Context) error
	Unretire(ctx context.Context) error
	Unswap(ctx context.Context) error
	Commit(ctx context.Context)
	Hold() string
}

// Ops holds what the handler uses.
type Ops struct {
	// WorkDir is the agent's root-only scratch directory.
	WorkDir string
	// Open opens a site's tree; what it creates belongs to the site user.
	Open      func(state.Site) (*sitefs.Root, error)
	Releases  Releases
	WordPress WordPress
	// StageDB imports the dump at path into a scratch database, to replace
	// database with.
	// database is one of s's ("" its primary), as the agent records them.
	StageDB func(ctx context.Context, s state.Site, database, dump string) (Staged, error)
	// Limits are DefaultLimits when zero.
	Limits Limits
	// FreeSpace reports the bytes free for root in dir (tests replace it).
	FreeSpace func(dir string) (int64, error)
	Log       *slog.Logger
}

// Register wires the handler into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.ImportMigration, o.Import)
	agentrpc.Handle(s, agentrpc.PullEstimate, o.PullEstimate)
	agentrpc.Handle(s, agentrpc.PullMigration, o.PullMigration)
	agentrpc.Handle(s, agentrpc.PullCleanup, o.PullCleanup)
}

// limits are the migration's limits: o.Limits (DefaultLimits when zero),
// with the expansion limit the request sets, if it sets one (SPEC §3).
func (o *Ops) limits(p rpc.MigrationParams) Limits {
	l := o.Limits
	if l == (Limits{}) {
		l = DefaultLimits
	}
	if p.MaxRatio > 0 {
		l.MaxRatio = p.MaxRatio
	}
	return l
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) freeSpace(dir string) (int64, error) {
	if o.FreeSpace != nil {
		return o.FreeSpace(dir)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

const (
	uploadsLink   = "wp-content/uploads"
	sharedUploads = "shared/uploads"
	stagePrefix   = ".cg-migrate-"
	stagedUploads = "uploads"
)

func isWordPress(t state.SiteType) bool {
	return t == state.SiteWordPress || t == state.SiteWooCommerce
}

// siteRel turns a path from the API into a site-relative one.
func siteRel(p string) string {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == "" {
		return "."
	}
	return rel
}

func validate(p rpc.MigrationParams) error {
	if !site.ValidReleaseID(p.ReleaseID) {
		return agentrpc.Invalid("invalid migration release id")
	}
	if p.OldDomain != "" {
		if err := nginx.ValidateDomain(p.OldDomain); err != nil {
			return agentrpc.Invalid("invalid old domain: %s", err)
		}
	}
	a := strings.ToLower(p.Archive)
	if !strings.HasSuffix(a, ".tar.gz") && !strings.HasSuffix(a, ".tgz") {
		return agentrpc.Invalid("the migration archive must be a .tar.gz or .tgz file")
	}
	if p.SQL != "" && !strings.HasSuffix(strings.ToLower(p.SQL), ".sql") {
		return agentrpc.Invalid("the database dump must be a .sql file")
	}
	localPrimary := p.Site.Config.Database.Enabled && !p.Site.Config.Database.External
	if p.SQL != "" && p.Database == "" && !localPrimary {
		return agentrpc.Invalid("site has no local managed database")
	}
	// A WordPress dump goes into the database wp-config.php names: its URLs
	// and table prefix are set there.
	if p.SQL != "" && isWordPress(p.Site.Type) && p.Database != "" && (!localPrimary || p.Database != p.Site.Config.Database.Name) {
		return agentrpc.Invalid("a WordPress site's dump goes into its primary database")
	}
	if p.MaxRatio < 0 || p.MaxRatio > MaxRatioCeiling {
		return agentrpc.Invalid("the expansion limit must be between 1 and %d times the archive's size", MaxRatioCeiling)
	}
	return nil
}

// run is one migration in progress: what has changed so far, so a failure
// can undo exactly that (SPEC §5).
type run struct {
	o     *Ops
	p     rpc.MigrationParams
	r     *sitefs.Root
	oldID string

	stage string // root's directory in the site root holding the uploads
	built bool
	// The database (SPEC §4.3): the staged dump and how far it has moved.
	db        Staged
	dbSwapped bool
	dbRetired bool
	prefix    string // the prefix the site runs on after ("" without one)
	oldPrefix string // the site's $table_prefix before
	// prefixMoved: wp-config.php was (or may have been) set to prefix.
	prefixMoved bool
	maintenance []string // the maintenance files in place (SPEC §4.4)
	swapped     bool     // shared/uploads exchanged with the staged uploads
	moved       bool     // the staged uploads moved to a site that had none
	promoted    bool
}

// Import unpacks the archive into a new release, imports the dump, swaps
// the uploads and makes it all live, or none of it (SPEC §4, §5). The server
// holds the site's lock (LockLiveSite).
func (o *Ops) Import(ctx context.Context, p rpc.MigrationParams) (rpc.MigrationResult, error) {
	p.OldDomain = strings.ToLower(strings.TrimSpace(p.OldDomain))
	if err := validate(p); err != nil {
		return rpc.MigrationResult{}, err
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.MigrationResult{}, err
	}
	defer r.Close()
	t, err := r.Readlink(release.CurrentLink)
	if err != nil {
		return rpc.MigrationResult{}, fmt.Errorf("read the current release: %w", err)
	}
	oldID, ok := release.ParseLink(t, p.Site.RootPath)
	if !ok {
		return rpc.MigrationResult{}, fmt.Errorf("the current release points outside the site's releases")
	}
	// The prefix to come back to on a rollback is known, and valid, before
	// anything changes (SPEC §2).
	wordpressDB := p.SQL != "" && isWordPress(p.Site.Type)
	var oldPrefix string
	if wordpressDB {
		if oldPrefix, err = o.WordPress.TablePrefix(ctx, p.Site); err != nil {
			return rpc.MigrationResult{}, agentrpc.Wrap(agentrpc.CodeInvalid, "cannot read the site's table prefix from its wp-config.php", err)
		}
	}

	work, err := os.MkdirTemp(o.WorkDir, "migration-")
	if err != nil {
		return rpc.MigrationResult{}, err
	}
	defer os.RemoveAll(work)
	lim := o.limits(p)
	archiveRel, sqlRel := siteRel(p.Archive), siteRel(p.SQL)
	archive, size, err := stage(r, archiveRel, work, "archive.tar.gz", lim.MaxBytes)
	if err != nil {
		return rpc.MigrationResult{}, fmt.Errorf("read the migration archive: %w", err)
	}
	defer archive.Close()
	var dump string
	var tables []string
	if p.SQL != "" {
		f, _, err := stage(r, sqlRel, work, "dump.sql", lim.MaxBytes)
		if err != nil {
			return rpc.MigrationResult{}, fmt.Errorf("read the database dump: %w", err)
		}
		dump = f.Name()
		f.Close()
		if wordpressDB {
			if tables, err = dumpTables(dump); err != nil {
				return rpc.MigrationResult{}, err
			}
			// Refused here already when no choice could be right; the
			// archive's wp-config.php may still pick among several.
			if len(wordPressPrefixes(tables)) == 0 {
				_, err := WordPressPrefix(tables, "")
				return rpc.MigrationResult{}, err
			}
		}
	}

	// Extraction.
	avail, err := o.freeSpace(work)
	if err != nil {
		return rpc.MigrationResult{}, err
	}
	treeDir := filepath.Join(work, "tree")
	if err := os.Mkdir(treeDir, 0o700); err != nil {
		return rpc.MigrationResult{}, err
	}
	tree, err := sitefs.Open(treeDir, sitefs.Owner{})
	if err != nil {
		return rpc.MigrationResult{}, err
	}
	defer tree.Close()
	stats, srcRel, err := Unpack(archive, tree, lim, lim.Budget(size, avail))
	archive.Close()
	if errors.Is(err, ErrTooLarge) {
		err = lim.TooLarge(size, avail)
	}
	if err != nil {
		return rpc.MigrationResult{}, err
	}
	src, err := tree.Sub(srcRel)
	if err != nil {
		return rpc.MigrationResult{}, err
	}
	defer src.Close()

	// A WordPress site runs on the dump's tables only if wp-config.php names
	// their prefix (SPEC §4.3); the archive's own wp-config.php, read before
	// the build replaces it with the site's link, says which.
	var prefix string
	if wordpressDB {
		if prefix, err = WordPressPrefix(tables, archivePrefix(src)); err != nil {
			return rpc.MigrationResult{}, err
		}
	}

	m := &run{o: o, p: p, r: r, oldID: oldID, prefix: prefix, oldPrefix: oldPrefix}
	defer m.cleanup()
	if err := m.stageUploads(src); err != nil {
		return rpc.MigrationResult{}, err
	}
	if _, err := o.Releases.Build(ctx, rpc.DeployParams{Site: p.Site, SourceDir: filepath.Join(treeDir, srcRel), ReleaseID: p.ReleaseID}, []string{work}); err != nil {
		return rpc.MigrationResult{}, err
	}
	m.built = true
	if err := m.activate(ctx, dump); err != nil {
		return rpc.MigrationResult{}, m.rollback(ctx, err)
	}

	// Done: the inputs have served their purpose (SPEC §4.8). Removed as
	// links if they have become links.
	_ = r.Remove(archiveRel)
	if p.SQL != "" {
		_ = r.Remove(sqlRel)
	}
	return rpc.MigrationResult{ReleaseID: p.ReleaseID, Files: stats.Files, Bytes: stats.Bytes, Skipped: stats.Skipped}, nil
}

// stage copies the regular file rel of the site's tree, never following a
// symlink at its name, into dir as name, and returns the copy, open for
// reading at its start, and its size.
func stage(r *sitefs.Root, rel, dir, name string, limit int64) (*os.File, int64, error) {
	in, err := r.OpenFile(rel, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fileError(err)
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil, 0, agentrpc.Invalid("%s must be a non-empty regular file", rel)
	}
	out, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if err == nil && n > limit {
		err = agentrpc.Invalid("%s is larger than %d GiB, the limit", rel, limit>>30)
	}
	if err == nil {
		_, err = out.Seek(0, io.SeekStart)
	}
	if err != nil {
		out.Close()
		return nil, 0, err
	}
	return out, n, nil
}

// fileError classifies a refusal of the site's tree for the panel.
func fileError(err error) error {
	switch {
	case errors.Is(err, sitefs.ErrEscape):
		return agentrpc.Invalid("path escapes the site root")
	case errors.Is(err, sitefs.ErrSymlink):
		return agentrpc.Invalid("the path is a symlink; refusing to follow it")
	case errors.Is(err, sitefs.ErrNotRegular):
		return agentrpc.Invalid("not a regular file")
	case errors.Is(err, fs.ErrNotExist):
		return agentrpc.Invalid("the file does not exist")
	}
	return err
}

// stageUploads copies the archive's wp-content/uploads, when it is a real
// directory, into a directory of root's in the site root, and takes it out
// of the source (the build would only replace it with the link).
func (m *run) stageUploads(src *sitefs.Root) error {
	fi, err := src.Lstat(uploadsLink)
	if err != nil || !fi.IsDir() {
		return nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	dir := stagePrefix + hex.EncodeToString(b[:])
	if err := m.r.WithOwner(sitefs.Owner{}).Mkdir(dir, 0o700); err != nil {
		return err
	}
	m.stage = dir
	if err := sitefs.CopyTree(src, uploadsLink, m.r, dir+"/"+stagedUploads); err != nil {
		return fmt.Errorf("stage the uploads: %w", err)
	}
	return src.RemoveAll(uploadsLink)
}

// activate stages the dump, puts the site in maintenance, swaps the
// database in, swaps the uploads, promotes the release and rewrites the
// URLs, recording each step for the rollback (SPEC §4).
func (m *run) activate(ctx context.Context, dump string) error {
	p, o := m.p, m.o
	if dump != "" {
		// The long part, while the site still serves from its own tables.
		st, err := o.StageDB(ctx, p.Site, p.Database, dump)
		if err != nil {
			return fmt.Errorf("the database import failed: %w", err)
		}
		m.db = st
		if m.prefix != "" {
			if err := checkStaged(ctx, st, m.prefix); err != nil {
				return err
			}
		}
	}
	if isWordPress(p.Site.Type) {
		if err := m.maintenanceOn(); err != nil {
			return err
		}
	}
	if m.db != nil {
		if err := m.db.Swap(ctx); err != nil {
			return err
		}
		m.dbSwapped = true
		if m.prefix != "" && m.prefix != m.oldPrefix {
			m.prefixMoved = true // a failed set may have written it
			if err := o.WordPress.SetTablePrefix(ctx, p.Site, m.prefix); err != nil {
				return fmt.Errorf("set the table prefix to the dump's %s: %w", m.prefix, err)
			}
		}
		if err := m.db.Retire(ctx); err != nil {
			return err
		}
		m.dbRetired = true
	}
	if m.stage != "" {
		staged := m.stage + "/" + stagedUploads
		err := m.r.Rename(staged, sharedUploads, sitefs.Exchange)
		switch {
		case err == nil:
			m.swapped = true
		case errors.Is(err, fs.ErrNotExist):
			if err := m.r.Rename(staged, sharedUploads, sitefs.NoReplace); err != nil {
				return fmt.Errorf("activate the migrated uploads: %w", err)
			}
			m.moved = true
		default:
			return fmt.Errorf("activate the migrated uploads: %w", err)
		}
	}
	m.promoted = true // a failed promote may have swapped current already
	if _, err := o.Releases.Promote(ctx, rpc.ReleaseParams{Site: p.Site, ReleaseID: p.ReleaseID}); err != nil {
		return err
	}
	// A site moved under its own domain (the usual case) has no URLs to
	// rewrite; ops/wp refuses a no-op search-replace.
	if p.OldDomain != "" && p.OldDomain != strings.ToLower(p.Site.Domain) && isWordPress(p.Site.Type) {
		if err := o.WordPress.SearchReplace(ctx, p.Site, "https://"+p.OldDomain, "https://"+p.Site.Domain); err != nil {
			return fmt.Errorf("the URL rewrite failed: %w", err)
		}
	}
	if m.db != nil {
		m.db.Commit(ctx)
		m.db = nil
	}
	m.built = false // live now: cleanup keeps it
	// The import and the rewrite ran outside FPM, whose APCu still holds
	// the old options and rewrite rules.
	if isWordPress(p.Site.Type) {
		if err := o.WordPress.Refresh(ctx, p.Site); err != nil {
			o.log().Warn("refreshing the migrated site's state failed", "site", p.Site.Domain, "err", err)
		}
	}
	m.maintenanceOff()
	return nil
}

// checkStaged verifies, before anything live moves, that the imported dump
// is a WordPress installation on prefix: its three core tables, and a
// site URL in its options (what WordPress reads to decide it is
// installed).
func checkStaged(ctx context.Context, st Staged, prefix string) error {
	have := st.Tables()
	for _, c := range coreTables {
		if !slices.Contains(have, prefix+c) {
			return agentrpc.Invalid("the imported dump has no %s%s table", prefix, c)
		}
	}
	url, err := st.Value(ctx, prefix+"options", "option_value", "option_name", "siteurl")
	if err != nil {
		return fmt.Errorf("read the dump's site URL: %w", err)
	}
	if strings.TrimSpace(url) == "" {
		return agentrpc.Invalid("the dump's %soptions has no siteurl: it is not an installed WordPress", prefix)
	}
	return nil
}

// rollback undoes what activate did, in reverse, and returns cause with
// the rollback's own failures joined (SPEC §5).
func (m *run) rollback(ctx context.Context, cause error) error {
	ctx = context.WithoutCancel(ctx)
	r, p := m.r, m.p
	if m.promoted {
		if err := r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID}).Symlink(release.Dir+"/"+m.oldID, release.CurrentLink); err != nil {
			cause = errors.Join(cause, fmt.Errorf("restore the current release: %w", err))
		}
	}
	staged := m.stage + "/" + stagedUploads
	switch {
	case m.swapped:
		if err := r.Rename(staged, sharedUploads, sitefs.Exchange); err != nil {
			cause = errors.Join(cause, fmt.Errorf("restore the previous uploads: %w", err))
		}
	case m.moved:
		if err := r.Rename(sharedUploads, staged, sitefs.NoReplace); err != nil {
			cause = errors.Join(cause, fmt.Errorf("take the migrated uploads away: %w", err))
		}
	}
	if m.db != nil {
		cause = m.rollbackDB(ctx, cause)
	}
	// Code, uploads or rows may have changed under FPM; Refresh does
	// nothing for a site without PHP.
	if err := m.o.WordPress.Refresh(ctx, p.Site); err != nil {
		m.o.log().Warn("refreshing the site after a failed migration failed", "site", p.Site.Domain, "err", err)
	}
	m.maintenanceOff()
	return cause
}

// rollbackDB undoes the database steps in reverse, keeping at every moment
// the tables the configured prefix names in the live database: the retired
// tables come back, then the prefix, then one statement exchanges the
// swapped ones back. A step that fails stops the undo there, with the
// database holding the replaced tables named in the error.
func (m *run) rollbackDB(ctx context.Context, cause error) error {
	st, p := m.db, m.p
	m.db = nil
	kept := func(err error) error {
		return errors.Join(cause, fmt.Errorf("%w; the database ALSO could not be put back: its previous tables are in the database %s", err, st.Hold()))
	}
	if m.dbRetired {
		if err := st.Unretire(ctx); err != nil {
			return kept(err)
		}
	}
	if m.prefixMoved {
		if err := m.o.WordPress.SetTablePrefix(ctx, p.Site, m.oldPrefix); err != nil {
			return kept(fmt.Errorf("restore the table prefix %s: %w", m.oldPrefix, err))
		}
	}
	if err := st.Unswap(ctx); err != nil {
		if m.dbSwapped {
			return kept(err)
		}
		return errors.Join(cause, err)
	}
	return cause
}

// cleanup removes what a migration leaves behind: the staging directory
// (the previous uploads, after a success) and a release that did not go
// live.
func (m *run) cleanup() {
	m.maintenanceOff()
	if m.built {
		if err := m.r.RemoveAll(release.Dir + "/" + m.p.ReleaseID); err != nil {
			m.o.log().Warn("removing the release of a failed migration failed", "site", m.p.Site.Domain, "err", err)
		}
	}
	if m.stage != "" {
		_ = m.r.RemoveAll(m.stage)
	}
}
