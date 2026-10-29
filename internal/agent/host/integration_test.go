//go:build hostintegration

// These tests drive the real tools and change the system they run on. They
// are for a throwaway container only:
//
//	accounts: any Debian/Ubuntu image as root
//	mariadb:  inside a MariaDB container, as root (unix_socket auth), with
//	          CG_TEST_MYSQL_SOCKET set to the server's socket
//
// go test -tags hostintegration -run Integration ./internal/agent/host/
package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
}

func TestIntegrationAccounts(t *testing.T) {
	requireRoot(t)
	s := NewAccounts(OSExec{})
	ctx := context.Background()
	const u, g = "cgit-user", "cgit-group"
	t.Cleanup(func() { s.DeleteUser(ctx, u, false); s.DeleteGroup(ctx, g) })

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.EnsureGroup(ctx, g, true))
	must(s.EnsureGroup(ctx, g, true))
	if err := s.CreateGroup(ctx, g, true); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("CreateGroup again: %v", err)
	}
	must(s.CreateUser(ctx, UserSpec{Name: u, System: true, Shell: "/usr/sbin/nologin"}))
	if err := s.CreateUser(ctx, UserSpec{Name: u}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("CreateUser again: %v", err)
	}
	if err := s.CreateUser(ctx, UserSpec{Name: "cgit-other", Groups: []string{"cgit-nogroup"}}); !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("CreateUser with missing group: %v", err)
	}
	if err := s.RemoveFromGroup(ctx, u, g); !errors.Is(err, ErrNotMember) {
		t.Fatalf("RemoveFromGroup non-member: %v", err)
	}
	must(s.AddToGroup(ctx, u, g))
	must(s.AddToGroup(ctx, u, g))
	if ok, err := s.IsMember(ctx, u, g); !ok || err != nil {
		t.Fatalf("IsMember: %v %v", ok, err)
	}
	must(s.RemoveFromGroup(ctx, u, g))
	if err := s.RemoveFromGroup(ctx, "cgit-ghost", g); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("RemoveFromGroup ghost: %v", err)
	}
	if err := s.AddToGroup(ctx, u, "cgit-nogroup"); !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("AddToGroup missing group: %v", err)
	}
	must(s.SetPassword(ctx, u, "Integration:pass-1"))
	if err := s.SetPassword(ctx, "cgit-ghost", "Integration-pass-1"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("SetPassword ghost: %v", err)
	}
	must(s.LockUser(ctx, u))
	must(s.ExpireUser(ctx, u, true))
	must(s.ExpireUser(ctx, u, false))
	if err := s.LockUser(ctx, "cgit-ghost"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("LockUser ghost: %v", err)
	}
	if err := s.DeleteGroup(ctx, u); !errors.Is(err, ErrPrimaryGroup) {
		t.Fatalf("DeleteGroup primary: %v", err)
	}
	must(s.DeleteUser(ctx, u, false))
	if err := s.DeleteUser(ctx, u, false); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("DeleteUser again: %v", err)
	}
	must(s.DeleteGroup(ctx, g))
	if err := s.DeleteGroup(ctx, g); !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("DeleteGroup again: %v", err)
	}
}

func TestIntegrationMariaDB(t *testing.T) {
	requireRoot(t)
	socket := os.Getenv("CG_TEST_MYSQL_SOCKET")
	if socket == "" {
		t.Skip("CG_TEST_MYSQL_SOCKET not set")
	}
	m := NewMariaDB(socket, t.TempDir(), OSExec{})
	defer m.Close()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Root over the socket, no password: unix_socket auth through the driver.
	must(m.Exec(ctx, "CREATE DATABASE IF NOT EXISTS cgit_a"))
	must(m.Exec(ctx, "CREATE DATABASE IF NOT EXISTS cgit_b"))
	must(m.Exec(ctx, "CREATE TABLE IF NOT EXISTS cgit_b.secret (v TEXT)"))
	must(m.Exec(ctx, "INSERT INTO cgit_b.secret VALUES (?)", "tenant-b-data"))

	var me *mysql.MySQLError
	if err := m.Exec(ctx, "SELECT * FROM cgit_nope.t"); !errors.As(err, &me) || me.Number != 1146 && me.Number != 1049 {
		t.Fatalf("driver error not typed: %v", err)
	}

	// A scoped account sees its database and nothing else.
	acct, err := m.ScopedAccount(ctx, "cgit_a", true)
	must(err)
	st, err := os.Stat(acct.DefaultsFile)
	must(err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("defaults file mode %v", st.Mode())
	}
	users, err := m.QueryStrings(ctx, "SELECT CONCAT(\"'\", User, \"'@'\", Host, \"'\") FROM mysql.user WHERE User = ?", acct.User)
	must(err)
	if len(users) != 1 || !ScopedAccountRe.MatchString(users[0]) {
		t.Fatalf("account %v does not match the sweep pattern", users)
	}
	must(acct.Close())
	must(acct.Close())
	if _, err := os.Stat(acct.DefaultsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("defaults file left behind")
	}
	users, _ = m.QueryStrings(ctx, "SELECT User FROM mysql.user WHERE User = ?", acct.User)
	if len(users) != 0 {
		t.Fatal("scoped account not dropped")
	}

	// Import: plain SQL goes in.
	_, impErr := m.Import(ctx, "cgit_a", strings.NewReader("CREATE TABLE t (v INT); INSERT INTO t VALUES (7);"), ImportAs{})
	must(impErr)
	rows, err := m.QueryStrings(ctx, "SELECT v FROM cgit_a.t")
	must(err)
	if len(rows) != 1 || rows[0] != "7" {
		t.Fatalf("imported rows %v", rows)
	}
	// R-0.1: a client command is refused by --sandbox, and nothing runs.
	marker := t.TempDir() + "/pwned"
	_, err = m.Import(ctx, "cgit_a", strings.NewReader("system touch "+marker+"\n"), ImportAs{})
	if err == nil {
		t.Fatal("--sandbox let a client command through")
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the client command ran")
	}
	// SR-T1: LOAD DATA LOCAL INFILE in a dump reads no file of this
	// machine, whatever the server's local_infile.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	must(os.WriteFile(secret, []byte("root-only-secret\n"), 0o600))
	must(m.Exec(ctx, "SET GLOBAL local_infile = 1"))
	_, err = m.Import(ctx, "cgit_a", strings.NewReader("CREATE TABLE loot (v TEXT); LOAD DATA LOCAL INFILE '"+secret+"' INTO TABLE loot;"), ImportAs{})
	must(m.Exec(ctx, "SET GLOBAL local_infile = 0"))
	if err == nil {
		t.Fatal("LOAD DATA LOCAL INFILE was imported")
	}
	if loot, _ := m.QueryStrings(ctx, "SELECT v FROM cgit_a.loot"); len(loot) != 0 {
		t.Fatalf("the client sent a local file: %v", loot)
	}
	// The scoped account cannot reach another tenant's database.
	if _, err := m.Import(ctx, "cgit_a", strings.NewReader("INSERT INTO cgit_a.t SELECT LENGTH(v) FROM cgit_b.secret;"), ImportAs{}); err == nil {
		t.Fatal("import read another database")
	}
	left, _ := m.QueryStrings(ctx, "SELECT User FROM mysql.user WHERE User LIKE 'cgq\\_%'")
	if len(left) != 0 {
		t.Fatalf("imports left accounts: %v", left)
	}

	// Dump streams the database.
	var buf bytes.Buffer
	must(m.Dump(ctx, "cgit_a", &buf))
	if !strings.Contains(buf.String(), "INSERT INTO `t`") {
		t.Fatalf("dump has no data: %.200s", buf.String())
	}
}
