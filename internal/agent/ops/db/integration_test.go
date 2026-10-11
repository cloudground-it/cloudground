//go:build hostintegration

// The console, the replace with its rollback, the restore test and the
// sweep against a real MariaDB: run as root where CG_TEST_MYSQL_SOCKET is
// the server's socket and the mariadb client and mariadb-dump are installed
// (a throwaway container only).
//
//	go test -tags hostintegration -run Integration ./internal/agent/ops/db/
package db

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
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestIntegrationDatabaseOps(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	socket := os.Getenv("CG_TEST_MYSQL_SOCKET")
	if socket == "" {
		t.Skip("CG_TEST_MYSQL_SOCKET not set")
	}
	work := t.TempDir()
	m := host.NewMariaDB(socket, work, host.OSExec{Timeout: time.Minute})
	defer m.Close()
	// Already tuned: the server in the container is not restarted.
	tuned := filepath.Join(t.TempDir(), "tune.cnf")
	os.WriteFile(tuned, []byte("[mysqld]\ninnodb_flush_log_at_trx_commit = 1\n"), 0o644)
	o := &Ops{MariaDB: m, Socket: socket, WorkDir: work, TuneFile: tuned, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
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
	site := state.Site{ID: 9000, Domain: "it.example.com", Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "site_9000", User: "site_9000"}}}
	t.Cleanup(func() {
		o.DropSite(ctx, site.ID)
		m.Exec(ctx, "DROP DATABASE IF EXISTS cgit_other")
	})
	must(m.Exec(ctx, "CREATE DATABASE IF NOT EXISTS cgit_other"))
	must(m.Exec(ctx, "CREATE TABLE IF NOT EXISTS cgit_other.secret (v TEXT)"))
	must(o.ProvisionSite(ctx, site, `pass'word"\with;quotes`))
	must(m.Exec(ctx, "CREATE TABLE site_9000.wp_options (id INT)"))
	must(m.Exec(ctx, "INSERT INTO site_9000.wp_options VALUES (1), (2)"))

	// The console: ordinary reads work, other databases and writes in
	// read-only mode are refused by the server, and client commands do not
	// exist on the protocol.
	q := func(sql string, ro bool) rpc.DBQueryResult {
		t.Helper()
		r, err := o.Query(ctx, rpc.DBQueryParams{Site: site, Database: "site_9000", SQL: sql, ReadOnly: ro})
		must(err)
		return r
	}
	if r := q("SELECT COUNT(*) FROM wp_options;", true); len(r.Rows) != 1 || r.Rows[0][0] != "2" {
		t.Fatalf("count %+v", r)
	}
	for _, sql := range []string{"SELECT * FROM cgit_other.secret", "SELECT COUNT(*) FROM mysql.user", "INSERT INTO wp_options VALUES (3)", "system id", "select 1; select 2"} {
		if r := q(sql, true); !strings.HasPrefix(r.Message, "ERROR ") || len(r.Rows) != 0 {
			t.Fatalf("%q: %+v", sql, r)
		}
	}

	// A statement that outlives the console is stopped on the server, and
	// nothing of the scoped account is left running.
	o.ConsoleTimeout = 2 * time.Second
	start := time.Now()
	r := q("SET STATEMENT max_statement_time=0 FOR SELECT SLEEP(30)", true)
	if time.Since(start) > 10*time.Second || !strings.Contains(r.Message, "ran longer") {
		t.Fatalf("after %s: %+v", time.Since(start), r)
	}
	r = q("SELECT SLEEP(30)", true)
	if !strings.Contains(r.Message, "max_statement_time") && !strings.Contains(r.Message, "ran longer") {
		t.Fatalf("%+v", r)
	}
	time.Sleep(500 * time.Millisecond)
	if left := rows("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER LIKE 'cgq\\_%'"); left[0] != "0" {
		t.Fatalf("%s scoped connections still running", left[0])
	}
	o.ConsoleTimeout = 0

	// Tables lists the site's tables only.
	tl, err := o.Tables(ctx, rpc.DBTablesParams{Site: site})
	must(err)
	if len(tl.Tables) != 1 || tl.Tables[0].Name != "wp_options" || tl.Tables[0].Engine == "" {
		t.Fatalf("tables %+v", tl)
	}

	// A failed import is rolled back exactly: the table it created is gone.
	bad := filepath.Join(t.TempDir(), "bad.sql")
	os.WriteFile(bad, []byte("CREATE TABLE cg_e2e_import (id INT);\nsystem touch /tmp/cgit-pwn\n"), 0o600)
	if _, err := o.Replace(ctx, "site_9000", bad, Over); err == nil {
		t.Fatal("a sandbox violation imported")
	}
	if _, err := os.Stat("/tmp/cgit-pwn"); err == nil {
		t.Fatal("the client command ran")
	}
	if got := rows("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'site_9000' ORDER BY 1"); strings.Join(got, ",") != "wp_options" {
		t.Fatalf("after rollback: %v", got)
	}
	if got := rows("SELECT COUNT(*) FROM site_9000.wp_options"); got[0] != "2" {
		t.Fatalf("rows after rollback: %v", got)
	}
	// The site's account still works after the database was recreated.
	if got := rows("SELECT COUNT(*) FROM mysql.db WHERE Db = ? AND User = 'site_9000'", host.GrantPattern("site_9000")); got[0] != "1" {
		t.Fatalf("grant lost: %v", got)
	}

	// Fresh: the database becomes the dump, without tables made since.
	snap := filepath.Join(t.TempDir(), "snap.sql")
	must(o.DumpFile(ctx, "site_9000", snap))
	must(m.Exec(ctx, "CREATE TABLE site_9000.cg_after (id INT)"))
	tx, err := o.Replace(ctx, "site_9000", snap, Fresh)
	must(err)
	tx.Commit()
	if got := rows("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'site_9000' ORDER BY 1"); strings.Join(got, ",") != "wp_options" {
		t.Fatalf("after a fresh replace: %v", got)
	}

	// The restore test imports, counts and drops its scratch database.
	n, err := o.VerifyDump(ctx, snap, true, 1)
	if err != nil || n != 1 {
		t.Fatalf("verify %d %v", n, err)
	}
	if left := rows("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'cgv\\_%'"); left[0] != "0" {
		t.Fatal("scratch database left")
	}

	// The sweep drops a leftover scoped account and nothing else.
	acct, err := m.ScopedAccount(ctx, "site_9000", true)
	must(err)
	must(o.Sweep(ctx))
	if got := rows("SELECT COUNT(*) FROM mysql.user WHERE User = ?", acct.User); got[0] != "0" {
		t.Fatal("leftover account kept")
	}
	if got := rows("SELECT COUNT(*) FROM mysql.user WHERE User = 'site_9000'"); got[0] != "1" {
		t.Fatal("the sweep dropped a site account")
	}
	acct.Close()
	ok, err := o.Exists(ctx, "site_9000")
	if err != nil || !ok {
		t.Fatalf("exists %v %v", ok, err)
	}
}
