package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

var ctx = context.Background()

const releaseID = "20261009-120000.000-abcdef"

// wpDump is a dump of a WordPress installation with table prefix p, as
// mariadb-dump writes it (an extended INSERT on one long line).
func wpDump(p string) string {
	var b strings.Builder
	b.WriteString("-- MariaDB dump 10.19\n")
	for _, t := range []string{"options", "posts", "users", "postmeta"} {
		fmt.Fprintf(&b, "DROP TABLE IF EXISTS `%s%s`;\nCREATE TABLE `%s%s` (\n  `id` bigint(20) NOT NULL\n);\n", p, t, p, t)
		fmt.Fprintf(&b, "INSERT INTO `%s%s` VALUES (1)%s;\n", p, t, strings.Repeat(",(1)", 20_000))
	}
	return b.String()
}

// newRows is the dump the fixture migrates: the old host's site, prefix wp_.
var newRows = wpDump("wp_")

type fakeWP struct {
	replaced  [][2]string
	refreshes int
	failSR    error
	prefix    string   // $table_prefix in wp-config.php
	prefixes  []string // each SetTablePrefix
	failSet   error
	failGet   error
	db        *fakeDB
	// sawMaintenance: both releases held the maintenance file at the URL
	// rewrite.
	sawMaintenance bool
}

func (w *fakeWP) TablePrefix(context.Context, state.Site) (string, error) {
	return w.prefix, w.failGet
}

func (w *fakeWP) SetTablePrefix(_ context.Context, _ state.Site, p string) error {
	w.prefixes = append(w.prefixes, p)
	if err := w.failSet; err != nil {
		w.failSet = nil // the first set fails; the rollback's works
		return err
	}
	w.prefix = p
	if w.db != nil {
		w.db.check("prefix " + p)
	}
	return nil
}

func (w *fakeWP) SearchReplace(_ context.Context, s state.Site, from, to string) error {
	w.replaced = append(w.replaced, [2]string{from, to})
	// Both releases carry the maintenance file while the switch runs.
	w.sawMaintenance = true
	for _, id := range []string{"initial", releaseID} {
		b, err := os.ReadFile(filepath.Join(s.RootPath, "releases", id, ".maintenance"))
		w.sawMaintenance = w.sawMaintenance && err == nil && string(b) == string(maintenanceBody)
	}
	return w.failSR
}

func (w *fakeWP) Refresh(context.Context, state.Site) error { w.refreshes++; return nil }

// fakeDB is the site's database as a list of tables, with a staged dump
// beside it (it is its own Staged). Every move checks what WordPress would
// see: the configured prefix's options table must be in the live database
// at every moment, or the site would offer its installer (broken).
type fakeDB struct {
	live, orig []string
	dump       []string
	imported   string
	siteurl    string
	failImport error
	failSwap   error
	failRetire error
	staged     bool
	committed  bool
	rolledBack bool
	retired    []string
	wp         *fakeWP
	broken     []string
}

var oldTables = []string{"wp_options", "wp_posts", "wp_users", "wp_plugin_log"}

func (d *fakeDB) check(when string) {
	if !slices.Contains(d.live, d.wp.prefix+"options") {
		d.broken = append(d.broken, when)
	}
}

func (d *fakeDB) stage(_ context.Context, s state.Site, database, dump string) (Staged, error) {
	if database == "" {
		database = s.Config.Database.Name // the agent's resolution of the primary
	}
	if database != "site_3" {
		return nil, errors.New("wrong database " + database)
	}
	b, err := os.ReadFile(dump)
	if err != nil {
		return nil, err
	}
	if d.failImport != nil {
		return nil, d.failImport
	}
	if d.dump, err = DumpTables(strings.NewReader(string(b))); err != nil {
		return nil, err
	}
	d.imported, d.staged, d.orig = string(b), true, slices.Clone(d.live)
	return d, nil
}

func (d *fakeDB) Tables() []string { return slices.Clone(d.dump) }
func (d *fakeDB) Hold() string     { return "cgh_0123456789abcdef" }
func (d *fakeDB) Value(_ context.Context, table, _, _, value string) (string, error) {
	if value == "siteurl" && slices.Contains(d.dump, table) {
		return d.siteurl, nil
	}
	return "", nil
}

func (d *fakeDB) Swap(context.Context) error {
	if d.failSwap != nil {
		return d.failSwap
	}
	for _, t := range d.dump {
		if !slices.Contains(d.live, t) {
			d.live = append(d.live, t)
		}
	}
	d.check("swap")
	return nil
}

func (d *fakeDB) Retire(context.Context) error {
	if d.failRetire != nil {
		return d.failRetire
	}
	for _, t := range d.live {
		if !slices.Contains(d.dump, t) {
			d.retired = append(d.retired, t)
		}
	}
	d.live = slices.Clone(d.dump)
	d.check("retire")
	return nil
}

func (d *fakeDB) Unretire(context.Context) error {
	d.live = append(d.live, d.retired...)
	d.retired = nil
	d.check("unretire")
	return nil
}

func (d *fakeDB) Unswap(context.Context) error {
	d.live, d.rolledBack, d.staged = slices.Clone(d.orig), true, false
	d.check("unswap")
	return nil
}

func (d *fakeDB) Commit(context.Context) { d.committed, d.staged = true, false }

func (d *fakeDB) contents() string { return strings.Join(d.live, ",") }

func (d *fakeDB) String() string {
	return fmt.Sprintf("live %s, staged %v, committed %v, rolled back %v, broken %v", d.contents(), d.staged, d.committed, d.rolledBack, d.broken)
}

type fixture struct {
	o    *Ops
	site state.Site
	wp   *fakeWP
	db   *fakeDB
	work string
}

// newSite is a provisioned WordPress site with releases/initial live, its
// uploads in shared/, and the migration's inputs uploaded into shared/.
func newSite(t *testing.T, archive []byte) *fixture {
	t.Helper()
	owner := testOwner()
	if os.Getuid() == 0 {
		owner = sitefs.Owner{UID: 4242, GID: 4242}
	}
	f := &fixture{wp: &fakeWP{prefix: "wp_"}, db: &fakeDB{live: slices.Clone(oldTables), siteurl: "https://old.example.org"}, work: t.TempDir()}
	f.db.wp, f.wp.db = f.wp, f.db
	f.site = state.Site{ID: 3, Domain: "shop.example.com", Type: state.SiteWordPress, SystemUser: "cg-site-3", PHPVersion: "8.3",
		RootPath: filepath.Join(t.TempDir(), "shop.example.com")}
	f.site.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_3", User: "site_3"}
	open := func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, owner) }
	f.o = &Ops{
		WorkDir:   f.work,
		Open:      open,
		Releases:  &release.Ops{Open: open},
		WordPress: f.wp,
		StageDB:   f.db.stage,
		FreeSpace: func(string) (int64, error) { return 100 << 30, nil },
	}
	for p, c := range map[string]string{
		"releases/initial/index.php":   "<?php // initial",
		"shared/wp-config.php":         "<?php // config",
		"shared/uploads/2025/old.jpg":  "old",
		"shared/migration/db.sql":      newRows,
		"shared/migration/site.tar.gz": string(archive),
	} {
		full := filepath.Join(f.site.RootPath, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/initial", f.path(release.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) path(p string) string { return filepath.Join(f.site.RootPath, p) }

func (f *fixture) params(old string) rpc.MigrationParams {
	return rpc.MigrationParams{Site: f.site, Archive: "shared/migration/site.tar.gz", SQL: "/shared/migration/db.sql", OldDomain: old, ReleaseID: releaseID}
}

func (f *fixture) current(t *testing.T) string {
	t.Helper()
	l, err := os.Readlink(f.path(release.CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (f *fixture) read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(f.path(p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// clean checks that a migration left no staging directory in the site and
// nothing in the work directory.
func (f *fixture) clean(t *testing.T) {
	t.Helper()
	ents, _ := os.ReadDir(f.site.RootPath)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), stagePrefix) {
			t.Errorf("staging directory %s left in the site", e.Name())
		}
	}
	if ents, _ := os.ReadDir(f.work); len(ents) != 0 {
		t.Errorf("%d entries left in the work directory", len(ents))
	}
	// Out of maintenance, and never without its tables on the way.
	for _, rel := range []string{"releases/initial/.maintenance", "releases/" + releaseID + "/.maintenance"} {
		absent(t, f.path(rel))
	}
	if len(f.db.broken) != 0 {
		t.Errorf("the site's prefix had no options table at: %v", f.db.broken)
	}
}

// unchanged checks that a failed migration left the site as it was.
func (f *fixture) unchanged(t *testing.T) {
	t.Helper()
	if c := f.current(t); c != "releases/initial" {
		t.Errorf("current -> %s after a failed migration", c)
	}
	if got := f.read(t, "shared/uploads/2025/old.jpg"); got != "old" {
		t.Errorf("previous uploads: %q", got)
	}
	absent(t, f.path("shared/uploads/2026/new.jpg"))
	absent(t, f.path("releases/"+releaseID))
	if got := f.db.contents(); got != strings.Join(oldTables, ",") || f.db.staged || f.wp.prefix != "wp_" {
		t.Errorf("database holds %s (staged %v, prefix %s) after a failed migration", got, f.db.staged, f.wp.prefix)
	}
	// The inputs stay for a retry.
	for _, p := range []string{"shared/migration/site.tar.gz", "shared/migration/db.sql"} {
		if _, err := os.Lstat(f.path(p)); err != nil {
			t.Errorf("input %s removed after a failed migration", p)
		}
	}
	f.clean(t)
}

func siteArchive(t *testing.T) []byte {
	return tgz(t,
		dir("public_html/"),
		reg("public_html/index.php", "<?php // migrated"),
		reg("public_html/wp-config.php", "<?php // the old host's credentials"),
		reg("public_html/wp-content/themes/t/style.css", "body{}"),
		reg("public_html/wp-content/uploads/2026/new.jpg", "new"),
		symlink("public_html/up", ".."),
	)
}

func TestImport(t *testing.T) {
	f := newSite(t, siteArchive(t))
	res, err := f.o.Import(ctx, f.params("old.example.org"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReleaseID != releaseID || res.Files != 4 || res.Skipped != 1 {
		t.Fatalf("result = %+v", res)
	}
	if c := f.current(t); c != "releases/"+releaseID {
		t.Fatalf("current -> %s", c)
	}
	rel := "releases/" + releaseID + "/"
	if got := f.read(t, rel+"index.php"); got != "<?php // migrated" {
		t.Fatalf("index.php = %q", got)
	}
	// The layout is the site's: its own configuration and uploads.
	if l, _ := os.Readlink(f.path(rel + "wp-config.php")); l != "../../shared/wp-config.php" {
		t.Fatalf("wp-config.php -> %q", l)
	}
	if l, _ := os.Readlink(f.path(rel + "wp-content/uploads")); l != "../../../shared/uploads" {
		t.Fatalf("uploads -> %q", l)
	}
	if _, err := os.Stat(f.path(rel + "wp-content/mu-plugins/cloudground-connector.php")); err != nil {
		t.Fatalf("connector not installed: %v", err)
	}
	absent(t, f.path(rel+"up"))
	// The uploads are the archive's.
	if got := f.read(t, "shared/uploads/2026/new.jpg"); got != "new" {
		t.Fatalf("migrated upload = %q", got)
	}
	absent(t, f.path("shared/uploads/2025/old.jpg"))
	if os.Getuid() == 0 {
		for _, p := range []string{"shared/uploads", "shared/uploads/2026/new.jpg", rel + "index.php"} {
			fi, err := os.Lstat(f.path(p))
			if err != nil {
				t.Fatal(err)
			}
			if uid := fi.Sys().(*syscall.Stat_t).Uid; uid != 4242 {
				t.Fatalf("%s belongs to uid %d, not the site user", p, uid)
			}
		}
	}
	// The database, the URLs, the cache.
	if f.db.imported != newRows || !f.db.committed || f.db.rolledBack || f.db.contents() != "wp_options,wp_posts,wp_users,wp_postmeta" {
		t.Fatalf("database: %+v", f.db)
	}
	// While the database and code switched, the site was in maintenance
	// (checked from the URL rewrite, the last step).
	if !f.wp.sawMaintenance {
		t.Fatal("the site was not in maintenance while the migration switched it")
	}
	if len(f.wp.replaced) != 1 || f.wp.replaced[0] != [2]string{"https://old.example.org", "https://shop.example.com"} {
		t.Fatalf("search-replace: %v", f.wp.replaced)
	}
	if f.wp.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", f.wp.refreshes)
	}
	// The inputs are gone, and so is everything temporary.
	absent(t, f.path("shared/migration/site.tar.gz"))
	absent(t, f.path("shared/migration/db.sql"))
	f.clean(t)
}

// A site moved under its own domain has no URLs to rewrite.
func TestImportSameDomainSkipsRewrite(t *testing.T) {
	f := newSite(t, siteArchive(t))
	if _, err := f.o.Import(ctx, f.params("SHOP.example.com")); err != nil {
		t.Fatal(err)
	}
	if len(f.wp.replaced) != 0 {
		t.Fatalf("search-replace ran: %v", f.wp.replaced)
	}
}

// Without a dump the database is not touched; without uploads in the
// archive the site's own stay.
func TestImportFilesOnly(t *testing.T) {
	f := newSite(t, tgz(t, reg("index.php", "<?php // migrated")))
	p := f.params("")
	p.SQL = ""
	if _, err := f.o.Import(ctx, p); err != nil {
		t.Fatal(err)
	}
	if f.db.imported != "" {
		t.Fatal("the database was imported")
	}
	if got := f.read(t, "shared/uploads/2025/old.jpg"); got != "old" {
		t.Fatalf("the site's uploads changed: %q", got)
	}
	if _, err := os.Lstat(f.path("shared/migration/db.sql")); err != nil {
		t.Fatal("an input the migration did not use was removed")
	}
	f.clean(t)
}

// A failure after the release went live undoes everything: current, the
// uploads, the database, the release.
func TestImportRollsBackOnRewriteFailure(t *testing.T) {
	f := newSite(t, siteArchive(t))
	f.wp.failSR = errors.New("wp-cli died")
	_, err := f.o.Import(ctx, f.params("old.example.org"))
	if err == nil || !strings.Contains(err.Error(), "wp-cli died") {
		t.Fatalf("err = %v", err)
	}
	if !f.db.rolledBack || f.db.committed {
		t.Fatalf("database: %+v", f.db)
	}
	if f.wp.refreshes == 0 {
		t.Fatal("the site was not refreshed after the rollback")
	}
	f.unchanged(t)
}

func TestImportRollsBackOnDatabaseFailure(t *testing.T) {
	f := newSite(t, siteArchive(t))
	f.db.failImport = errors.New("syntax error at line 3")
	if _, err := f.o.Import(ctx, f.params("")); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("err = %v", err)
	}
	f.unchanged(t)
}

// A site without shared/uploads gets the archive's, and loses them again on
// a rollback.
func TestImportRollsBackUploadsMovedIntoPlace(t *testing.T) {
	f := newSite(t, siteArchive(t))
	if err := os.RemoveAll(f.path("shared/uploads")); err != nil {
		t.Fatal(err)
	}
	f.wp.failSR = errors.New("wp-cli died")
	if _, err := f.o.Import(ctx, f.params("old.example.org")); err == nil {
		t.Fatal("the migration succeeded")
	}
	absent(t, f.path("shared/uploads"))
	if c := f.current(t); c != "releases/initial" {
		t.Fatalf("current -> %s", c)
	}
	f.clean(t)
}

// A bomb is refused before it is written, and nothing changes.
func TestImportRefusesBomb(t *testing.T) {
	f := newSite(t, siteArchive(t))
	big := tgz(t, reg("index.php", "x"), reg("big.log", strings.Repeat("a", 4<<20)))
	if err := os.WriteFile(f.path("shared/migration/site.tar.gz"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	f.o.Limits = DefaultLimits
	f.o.Limits.MinBudget = 1 << 20
	_, err := f.o.Import(ctx, f.params(""))
	wantInvalid(t, "bomb", err, "expands to more than")
	if f.db.imported != "" {
		t.Fatal("the database was imported")
	}
	f.unchanged(t)

	// The disk: an archive larger than the free space is refused too.
	f.o.Limits = Limits{}
	f.o.FreeSpace = func(string) (int64, error) { return DefaultLimits.Reserve + 1<<20, nil }
	_, err = f.o.Import(ctx, f.params(""))
	wantInvalid(t, "disk", err, "expands to more than")
	f.unchanged(t)
}

func TestImportValidation(t *testing.T) {
	f := newSite(t, siteArchive(t))
	for name, mod := range map[string]func(*rpc.MigrationParams){
		"release id":    func(p *rpc.MigrationParams) { p.ReleaseID = "initial" },
		"old domain":    func(p *rpc.MigrationParams) { p.OldDomain = "bad domain;" },
		"archive type":  func(p *rpc.MigrationParams) { p.Archive = "shared/migration/site.zip" },
		"dump type":     func(p *rpc.MigrationParams) { p.SQL = "shared/migration/db.txt" },
		"no database":   func(p *rpc.MigrationParams) { p.Site.Config.Database.Enabled = false },
		"external db":   func(p *rpc.MigrationParams) { p.Site.Config.Database.External = true },
		"missing input": func(p *rpc.MigrationParams) { p.Archive = "shared/nothing.tgz" },
	} {
		p := f.params("")
		mod(&p)
		_, err := f.o.Import(ctx, p)
		if agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	f.unchanged(t)
}

// The inputs are the site user's: one that is a symlink (to a file of
// root's, say) is refused, never read through.
func TestImportRefusesLinkedInputs(t *testing.T) {
	f := newSite(t, siteArchive(t))
	secret := filepath.Join(t.TempDir(), "secret.tar.gz")
	if err := os.WriteFile(secret, siteArchive(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{secret, "site.tar.gz"} {
		_ = os.Remove(f.path("shared/migration/link.tar.gz"))
		if err := os.Symlink(target, f.path("shared/migration/link.tar.gz")); err != nil {
			t.Fatal(err)
		}
		p := f.params("")
		p.Archive = "shared/migration/link.tar.gz"
		_, err := f.o.Import(ctx, p)
		wantInvalid(t, target, err, "symlink")
	}
	// A link on the way out of the site is refused as well.
	if err := os.Symlink(filepath.Dir(secret), f.path("shared/out")); err != nil {
		t.Fatal(err)
	}
	p := f.params("")
	p.Archive = "shared/out/secret.tar.gz"
	if _, err := f.o.Import(ctx, p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("archive through a link out of the site: %v", err)
	}
	if c := f.current(t); c != "releases/initial" {
		t.Fatalf("current -> %s", c)
	}
}
