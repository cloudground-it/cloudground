package db

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/tune"
)

// SR-T1: at start the running server's local_infile goes off, and a tuning
// file an older version wrote gets the security settings, once.
func TestHardenOldTuningFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := "[mysqld]\ninnodb_buffer_pool_size = 1024M\ninnodb_flush_log_at_trx_commit = 1\n"
	if err := os.WriteFile(f.o.TuneFile, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	restart, err := f.o.Harden(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.o.TuneFile)
	if !strings.HasPrefix(string(b), old) || !strings.Contains(string(b), "\nlocal_infile = 0\n") ||
		!strings.Contains(string(b), "\nskip-symbolic-links\n") {
		t.Fatalf("tuning file after Harden:\n%s", b)
	}
	// SR-T3: symbolic links are read at start only: the server restarts,
	// once, when the caller runs restart.
	sd := f.o.Systemd.(*host.FakeSystemd)
	if restart == nil || len(sd.Ops()) != 0 {
		t.Fatalf("restart %v, systemd %v", restart != nil, sd.Ops())
	}
	restart()
	if strings.Join(sd.Ops(), ",") != "restart mariadb.service" {
		t.Fatalf("systemd %v", sd.Ops())
	}
	if !strings.Contains(strings.Join(f.m.Statements(), "|"), "SET GLOBAL local_infile = 0") {
		t.Fatalf("the running server was not changed: %v", f.m.Statements())
	}
	// Idempotent: a second start adds nothing and restarts nothing.
	if restart, err := f.o.Harden(ctx); err != nil || restart != nil {
		t.Fatal(err, restart != nil)
	}
	again, _ := os.ReadFile(f.o.TuneFile)
	if string(again) != string(b) {
		t.Fatalf("a second Harden changed the file:\n%s", again)
	}
	// The values of the old tuning are kept as they were.
	if configValue(string(again), "innodb_buffer_pool_size") != "1024M" {
		t.Fatal("the tuning changed")
	}
}

// SR-T1: a session's driver connection never lets the server read a file
// of this machine through LOAD DATA LOCAL INFILE.
func TestSessionConfigAllowsNoLocalFile(t *testing.T) {
	cfg := scopedConfig("/run/mysqld/mysqld.sock", DialSpec{Account: &host.ScopedAccount{User: "cgq_0123456789abcdef", Password: "x"}, Database: "site_3"})
	if cfg.AllowAllFiles {
		t.Fatal("a session allows every local file")
	}
	if dsn := cfg.FormatDSN(); strings.Contains(strings.ToLower(dsn), "allowallfiles") {
		t.Fatalf("the session DSN enables local files: %s", dsn)
	}
}

// No tuning file yet: nothing is written (the first tuning has them).
func TestHardenUntunedServer(t *testing.T) {
	f := newFixture(t)
	if restart, err := f.o.Harden(context.Background()); err != nil || restart != nil {
		t.Fatal(err, restart != nil)
	}
	if _, err := os.Stat(f.o.TuneFile); !os.IsNotExist(err) {
		t.Fatal("Harden wrote a tuning file")
	}
	if _, err := f.o.CreateDB(context.Background(), rpc.SiteDatabaseParams{Site: f.site, Name: "site_3"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.o.TuneFile)
	if tune.HardeningBlock(string(b)) != "" {
		t.Fatalf("the first tuning lacks the security settings:\n%s", b)
	}
}

// SR-T2: at start a site account still granted on its database's name as a
// pattern gets the escaped grant first, then loses the pattern; an escaped
// grant (or another account's) is left alone.
func TestHardenRegrantsPatternGrants(t *testing.T) {
	f := newFixture(t)
	f.m.Rows["SELECT CONCAT(User, ' ', Db) FROM mysql.db WHERE Host = 'localhost' AND User = Db"] = []string{"site_3 site_3", "site_9 site_9", "plain plain"}
	if _, err := f.o.Harden(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.m.Statements(), "\n")
	want := strings.Join([]string{
		"SET GLOBAL local_infile = 0",
		"GRANT ALL PRIVILEGES ON `site\\_3`.* TO ?@'localhost' [site_3]",
		"REVOKE ALL PRIVILEGES ON `site_3`.* FROM ?@'localhost' [site_3]",
		"GRANT ALL PRIVILEGES ON `site\\_9`.* TO ?@'localhost' [site_9]",
		"REVOKE ALL PRIVILEGES ON `site_9`.* FROM ?@'localhost' [site_9]",
	}, "\n")
	if got != want {
		t.Fatalf("statements:\n%s\nwant:\n%s", got, want)
	}
}
