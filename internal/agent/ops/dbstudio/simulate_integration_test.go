//go:build studiointegration

// The SQL editor's simulate (SPEC §12) against a real MariaDB: run as root
// where CG_TEST_MYSQL_SOCKET is the server's socket, in a throwaway
// container only:
//
//	go test -tags studiointegration -run Integration ./internal/agent/ops/dbstudio/
package dbstudio

import (
	"context"
	"database/sql"
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
	_ "github.com/go-sql-driver/mysql"
)

func TestIntegrationSimulate(t *testing.T) {
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
	root := filepath.Join(t.TempDir(), "sim.example.com")
	os.MkdirAll(filepath.Join(root, "shared"), 0o755)
	open := func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, sitefs.Owner{UID: 0, GID: 0}) }
	dbo := &db.Ops{MariaDB: m, Socket: socket, WorkDir: work, TuneFile: tuned, Log: quiet, Open: open}
	o := &Ops{DB: dbo, Log: quiet, Open: open}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	one := func(q string) string {
		t.Helper()
		r, err := m.QueryStrings(ctx, q)
		must(err)
		return strings.Join(r, ",")
	}
	site := state.Site{ID: 11, Domain: "sim.example.com", Type: state.SiteWordPress, RootPath: root,
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "cgit_sim", User: "cgit_sim"}}}
	const wpw = "simulate-writer-pw-1"
	t.Cleanup(func() {
		m.Exec(ctx, "DROP USER IF EXISTS 'cgit_simw'@'localhost'")
		m.Exec(ctx, "DROP DATABASE IF EXISTS cgit_sim")
	})
	_, err := dbo.CreateDB(ctx, rpc.SiteDatabaseParams{Site: site, Name: "cgit_sim"})
	must(err)
	_, err = dbo.CreateUser(ctx, rpc.DBUserParams{Site: site, User: "cgit_simw", Password: wpw, Grants: []state.DBGrant{{Database: "cgit_sim", Level: "all"}}})
	must(err)
	for _, q := range []string{
		"CREATE TABLE cgit_sim.orders (id INT PRIMARY KEY AUTO_INCREMENT, status VARCHAR(20), total INT) ENGINE=InnoDB",
		"INSERT INTO cgit_sim.orders (status, total) VALUES ('open', 10), ('open', 20), ('paid', 30), ('open', 40), ('void', 50)",
		"CREATE TABLE cgit_sim.archive (id INT PRIMARY KEY, status VARCHAR(20), total INT) ENGINE=InnoDB",
		"CREATE TABLE cgit_sim.legacy (id INT PRIMARY KEY, note TEXT) ENGINE=MyISAM",
		"INSERT INTO cgit_sim.legacy VALUES (1, 'old')",
		"CREATE TABLE cgit_sim.audited (id INT PRIMARY KEY) ENGINE=InnoDB",
		"CREATE TRIGGER cgit_sim.audited_ad AFTER DELETE ON cgit_sim.audited FOR EACH ROW INSERT INTO cgit_sim.legacy VALUES (OLD.id + 100, 'gone')",
		"INSERT INTO cgit_sim.audited VALUES (1), (2)",
	} {
		must(m.Exec(ctx, q))
	}
	before := one("SELECT GROUP_CONCAT(CONCAT(id, ':', status, ':', total) ORDER BY id) FROM cgit_sim.orders")
	in, err := o.SignIn(ctx, rpc.DBStudioOpenParams{Site: site, User: "cgit_simw", Password: wpw})
	must(err)
	if in.Refused {
		t.Fatal("the writer was refused")
	}
	sim := func(login, q string) rpc.DBStudioSimulateResult {
		t.Helper()
		p := rpc.DBStudioSimulateParams{Site: site, SQL: q}
		if login != "" {
			p.Database, p.Login = "cgit_sim", login
		}
		res, err := o.Simulate(ctx, p)
		must(err)
		return res
	}

	// As the signed-in user and as the panel's scoped account: the count
	// is the server's, the rows shown are the ones matched, nothing stays.
	for _, login := range []string{in.Login, ""} {
		res := sim(login, "UPDATE orders SET status = 'paid' WHERE status = 'open'")
		if res.Refused != "" || res.Message != "" || res.Affected != 3 || !res.RolledBack || res.Preview == nil || len(res.Preview.Rows) != 3 {
			t.Fatalf("UPDATE (%q): %+v", login, res)
		}
		// The count is of the rows matched, as phpMyAdmin's "rows
		// matched": three of these four are 'open' already, and a server
		// that counts only changed rows would say 1.
		res = sim(login, "UPDATE orders SET status = 'open' WHERE total < 50")
		if res.Refused != "" || res.Message != "" || res.Affected != 4 || !res.RolledBack {
			t.Fatalf("an UPDATE that leaves rows as they are (%q): %+v", login, res)
		}
		// Under the default strict sql_mode a string that is no number
		// into an INT column is the server's error, shown as its message,
		// with a count of 0: the statement changed nothing.
		res = sim(login, "UPDATE orders SET total = CONCAT(UPPER('a'), NOW())")
		if !strings.HasPrefix(res.Message, "ERROR ") || res.Affected != 0 || !res.RolledBack {
			t.Fatalf("a strict-mode conversion error (%q): %+v", login, res)
		}
		res = sim(login, "DELETE FROM orders WHERE total >= 20")
		if res.Affected != 4 || !res.RolledBack || res.Preview == nil || len(res.Preview.Rows) != 4 {
			t.Fatalf("DELETE (%q): %+v", login, res)
		}
		res = sim(login, "INSERT INTO archive SELECT * FROM orders WHERE status <> 'void'")
		if res.Affected != 4 || !res.RolledBack || res.Preview != nil {
			t.Fatalf("INSERT … SELECT (%q): %+v", login, res)
		}
		res = sim(login, "REPLACE INTO orders (id, status, total) VALUES (1, 'void', 0)")
		if res.Refused != "" || !res.RolledBack || res.Affected < 1 {
			t.Fatalf("REPLACE (%q): %+v", login, res)
		}
		// A statement the server refuses halfway is still rolled back.
		res = sim(login, "UPDATE orders SET id = 1 WHERE id > 1")
		if !strings.HasPrefix(res.Message, "ERROR ") || !res.RolledBack {
			t.Fatalf("a duplicate key (%q): %+v", login, res)
		}
		if got := one("SELECT GROUP_CONCAT(CONCAT(id, ':', status, ':', total) ORDER BY id) FROM cgit_sim.orders"); got != before {
			t.Fatalf("a simulation changed orders (%q): %s, was %s", login, got, before)
		}
		if got := one("SELECT COUNT(*) FROM cgit_sim.archive"); got != "0" {
			t.Fatalf("a simulation filled archive (%q): %s", login, got)
		}

		// What cannot be rolled back is refused, and not run.
		for q, want := range map[string]string{
			"DELETE FROM legacy": "MyISAM",
			"UPDATE orders SET total = (SELECT COUNT(*) FROM legacy)": "MyISAM",
			"DELETE FROM audited":                                "triggers",
			"TRUNCATE TABLE orders":                              "cannot be simulated",
			"ALTER TABLE orders ADD COLUMN x INT":                "cannot be simulated",
			"UPDATE orders SET status = 'x'; DELETE FROM orders": "one statement",
		} {
			if res := sim(login, q); !strings.Contains(res.Refused, want) {
				t.Fatalf("%q (%q): %+v", q, login, res)
			}
		}
		if got := one("SELECT GROUP_CONCAT(id ORDER BY id) FROM cgit_sim.legacy"); got != "1" {
			t.Fatalf("legacy changed (%q): %s", login, got)
		}
		if got := one("SELECT COUNT(*) FROM cgit_sim.audited"); got != "2" {
			t.Fatalf("audited changed (%q): %s", login, got)
		}
	}
	if got := one("SELECT GROUP_CONCAT(CONCAT(id, ':', status, ':', total) ORDER BY id) FROM cgit_sim.orders"); got != before {
		t.Fatalf("orders changed: %s, was %s", got, before)
	}

	// Names the check once missed: all digits, more than 64 bytes (but
	// 64 characters at most), a capital whose lower case differs between
	// Go and the server, a function of a package, and a "name" read
	// differently under ANSI_QUOTES with backslash escapes. Each reaches a
	// MyISAM table, so each must be refused and leave it as it was.
	const cyr = "ббббббббббббббббббббббббббббббббб" // 33 characters, 66 bytes
	for _, q := range []string{
		"CREATE TABLE cgit_sim.`123` (id INT PRIMARY KEY, v INT) ENGINE=MyISAM",
		"INSERT INTO cgit_sim.`123` VALUES (1, 1)",
		"CREATE TABLE cgit_sim.`" + cyr + "` (id INT PRIMARY KEY, v INT) ENGINE=MyISAM",
		"INSERT INTO cgit_sim.`" + cyr + "` VALUES (1, 1)",
		"CREATE TABLE cgit_sim.`İx` (id INT PRIMARY KEY, v INT) ENGINE=MyISAM",
		"INSERT INTO cgit_sim.`İx` VALUES (1, 1)",
		"CREATE TABLE cgit_sim.pklog (id INT AUTO_INCREMENT PRIMARY KEY, n INT) ENGINE=MyISAM",
		"CREATE PACKAGE cgit_sim.pk FUNCTION f(x INT) RETURNS INT; END",
		"CREATE PACKAGE BODY cgit_sim.pk FUNCTION f(x INT) RETURNS INT BEGIN INSERT INTO cgit_sim.pklog (n) VALUES (x); RETURN x; END; END",
		"CREATE TABLE cgit_sim.quoted (`id\\` INT PRIMARY KEY, v VARCHAR(10)) ENGINE=InnoDB",
		"INSERT INTO cgit_sim.quoted VALUES (1, 1)",
		"CREATE FUNCTION cgit_sim.logit() RETURNS INT MODIFIES SQL DATA BEGIN INSERT INTO cgit_sim.pklog (n) VALUES (7); RETURN 1; END",
	} {
		must(m.Exec(ctx, q))
	}
	for _, login := range []string{in.Login, ""} {
		for _, q := range []string{
			"UPDATE orders, `123` SET orders.total = orders.total, `123`.v = 9",
			"UPDATE orders, cgit_sim.123 SET orders.total = orders.total, cgit_sim.123.v = 9",
			"UPDATE orders, `" + cyr + "` SET orders.total = orders.total, `" + cyr + "`.v = 9",
			"UPDATE orders, `İx` SET orders.total = orders.total, `İx`.v = 9",
			"UPDATE orders SET total = pk.f(total)",
		} {
			if res := sim(login, q); res.Refused == "" {
				t.Errorf("%q (%q) was not refused: %+v", q, login, res)
			}
		}
		for _, tbl := range []string{"`123`", "`" + cyr + "`", "`İx`"} {
			if got := one("SELECT GROUP_CONCAT(v) FROM cgit_sim." + tbl); got != "1" {
				t.Errorf("%s changed (%q): %s", tbl, login, got)
			}
		}
		if got := one("SELECT COUNT(*) FROM cgit_sim.pklog"); got != "0" {
			t.Errorf("pklog filled (%q): %s", login, got)
		}
	}
	must(m.Exec(ctx, "TRUNCATE cgit_sim.pklog"))
	mode := one("SELECT @@GLOBAL.sql_mode")
	must(m.Exec(ctx, "SET GLOBAL sql_mode = CONCAT(@@GLOBAL.sql_mode, ',ANSI_QUOTES')"))
	t.Cleanup(func() { m.Exec(ctx, "SET GLOBAL sql_mode = ?", mode) })
	if res := sim(in.Login, `UPDATE quoted SET v = 'q\'' WHERE "id\" = 0 OR logit() -- " -- '`); res.Refused == "" {
		t.Errorf("a function hidden by the ANSI_QUOTES reading was not refused: %+v", res)
	}
	// A session that reads strings otherwise is refused outright, even
	// for a plain statement; ANSI (which the server expands) too.
	for _, add := range []string{"ANSI_QUOTES", "NO_BACKSLASH_ESCAPES", "ANSI"} {
		must(m.Exec(ctx, "SET GLOBAL sql_mode = CONCAT(?, ',', ?)", mode, add))
		want := add
		if add == "ANSI" {
			want = "ANSI_QUOTES"
		}
		if res := sim(in.Login, "UPDATE orders SET total = total"); !strings.Contains(res.Refused, want) {
			t.Errorf("a plain simulation under %s: %+v", add, res)
		}
	}
	must(m.Exec(ctx, "SET GLOBAL sql_mode = ?", mode))
	if got := one("SELECT COUNT(*) FROM cgit_sim.pklog"); got != "0" {
		t.Errorf("pklog filled under ANSI_QUOTES: %s", got)
	}

	// More rows than the preview shows: the preview stops at 25 without
	// dropping the connection the transaction is on.
	must(m.Exec(ctx, "CREATE TABLE cgit_sim.many (id INT PRIMARY KEY, v INT) ENGINE=InnoDB"))
	must(m.Exec(ctx, "INSERT INTO cgit_sim.many SELECT seq, seq FROM cgit_sim.seq_1_to_40"))
	for _, login := range []string{in.Login, ""} {
		res, err := o.Simulate(ctx, rpc.DBStudioSimulateParams{Site: site, Login: login, Database: map[bool]string{true: "cgit_sim"}[login != ""], SQL: "UPDATE many SET v = v + 1"})
		if err != nil || res.Affected != 40 || !res.RolledBack || res.Preview == nil || len(res.Preview.Rows) != 25 || !res.Preview.Truncated {
			t.Errorf("a preview of 40 rows (%q): %+v, %v", login, res, err)
		}
	}
	if got := one("SELECT SUM(v) FROM cgit_sim.many"); got != "820" {
		t.Errorf("many changed: %s", got)
	}

	// A row the live site holds locked: the simulation gives up within
	// seconds (innodb_lock_wait_timeout 5), not the server's 50.
	live, err := sql.Open("mysql", "root@unix("+socket+")/cgit_sim")
	must(err)
	defer live.Close()
	tx, err := live.BeginTx(ctx, nil)
	must(err)
	_, err = tx.ExecContext(ctx, "UPDATE orders SET total = total WHERE id = 3")
	must(err)
	began := time.Now()
	res := sim(in.Login, "UPDATE orders SET total = 0 WHERE id >= 2")
	took := time.Since(began)
	must(tx.Rollback())
	if !strings.Contains(res.Message, "1205") || !res.RolledBack || took > 15*time.Second {
		t.Errorf("a simulation behind a held lock: %+v after %s", res, took)
	}

	// The allowlists take what the server takes: built-ins run, the
	// others are refused before a connection.
	if res := sim(in.Login, "UPDATE orders SET status = CONCAT(UPPER(status), DATE_FORMAT(NOW(), '%Y')) WHERE total > (SELECT COALESCE(MIN(total), 0) FROM orders)"); res.Refused != "" || res.Message != "" || res.Affected != 4 {
		t.Errorf("built-ins: %+v", res)
	}
	if res := sim(in.Login, "INSERT INTO archive (id, status, total) VALUES (99, CAST(42 AS CHAR(5)), 1) ON DUPLICATE KEY UPDATE total = VALUES(total)"); res.Refused != "" || res.Message != "" || res.Affected != 1 {
		t.Errorf("INSERT … ON DUPLICATE KEY UPDATE: %+v", res)
	}
	for _, q := range []string{"UPDATE orders SET total = SLEEP(1)", "UPDATE orders SET total = GET_LOCK('cg', 1)", "UPDATE orders SET total = (SELECT total FROM archive FOR UPDATE)"} {
		if res := sim(in.Login, q); res.Refused == "" {
			t.Errorf("%q was not refused: %+v", q, res)
		}
	}

	// One budget for the preview and the statement: with 9 s, a preview
	// that would run for minutes is stopped by the server at 4 s, and the
	// statement, with under a second left, does not run. (Before, each had
	// its own 55 s.)
	func() {
		defer func(b time.Duration) { simBudget = b }(simBudget)
		simBudget = 9 * time.Second
		began := time.Now()
		res := sim(in.Login, "UPDATE many SET v = 0 WHERE (SELECT COUNT(*) FROM many a, many b, many c, many d, many e, many f) > 0")
		took := time.Since(began)
		if took > 8*time.Second || !res.RolledBack || !strings.Contains(res.Message, "used up") {
			t.Errorf("a simulation over its budget: %+v after %s", res, took)
		}
	}()
	if got := one("SELECT SUM(v) FROM cgit_sim.many"); got != "820" {
		t.Errorf("many changed: %s", got)
	}

	// A user granted SELECT only: the server refuses the write, and the
	// simulation says so, rolled back.
	const rpw = "simulate-reader-pw-1"
	t.Cleanup(func() { m.Exec(ctx, "DROP USER IF EXISTS 'cgit_simr'@'localhost'") })
	_, err = dbo.CreateUser(ctx, rpc.DBUserParams{Site: site, User: "cgit_simr", Password: rpw, Grants: []state.DBGrant{{Database: "cgit_sim", Level: "read"}}})
	must(err)
	rin, err := o.SignIn(ctx, rpc.DBStudioOpenParams{Site: site, User: "cgit_simr", Password: rpw})
	must(err)
	if res := sim(rin.Login, "UPDATE orders SET total = 0"); !strings.Contains(res.Message, "1142") || !res.RolledBack {
		t.Errorf("a SELECT-only user's simulation: %+v", res)
	}

	// RETURNING: the statement's own count, not the rows it returned (0).
	res = sim(in.Login, "DELETE FROM orders WHERE total >= 20 RETURNING id")
	if res.Refused != "" || res.Affected != 4 || !res.RolledBack {
		t.Errorf("DELETE … RETURNING: %+v", res)
	}
	res = sim(in.Login, "INSERT INTO archive SELECT * FROM orders WHERE status <> 'void' RETURNING id")
	if res.Refused != "" || res.Affected != 4 || !res.RolledBack {
		t.Errorf("INSERT … RETURNING: %+v", res)
	}
	if got := one("SELECT GROUP_CONCAT(CONCAT(id, ':', status, ':', total) ORDER BY id) FROM cgit_sim.orders"); got != before {
		t.Fatalf("orders changed: %s, was %s", got, before)
	}
}
