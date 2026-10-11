package migration

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/i18n"
	"github.com/cloudground-it/cloudground/internal/state"
)

// ------------------------------------------------------------ table prefix --

func (f *fixture) dump(t *testing.T, sql string) {
	t.Helper()
	if err := os.WriteFile(f.path("shared/migration/db.sql"), []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A dump whose tables use another prefix than the site's: wp-config.php
// gets the dump's, or the site would come up on the fresh install's tables.
func TestImportSetsTheDumpsTablePrefix(t *testing.T) {
	f := newSite(t, siteArchive(t))
	f.dump(t, wpDump("xyz_"))
	if _, err := f.o.Import(ctx, f.params("")); err != nil {
		t.Fatal(err)
	}
	if f.wp.prefix != "xyz_" || len(f.wp.prefixes) != 1 {
		t.Fatalf("table prefix %q (set %v), want xyz_ once", f.wp.prefix, f.wp.prefixes)
	}
	if !f.db.committed {
		t.Fatal("the database import was not committed")
	}
	f.clean(t)
}

// The site's own prefix already: nothing is written.
func TestImportKeepsAMatchingTablePrefix(t *testing.T) {
	f := newSite(t, siteArchive(t))
	if _, err := f.o.Import(ctx, f.params("")); err != nil {
		t.Fatal(err)
	}
	if len(f.wp.prefixes) != 0 {
		t.Fatalf("the prefix was set although it matched: %v", f.wp.prefixes)
	}
}

// A failure after the prefix moved puts the old one back with everything
// else; a failure to set it rolls back too.
func TestImportRollsBackTheTablePrefix(t *testing.T) {
	f := newSite(t, siteArchive(t))
	f.dump(t, wpDump("xyz_"))
	f.wp.failSR = errors.New("wp-cli died")
	if _, err := f.o.Import(ctx, f.params("old.example.org")); err == nil {
		t.Fatal("the migration succeeded")
	}
	if f.wp.prefix != "wp_" || strings.Join(f.wp.prefixes, ",") != "xyz_,wp_" {
		t.Fatalf("prefix %q after the rollback (set %v)", f.wp.prefix, f.wp.prefixes)
	}
	if !f.db.rolledBack {
		t.Fatal("the database was not rolled back")
	}
	if c := f.current(t); c != "releases/initial" {
		t.Fatalf("current -> %s", c)
	}

	f = newSite(t, siteArchive(t))
	f.dump(t, wpDump("xyz_"))
	f.wp.failSet = errors.New("wp-config.php is not writable")
	if _, err := f.o.Import(ctx, f.params("")); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("err = %v", err)
	}
	if !f.db.rolledBack || strings.Join(f.wp.prefixes, ",") != "xyz_,wp_" {
		t.Fatalf("a failed prefix change was not rolled back: db %+v, set %v", f.db, f.wp.prefixes)
	}
	f.unchanged(t)
}

// A dump without a WordPress installation, or with two, is refused before
// anything changes: the site would run on tables that are not the dump's.
func TestImportRefusesAmbiguousDumps(t *testing.T) {
	for name, dump := range map[string]string{
		"no wordpress": "CREATE TABLE `orders` (id int);\nINSERT INTO `orders` VALUES (1);\n",
		"two sites":    wpDump("wp_") + wpDump("blog_"),
	} {
		f := newSite(t, siteArchive(t))
		f.dump(t, dump)
		_, err := f.o.Import(ctx, f.params(""))
		wantInvalid(t, name, err, "WordPress")
		if f.db.imported != "" || len(f.wp.prefixes) != 0 {
			t.Fatalf("%s: the database or the prefix changed", name)
		}
		f.unchanged(t)
	}
	// A site that is not WordPress imports any dump, and has no prefix.
	f := newSite(t, siteArchive(t))
	f.site.Type = state.SitePHP
	f.dump(t, "CREATE TABLE `orders` (id int);\n")
	if _, err := f.o.Import(ctx, f.params("")); err != nil {
		t.Fatalf("a PHP site's dump: %v", err)
	}
	if len(f.wp.prefixes) != 0 {
		t.Fatal("a table prefix was set on a site that is not WordPress")
	}
}

func TestWordPressPrefix(t *testing.T) {
	read := func(dump string) []string {
		t.Helper()
		tables, err := DumpTables(strings.NewReader(dump))
		if err != nil {
			t.Fatal(err)
		}
		return tables
	}
	for _, c := range []struct {
		name, dump, want string
	}{
		{"mariadb-dump", wpDump("xyz_"), "xyz_"},
		{"the default", wpDump("wp_"), "wp_"},
		{"unquoted, IF NOT EXISTS, a database", "create table if not exists db.Ab9_options (x int);\nCREATE TABLE `db`.`Ab9_posts` (\nCREATE TABLE Ab9_users (\n", "Ab9_"},
		{"a multisite: sub-sites have no users table", wpDump("wp_") + "CREATE TABLE `wp_2_options` (\nCREATE TABLE `wp_2_posts` (\n", "wp_"},
		{"no final newline", "CREATE TABLE `a_options` (\nCREATE TABLE `a_posts` (\nCREATE TABLE `a_users` (", "a_"},
	} {
		got, err := WordPressPrefix(read(c.dump), "")
		if err != nil || got != c.want {
			t.Errorf("%s: prefix %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for name, dump := range map[string]string{
		"nothing":              "",
		"a table in an INSERT": "INSERT INTO `x` VALUES ('CREATE TABLE `wp_options`');\nCREATE TABLE `wp_posts` (\nCREATE TABLE `wp_users` (\n",
		"an unusable prefix":   "CREATE TABLE `wp-x_options` (\nCREATE TABLE `wp-x_posts` (\nCREATE TABLE `wp-x_users` (\n",
		"options only":         "CREATE TABLE `wp_options` (\n",
	} {
		if p, err := WordPressPrefix(read(dump), ""); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%s: prefix %q, %v; want invalid", name, p, err)
		}
	}
	// A CREATE TABLE after a line longer than the reader's buffer is found.
	long := "INSERT INTO `t` VALUES " + strings.Repeat("(1),", 1<<20) + "(1);\n" + wpDump("q_")
	if got, err := WordPressPrefix(read(long), ""); err != nil || got != "q_" {
		t.Fatalf("after a long line: %q, %v", got, err)
	}
}

// Two WordPress table sets (a WP Staging copy, a leftover install): the
// archive's own wp-config.php picks one, read as text; without one that
// names a set of the dump, the dump is refused.
func TestWordPressPrefixChosenByTheArchive(t *testing.T) {
	two := append(append([]string{}, "wp_options", "wp_posts", "wp_users"), "wpstg0_options", "wpstg0_posts", "wpstg0_users")
	if p, err := WordPressPrefix(two, "wpstg0_"); err != nil || p != "wpstg0_" {
		t.Fatalf("chosen by wp-config: %q %v", p, err)
	}
	if _, err := WordPressPrefix(two, "other_"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a wp-config naming neither: %v", err)
	}
	// One set: a stale wp-config does not stop the site running on it.
	if p, err := WordPressPrefix(two[:3], "old_"); err != nil || p != "wp_" {
		t.Fatalf("one set, stale wp-config: %q %v", p, err)
	}

	archive := tgz(t, dir("public_html/"), reg("public_html/index.php", "<?php"),
		reg("public_html/wp-config.php", "<?php\n// $table_prefix = 'wp_';\n$table_prefix = 'wp2_';\nrequire_once ABSPATH . 'wp-settings.php';\n"))
	f := newSite(t, archive)
	f.dump(t, wpDump("wp_")+wpDump("wp2_"))
	if _, err := f.o.Import(ctx, f.params("")); err != nil {
		t.Fatalf("a dump with two sets and an archive naming one: %v", err)
	}
	if f.wp.prefix != "wp2_" || !slices.Contains(f.db.live, "wp2_options") {
		t.Fatalf("prefix %q, tables %v", f.wp.prefix, f.db.live)
	}
	if len(f.db.broken) != 0 {
		t.Fatalf("the site's prefix had no options table at: %v", f.db.broken)
	}
}

// The prefix to come back to is read and checked before anything changes:
// a wp-config.php it cannot be read from refuses the migration.
func TestImportNeedsTheSitesPrefixFirst(t *testing.T) {
	f := newSite(t, siteArchive(t))
	f.wp.failGet = errors.New("wp-config.php has no single $table_prefix assignment")
	_, err := f.o.Import(ctx, f.params(""))
	wantInvalid(t, "unreadable prefix", err, "table prefix")
	if f.db.imported != "" {
		t.Fatal("the dump was staged")
	}
	f.unchanged(t)
}

// A staged dump that is not an installed WordPress (no siteurl) is refused
// before the live database moves; a failed swap or retire puts everything
// back, never leaving the site's prefix without its tables.
func TestImportStagedChecksAndDatabaseRollbacks(t *testing.T) {
	f := newSite(t, siteArchive(t))
	f.db.siteurl = ""
	_, err := f.o.Import(ctx, f.params(""))
	wantInvalid(t, "no siteurl", err, "siteurl")
	if f.db.rolledBack != true || f.db.committed {
		t.Fatalf("the staged dump was not dropped: %+v", f.db)
	}
	f.unchanged(t)

	for name, set := range map[string]func(*fakeDB){
		"swap":   func(d *fakeDB) { d.failSwap = errors.New("rename refused") },
		"retire": func(d *fakeDB) { d.failRetire = errors.New("rename refused") },
	} {
		f := newSite(t, siteArchive(t))
		f.dump(t, wpDump("xyz_"))
		set(f.db)
		if _, err := f.o.Import(ctx, f.params("")); err == nil || !strings.Contains(err.Error(), "rename refused") {
			t.Fatalf("%s: err = %v", name, err)
		}
		f.unchanged(t)
	}
}

// Agent start takes a site left in maintenance by a migration that died out
// of it; a .maintenance the site wrote itself stays.
func TestSweepEndsMaintenance(t *testing.T) {
	f := newSite(t, siteArchive(t))
	m := &run{o: f.o, p: f.params(""), oldID: "initial"}
	r, err := f.o.Open(f.site)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m.r = r
	if err := os.MkdirAll(f.path("releases/"+releaseID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.maintenanceOn(); err != nil {
		t.Fatal(err)
	}
	own := f.path("releases/" + releaseID + "/.maintenance")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("<?php $upgrading = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.path("releases/initial/.maintenance")); string(b) != string(maintenanceBody) {
		t.Fatalf("maintenance file: %q", b)
	}
	// The agent dies here; the next one sweeps.
	if err := f.o.Sweep(); err != nil {
		t.Fatal(err)
	}
	absent(t, f.path("releases/initial/.maintenance"))
	if _, err := os.Stat(own); err != nil {
		t.Fatal("the site's own .maintenance was removed")
	}
	if ents, _ := os.ReadDir(filepath.Join(f.work, maintenanceJournal)); len(ents) != 0 {
		t.Fatalf("journal left: %v", ents)
	}
	if err := f.o.Sweep(); err != nil {
		t.Fatal("a second sweep failed")
	}
}

// ------------------------------------------------------- expansion limit --

// The request's MaxRatio replaces the default expansion limit, within the
// ceiling; past the limit, the error names it, how to raise it, and carries
// its translation key.
func TestImportExpansionLimit(t *testing.T) {
	noise := make([]byte, 20<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range noise {
		noise[i] = byte(rng.Uint32())
	}
	// About 50 times: a megabyte of one byte, plus some noise.
	archive := tgz(t, reg("index.php", "<?php"), reg("a.txt", strings.Repeat("a", 1<<20)), reg("b.bin", string(noise)))
	f := newSite(t, archive)
	f.o.Limits = DefaultLimits
	f.o.Limits.MinBudget, f.o.Limits.MaxRatio = 1, 10
	p := f.params("")
	p.SQL = ""
	_, err := f.o.Import(ctx, p)
	e := agentrpc.AsError(err)
	if e == nil || e.Code != agentrpc.CodeInvalid || e.Key != KeyTooLargeRatio || len(e.Args) != 3 || e.Args[1] != "10" || e.Args[2] != "1000" {
		t.Fatalf("refused archive: %#v (%v)", e, err)
	}
	if !strings.Contains(e.Message, "10 times its compressed size") || !strings.Contains(e.Message, "up to 1000 times") {
		t.Fatalf("message %q", e.Message)
	}
	f.unchanged(t)

	for _, bad := range []int64{-1, MaxRatioCeiling + 1} {
		p.MaxRatio = bad
		if _, err := f.o.Import(ctx, p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("MaxRatio %d: %v", bad, err)
		}
	}
	p.MaxRatio = 500
	if _, err := f.o.Import(ctx, p); err != nil {
		t.Fatalf("with the limit raised: %v", err)
	}
}

// Which bound a too-large archive reached decides the message.
func TestTooLarge(t *testing.T) {
	l := Limits{MaxBytes: 20 << 30, MaxRatio: 200, MinBudget: 64 << 20, Reserve: 1 << 30}
	for _, c := range []struct {
		compressed, avail int64
		key               string
		args              []string
	}{
		{10 << 20, 100 << 30, KeyTooLargeRatio, []string{"2.0 GiB", "200", "1000"}},
		{1 << 30, 100 << 30, KeyTooLargeSize, []string{"20.0 GiB"}},
		{1 << 30, 5 << 30, KeyTooLargeDisk, []string{"4.0 GiB", "1.0 GiB"}},
	} {
		e := agentrpc.AsError(l.TooLarge(c.compressed, c.avail))
		if e == nil || e.Key != c.key || strings.Join(e.Args, "|") != strings.Join(c.args, "|") || e.Code != agentrpc.CodeInvalid {
			t.Errorf("TooLarge(%d, %d) = %#v, want %s %v", c.compressed, c.avail, e, c.key, c.args)
			continue
		}
		// The catalogue's English is the agent's message, so a translated
		// one says the same.
		args := make([]any, len(e.Args))
		for i, a := range e.Args {
			args[i] = a
		}
		if en := i18n.T("en", e.Key, args...); en != e.Message {
			t.Errorf("%s: catalogue %q, agent %q", e.Key, en, e.Message)
		}
		if it := i18n.T("it", e.Key, args...); it == e.Message || strings.Contains(it, "%!") {
			t.Errorf("%s: Italian %q", e.Key, it)
		}
	}
}
