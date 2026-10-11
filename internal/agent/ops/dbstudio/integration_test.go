//go:build hostintegration

// Quarry against a real MariaDB: run as root where CG_TEST_MYSQL_SOCKET
// is the server's socket and mariadb-dump is installed (a throwaway
// container only, e.g. mariadb:11.8).
//
//	go test -tags hostintegration -run Integration ./internal/agent/ops/dbstudio/
package dbstudio

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestIntegrationDBStudio(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	socket := os.Getenv("CG_TEST_MYSQL_SOCKET")
	if socket == "" {
		t.Skip("CG_TEST_MYSQL_SOCKET not set")
	}
	ctx := context.Background()
	work := t.TempDir()
	m := host.NewMariaDB(socket, work, host.OSExec{Timeout: time.Minute})
	defer m.Close()
	tuned := filepath.Join(t.TempDir(), "tune.cnf")
	os.WriteFile(tuned, []byte("[mysqld]\ninnodb_flush_log_at_trx_commit = 1\n"), 0o644)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbo := &db.Ops{MariaDB: m, Socket: socket, WorkDir: work, TuneFile: tuned, Log: quiet}
	root := filepath.Join(t.TempDir(), "it.example.com")
	os.MkdirAll(filepath.Join(root, "shared"), 0o755)
	o := &Ops{DB: dbo, Log: quiet, Open: func(s state.Site) (*sitefs.Root, error) {
		return sitefs.Open(s.RootPath, sitefs.Owner{UID: 0, GID: 0})
	}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := func(q string, args ...any) []string {
		t.Helper()
		r, err := m.QueryStrings(ctx, q, args...)
		must(err)
		return r
	}
	t.Cleanup(func() {
		dbo.DropSite(ctx, 9)
		m.Exec(ctx, "DROP DATABASE IF EXISTS cgit_tenant")
	})
	must(m.Exec(ctx, "CREATE DATABASE IF NOT EXISTS cgit_tenant"))
	must(m.Exec(ctx, "CREATE TABLE IF NOT EXISTS cgit_tenant.secret (v TEXT)"))
	must(m.Exec(ctx, "INSERT INTO cgit_tenant.secret VALUES ('other tenant')"))
	site := state.Site{ID: 9, Domain: "it.example.com", Type: state.SiteWordPress, RootPath: root,
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "cgit_studio", User: "cgit_studio"}}}
	must(dbo.ProvisionSite(ctx, site, "a-long-enough-password-1"))
	for _, q := range []string{
		"CREATE TABLE cgit_studio.wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title TEXT NOT NULL, post_status VARCHAR(20) NOT NULL DEFAULT 'publish', post_parent BIGINT NULL, UNIQUE KEY slug (post_status, ID))",
		"INSERT INTO cgit_studio.wp_posts (post_title, post_status, post_parent) VALUES ('Hello', 'publish', NULL), ('50% off_now', 'draft', 1), ('It''s \\\\ fine', 'publish', 1)",
		"CREATE TABLE cgit_studio.wp_postmeta (meta_id INT PRIMARY KEY, post_id BIGINT UNSIGNED, CONSTRAINT fk_post FOREIGN KEY (post_id) REFERENCES cgit_studio.wp_posts (ID) ON DELETE CASCADE)",
		"CREATE TABLE cgit_studio.`odd``name` (`we``ird` INT PRIMARY KEY)",
	} {
		must(m.Exec(ctx, q))
	}

	// Browse.
	tl, err := o.Tables(ctx, rpc.DBStudioTablesParams{Site: site, Columns: true})
	must(err)
	if len(tl.Tables) != 3 || tl.Tables[2].Name != "wp_posts" || tl.Tables[2].Engine != "InnoDB" || len(tl.Columns["wp_posts"]) != 4 {
		t.Fatalf("tables %+v", tl)
	}
	st, err := o.Structure(ctx, rpc.DBStudioTableParams{Site: site, Table: "wp_postmeta"})
	must(err)
	if len(st.ForeignKeys) != 1 || st.ForeignKeys[0].RefTable != "wp_posts" || st.ForeignKeys[0].OnDelete != "CASCADE" ||
		!strings.Contains(st.CreateSQL, "CREATE TABLE `wp_postmeta`") {
		t.Fatalf("structure %+v", st)
	}
	page, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: "wp_posts",
		Filters: []rpc.DBFilter{{Column: "post_title", Op: "contains", Value: "% off_"}}})
	must(err)
	if len(page.Rows) != 1 || *page.Rows[0].Values[1] != "50% off_now" || page.Total != 1 {
		t.Fatalf("LIKE with %% and _ as text: %+v", page)
	}
	page, err = o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: "wp_posts", Sort: []rpc.DBSort{{Column: "post_title", Desc: true}},
		Filters: []rpc.DBFilter{{Column: "post_parent", Op: "null"}}})
	must(err)
	if len(page.Rows) != 1 || page.Rows[0].Values[3] != nil {
		t.Fatalf("IS NULL: %+v", page)
	}
	odd, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: "odd`name", Sort: []rpc.DBSort{{Column: "we`ird"}}})
	must(err)
	if odd.Columns[0].Name != "we`ird" {
		t.Fatalf("a name with a backtick: %+v", odd)
	}

	// Identifier injection in every place a name goes: refused, the
	// server untouched.
	for _, name := range []string{"ID` DESC, (SELECT SLEEP(5)) -- ", "ID`; DROP TABLE wp_posts; -- ", "id"} {
		if _, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: "wp_posts", Sort: []rpc.DBSort{{Column: name}}}); code(err) != "not_found" {
			t.Fatalf("sort %q: %v", name, err)
		}
		if _, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: "wp_posts", Filters: []rpc.DBFilter{{Column: name, Op: "notnull"}}}); code(err) != "not_found" {
			t.Fatalf("filter %q: %v", name, err)
		}
	}
	for _, table := range []string{"cgit_tenant`.`secret", "WP_POSTS", "secret", "user"} {
		if _, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: table}); code(err) != "not_found" {
			t.Fatalf("table %q: %v", table, err)
		}
	}
	if got := rows("SELECT COUNT(*) FROM cgit_studio.wp_posts"); got[0] != "3" {
		t.Fatal("the table changed")
	}

	// Another tenant's database and mysql.user are out of reach of the
	// scoped account, also through the SQL editor.
	for _, q := range []string{"SELECT * FROM cgit_tenant.secret", "SELECT COUNT(*) FROM mysql.user", "SELECT * FROM information_schema.TABLES WHERE TABLE_SCHEMA='cgit_tenant'"} {
		r, err := dbo.Query(ctx, rpc.DBQueryParams{Site: site, Database: "cgit_studio", SQL: q, ReadOnly: false})
		must(err)
		if len(r.Rows) != 0 && !strings.HasPrefix(r.Message, "ERROR ") {
			t.Fatalf("%q answered %+v", q, r)
		}
	}

	// Edit: the preview is what runs, the batch is one transaction.
	p := rpc.DBStudioEditParams{Site: site, Table: "wp_posts", DryRun: true, Changes: []rpc.DBChange{
		{Op: "update", Key: map[string]string{"ID": "1"}, Values: map[string]*string{"post_title": sp(`O'Hara \ "x"`), "post_parent": sp("2")}},
		{Op: "insert", Values: map[string]*string{"post_title": sp("new"), "post_parent": nil}},
	}}
	dry, err := o.Edit(ctx, p)
	must(err)
	p.DryRun = false
	run, err := o.Edit(ctx, p)
	must(err)
	if strings.Join(dry.SQL, ";") != strings.Join(run.SQL, ";") || !run.Executed || run.Affected != 2 {
		t.Fatalf("preview %q, run %+v", dry.SQL, run)
	}
	if got := rows("SELECT CONCAT(post_title, '|', post_parent) FROM cgit_studio.wp_posts WHERE ID = 1"); got[0] != `O'Hara \ "x"|2` {
		t.Fatalf("after the edit: %q", got)
	}
	// What the preview shows, run by hand, does the same: it is real SQL.
	shown := strings.Replace(dry.SQL[1], "'O''Hara", "'Again O''Hara", 1)
	if err := m.Exec(ctx, strings.Replace(shown, "`wp_posts`", "cgit_studio.`wp_posts`", 1)); err != nil {
		t.Fatalf("the preview is not valid SQL: %v\n%s", err, shown)
	}
	if got := rows("SELECT post_title FROM cgit_studio.wp_posts WHERE ID = 1"); got[0] != `Again O'Hara \ "x"` {
		t.Fatalf("the preview's literal differs: %q", got)
	}

	// A batch whose second change fails leaves nothing of the first.
	_, err = o.Edit(ctx, rpc.DBStudioEditParams{Site: site, Table: "wp_posts", Changes: []rpc.DBChange{
		{Op: "update", Key: map[string]string{"ID": "2"}, Values: map[string]*string{"post_title": sp("should not stay")}},
		{Op: "insert", Values: map[string]*string{"ID": sp("1"), "post_title": sp("duplicate")}},
	}})
	if code(err) != "invalid" || !strings.Contains(err.Error(), "Duplicate") {
		t.Fatalf("a failing batch: %v", err)
	}
	if got := rows("SELECT post_title FROM cgit_studio.wp_posts WHERE ID = 2"); got[0] != "50% off_now" {
		t.Fatalf("not rolled back: %q", got)
	}
	// A row that is gone is a conflict, and rolls back too.
	_, err = o.Edit(ctx, rpc.DBStudioEditParams{Site: site, Table: "wp_posts", Changes: []rpc.DBChange{
		{Op: "update", Key: map[string]string{"ID": "2"}, Values: map[string]*string{"post_title": sp("should not stay")}},
		{Op: "delete", Key: map[string]string{"ID": "999"}},
	}})
	if code(err) != "conflict" {
		t.Fatalf("a missing row: %v", err)
	}
	if got := rows("SELECT post_title FROM cgit_studio.wp_posts WHERE ID = 2"); got[0] != "50% off_now" {
		t.Fatalf("not rolled back: %q", got)
	}
	// An update to the same values still matches its row.
	if _, err := o.Edit(ctx, rpc.DBStudioEditParams{Site: site, Table: "wp_posts", Changes: []rpc.DBChange{
		{Op: "update", Key: map[string]string{"ID": "2"}, Values: map[string]*string{"post_title": sp("50% off_now")}},
	}}); err != nil {
		t.Fatalf("a no-op update: %v", err)
	}

	// A read-only caller's write is refused by the server itself.
	_, err = o.Edit(ctx, rpc.DBStudioEditParams{Site: site, Table: "wp_posts", ReadOnly: true, Changes: []rpc.DBChange{
		{Op: "delete", Key: map[string]string{"ID": "3"}},
	}})
	if code(err) != "invalid" || !strings.Contains(err.Error(), "1142") {
		t.Fatalf("a read-only delete: %v", err)
	}
	_, err = o.Alter(ctx, rpc.DBStudioAlterParams{Site: site, Table: "wp_posts", Kind: "add_column", ReadOnly: true,
		Column: &rpc.DBNewColumn{Name: "ro", Type: "INT", Nullable: true}})
	if code(err) != "invalid" || !strings.Contains(err.Error(), "1142") {
		t.Fatalf("a read-only alter: %v", err)
	}
	if got := rows("SELECT COUNT(*) FROM cgit_studio.wp_posts WHERE ID = 3"); got[0] != "1" {
		t.Fatal("a read-only caller deleted a row")
	}

	// Guided ALTER.
	a, err := o.Alter(ctx, rpc.DBStudioAlterParams{Site: site, Table: "wp_posts", Kind: "add_column",
		Column: &rpc.DBNewColumn{Name: "note", Type: "VARCHAR", Length: 50, Default: sp("it's"), After: "post_title"}})
	must(err)
	if !a.Executed || rows("SELECT COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='cgit_studio' AND TABLE_NAME='wp_posts' AND COLUMN_NAME='note'")[0] != "'it''s'" {
		t.Fatalf("add column: %+v", a)
	}
	_, err = o.Alter(ctx, rpc.DBStudioAlterParams{Site: site, Table: "wp_posts", Kind: "add_index",
		Index: &rpc.DBNewIndex{Name: "by_title", Columns: []rpc.DBIndexColumn{{Name: "post_title", Length: 20}}}})
	must(err)
	st, err = o.Structure(ctx, rpc.DBStudioTableParams{Site: site, Table: "wp_posts"})
	must(err)
	found := false
	for _, ix := range st.Indexes {
		if ix.Name == "by_title" && ix.Columns[0].Length == 20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the index is not there: %+v", st.Indexes)
	}

	// A page past 8 MiB of cells is cut, and still counts its table.
	must(m.Exec(ctx, "CREATE TABLE cgit_studio.wide (id INT PRIMARY KEY, a LONGTEXT, b LONGTEXT, c LONGTEXT, d LONGTEXT, e LONGTEXT, f LONGTEXT)"))
	must(m.Exec(ctx, "INSERT INTO cgit_studio.wide SELECT seq, REPEAT('a', 5000), REPEAT('b', 5000), REPEAT('c', 5000), REPEAT('d', 5000), REPEAT('e', 5000), REPEAT('f', 5000) FROM cgit_studio.seq_1_to_500"))
	wide, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: site, Table: "wide", PageSize: 500})
	must(err)
	if !wide.Truncated || len(wide.Rows) == 0 || len(wide.Rows) >= 500 || wide.Total != 500 || len(wide.Rows[0].Cut) != 6 {
		t.Fatalf("a wide page: truncated %v, %d rows, total %d", wide.Truncated, len(wide.Rows), wide.Total)
	}
	must(m.Exec(ctx, "DROP TABLE cgit_studio.wide"))

	// Export and import, CSV and SQL.
	ex, err := o.Export(ctx, rpc.DBStudioExportParams{Site: site, Format: "csv", Tables: []string{"wp_posts"}})
	must(err)
	if ex.Rows != 4 {
		t.Fatalf("csv export %+v", ex)
	}
	must(m.Exec(ctx, "DELETE FROM cgit_studio.wp_postmeta"))
	must(m.Exec(ctx, "DELETE FROM cgit_studio.wp_posts"))
	os.MkdirAll(filepath.Join(root, "shared/imports"), 0o750)
	data, _ := os.ReadFile(filepath.Join(root, "shared/exports", ex.Name))
	os.WriteFile(filepath.Join(root, "shared/imports/posts.csv"), data, 0o640)
	in, err := o.ImportCSV(ctx, rpc.DBStudioImportCSVParams{Site: site, Table: "wp_posts", Name: "posts.csv"})
	must(err)
	if in.Rows != 4 || rows("SELECT COUNT(*) FROM cgit_studio.wp_posts WHERE post_parent IS NULL")[0] != "1" ||
		rows("SELECT post_title FROM cgit_studio.wp_posts WHERE ID = 3")[0] != `It's \ fine` {
		t.Fatalf("csv import %+v", in)
	}
	// Importing the same rows again fails on the keys and leaves the table
	// as it was.
	if _, err := o.ImportCSV(ctx, rpc.DBStudioImportCSVParams{Site: site, Table: "wp_posts", Name: "posts.csv"}); code(err) != "invalid" {
		t.Fatalf("a duplicate import: %v", err)
	}
	if got := rows("SELECT COUNT(*) FROM cgit_studio.wp_posts"); got[0] != "4" {
		t.Fatalf("after a failed import: %v", got)
	}
	sqlx, err := o.Export(ctx, rpc.DBStudioExportParams{Site: site, Format: "sql", Tables: []string{"wp_posts"}})
	must(err)
	dump, _ := os.ReadFile(filepath.Join(root, "shared/exports", sqlx.Name))
	if !strings.Contains(string(dump), "CREATE TABLE `wp_posts`") || strings.Contains(string(dump), "wp_postmeta") {
		t.Fatalf("sql export of one table: %d bytes", len(dump))
	}
	if _, err := o.Export(ctx, rpc.DBStudioExportParams{Site: site, Format: "sql", Tables: []string{"secret"}}); code(err) != "not_found" {
		t.Fatalf("an export of another tenant's table: %v", err)
	}
	if left := rows("SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'cgq\\_%'"); left[0] != "0" {
		t.Fatalf("%s scoped accounts left", left[0])
	}
}
