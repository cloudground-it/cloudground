//go:build hostintegration

// The security review's tenant-data fixes against a real MariaDB (see
// integration_test.go for how to run it).
package db

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

type secIT struct {
	t      *testing.T
	ctx    context.Context
	m      *host.MariaDBAdmin
	o      *Ops
	socket string
}

func newSecIT(t *testing.T) *secIT {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	socket := os.Getenv("CG_TEST_MYSQL_SOCKET")
	if socket == "" {
		t.Skip("CG_TEST_MYSQL_SOCKET not set")
	}
	work := t.TempDir()
	m := host.NewMariaDB(socket, work, host.OSExec{Timeout: time.Minute})
	t.Cleanup(func() { m.Close() })
	tuned := filepath.Join(t.TempDir(), "tune.cnf")
	os.WriteFile(tuned, []byte("[mysqld]\ninnodb_flush_log_at_trx_commit = 1\n"), 0o644)
	o := &Ops{MariaDB: m, Socket: socket, WorkDir: work, TuneFile: tuned, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &secIT{t: t, ctx: context.Background(), m: m, o: o, socket: socket}
}

func (s *secIT) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *secIT) rows(q string, args ...any) []string {
	s.t.Helper()
	r, err := s.m.QueryStrings(s.ctx, q, args...)
	s.must(err)
	return r
}

// as opens a connection as user with password.
func (s *secIT) as(user, password, database string) *sql.DB {
	s.t.Helper()
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = user, password, "unix", s.socket, database
	c, err := mysql.NewConnector(cfg)
	s.must(err)
	db := sql.OpenDB(c)
	s.t.Cleanup(func() { db.Close() })
	return db
}

const secPassword = "it-site-password-0123456789"

// site makes a site database and account (site_<id>) as a new site's are
// made (ProvisionSite).
func (s *secIT) site(name string) {
	s.t.Helper()
	id, err := strconv.ParseInt(strings.TrimPrefix(name, "site_"), 10, 64)
	s.must(err)
	st := state.Site{ID: id, Domain: name + ".example.com", Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: name, User: name}}}
	s.t.Cleanup(func() { s.o.DropSite(context.Background(), id) })
	s.must(s.o.ProvisionSite(s.ctx, st, secPassword))
}

// SR-T2: the site's account reaches its own database only, not one whose
// name its own matches as a LIKE pattern, and creates none; an account
// granted the old way is moved to the escaped grant at start.
func TestIntegrationGrantIsOnOneDatabase(t *testing.T) {
	s := newSecIT(t)
	s.site("site_9001")
	for _, d := range []string{"siteA9001", "site09001"} {
		d := d
		s.must(s.m.Exec(s.ctx, "CREATE DATABASE IF NOT EXISTS `"+d+"`"))
		t.Cleanup(func() { s.m.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+d+"`") })
		s.must(s.m.Exec(s.ctx, "CREATE TABLE IF NOT EXISTS `"+d+"`.secret (v TEXT)"))
	}
	db := s.as("site_9001", secPassword, "site_9001")
	if _, err := db.ExecContext(s.ctx, "CREATE TABLE own (i INT)"); err != nil {
		t.Fatalf("the site cannot use its own database: %v", err)
	}
	if _, err := db.ExecContext(s.ctx, "SELECT * FROM siteA9001.secret"); err == nil {
		t.Fatal("the site's account reads siteA9001")
	}
	if _, err := db.ExecContext(s.ctx, "CREATE DATABASE siteB9001"); err == nil {
		s.m.Exec(s.ctx, "DROP DATABASE siteB9001")
		t.Fatal("the site's account created siteB9001")
	}

	// The old grant, as Create wrote it before.
	s.must(s.m.Exec(s.ctx, "REVOKE ALL PRIVILEGES ON `site\\_9001`.* FROM 'site_9001'@'localhost'"))
	s.must(s.m.Exec(s.ctx, "GRANT ALL PRIVILEGES ON `site_9001`.* TO 'site_9001'@'localhost'"))
	if _, err := db.ExecContext(s.ctx, "SELECT * FROM site09001.secret"); err != nil {
		t.Fatalf("control: the pattern grant does not reach site09001: %v", err)
	}
	for range 2 {
		_, err := s.o.Harden(s.ctx)
		s.must(err)
	}
	if got := s.rows("SELECT Db FROM mysql.db WHERE User = 'site_9001' AND Host = 'localhost'"); strings.Join(got, ",") != `site\_9001` {
		t.Fatalf("grants after Harden: %v", got)
	}
	fresh := s.as("site_9001", secPassword, "site_9001")
	if _, err := fresh.ExecContext(s.ctx, "SELECT * FROM site09001.secret"); err == nil {
		t.Fatal("after Harden the account still reads site09001")
	}
	if _, err := fresh.ExecContext(s.ctx, "INSERT INTO own VALUES (1)"); err != nil {
		t.Fatalf("after Harden the site lost its database: %v", err)
	}
}

// SR-T3: with symbolic links off (the tuning file's skip-symbolic-links),
// DATA DIRECTORY is ignored: the table's file stays in the database's
// directory. Run with the server started with --skip-symbolic-links.
func TestIntegrationNoDataDirectory(t *testing.T) {
	s := newSecIT(t)
	if got := s.rows("SELECT @@have_symlink"); got[0] != "DISABLED" {
		t.Skipf("the server runs with have_symlink = %s", got[0])
	}
	s.site("site_9002")
	dir := t.TempDir()
	s.must(os.Chmod(dir, 0o777))
	db := s.as("site_9002", secPassword, "site_9002")
	if _, err := db.ExecContext(s.ctx, "CREATE TABLE dd (i INT) ENGINE=InnoDB DATA DIRECTORY='"+dir+"'"); err != nil {
		t.Logf("refused: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the table's files went to %s: %v", dir, entries)
	}
}

// SR-T7: a dump with a trigger, a view and a routine of a foreign (and
// privileged) DEFINER imports as the site's own account: every object is
// the site's, runs, and no DEFINER of the dump is kept. The site's password
// keeps working and the import's own is gone; a borrow cut short is undone
// by the sweep. The restore test verifies such a dump too.
func TestIntegrationDefinersBecomeTheSites(t *testing.T) {
	s := newSecIT(t)
	s.site("site_9004")
	dump := filepath.Join(t.TempDir(), "definers.sql")
	s.must(os.WriteFile(dump, []byte("CREATE TABLE wp_options (i INT);\nCREATE TABLE audit (i INT);\n"+
		"DELIMITER ;;\n"+
		"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER trg BEFORE INSERT ON wp_options FOR EACH ROW BEGIN SET NEW.i = NEW.i + 1; INSERT INTO audit VALUES (NEW.i); END */;;\n"+
		"DELIMITER ;\n"+
		"/*!50001 CREATE ALGORITHM=UNDEFINED */\n/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n/*!50001 VIEW `v` AS select `i` from `wp_options` */;\n"+
		"CREATE DEFINER='mariadb.sys'@'localhost' PROCEDURE p() SELECT COUNT(*) FROM audit;\n"+
		"INSERT INTO wp_options VALUES (1);\n"), 0o600))
	tx, err := s.o.Replace(s.ctx, "site_9004", dump, Over)
	s.must(err)
	tx.Commit()
	definers := s.rows("SELECT DEFINER FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = 'site_9004' UNION ALL " +
		"SELECT DEFINER FROM information_schema.VIEWS WHERE TABLE_SCHEMA = 'site_9004' UNION ALL " +
		"SELECT DEFINER FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = 'site_9004'")
	if strings.Join(definers, ",") != "site_9004@localhost,site_9004@localhost,site_9004@localhost" {
		t.Fatalf("definers %v", definers)
	}
	db := s.as("site_9004", secPassword, "site_9004")
	if _, err := db.ExecContext(s.ctx, "INSERT INTO wp_options VALUES (10)"); err != nil {
		t.Fatalf("the trigger does not run as the site: %v", err)
	}
	var n int
	s.must(db.QueryRowContext(s.ctx, "SELECT COUNT(*) FROM v").Scan(&n))
	if got := s.rows("SELECT GROUP_CONCAT(i ORDER BY i) FROM site_9004.audit"); n != 2 || got[0] != "2,11" {
		t.Fatalf("view %d rows, audit %v", n, got)
	}
	if got := s.rows("SELECT COALESCE(JSON_LENGTH(Priv, '$.auth_or'), 0) FROM mysql.global_priv WHERE User = 'site_9004'"); got[0] > "1" {
		t.Fatalf("the import's password is still there: auth_or %v", got)
	}
	// A borrow cut short: the sweep puts the site's own authentication back.
	hash := s.rows("SELECT JSON_VALUE(Priv, '$.authentication_string') FROM mysql.global_priv WHERE User = 'site_9004'")[0]
	s.must(s.m.Exec(s.ctx, "ALTER USER 'site_9004'@'localhost' IDENTIFIED VIA mysql_native_password USING ? OR mysql_native_password USING PASSWORD('left-behind-0123')", hash))
	s.must(s.o.Sweep(s.ctx))
	if got := s.rows("SELECT COALESCE(JSON_LENGTH(Priv, '$.auth_or'), 0) FROM mysql.global_priv WHERE User = 'site_9004'"); got[0] > "1" {
		t.Fatalf("the sweep left the second password: auth_or %v", got)
	}
	if err := s.as("site_9004", "left-behind-0123", "site_9004").PingContext(s.ctx); err == nil {
		t.Fatal("the left-behind password still signs in")
	}
	if err := s.as("site_9004", secPassword, "site_9004").PingContext(s.ctx); err != nil {
		t.Fatalf("the site's own password stopped working: %v", err)
	}
	// The restore test verifies a dump with triggers.
	snap := filepath.Join(t.TempDir(), "snap.sql")
	s.must(s.o.DumpFile(s.ctx, "site_9004", snap))
	if got, err := s.o.VerifyDump(s.ctx, snap, true, 3); err != nil || got != 3 { // two tables and the view
		t.Fatalf("verify %d %v", got, err)
	}
	// A restore of that snapshot (fresh) works too.
	tx, err = s.o.Replace(s.ctx, "site_9004", snap, Fresh)
	s.must(err)
	tx.Commit()
}
