package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sqlLog struct{ stmts []string }

func (l *sqlLog) exec(fail string) func(context.Context, string, ...any) error {
	return func(_ context.Context, q string, args ...any) error {
		l.stmts = append(l.stmts, fmt.Sprintf("%s %v", q, args))
		if fail != "" && strings.HasPrefix(q, fail) {
			return errors.New("refused")
		}
		return nil
	}
}

func TestScopedAccountGrantsAndCleanup(t *testing.T) {
	for _, ro := range []bool{true, false} {
		log := &sqlLog{}
		dir := t.TempDir()
		m := &MariaDBAdmin{WorkDir: dir, execFn: log.exec("")}
		acct, err := m.ScopedAccount(context.Background(), "site_db", ro)
		if err != nil {
			t.Fatal(err)
		}
		if !ScopedAccountRe.MatchString("'" + acct.User + "'@'localhost'") {
			t.Fatalf("user %q", acct.User)
		}
		if len(acct.Password) != 32 || strings.ContainsAny(acct.Password, "'\"\\") {
			t.Fatalf("password %q", acct.Password)
		}
		create := log.stmts[0]
		if !strings.HasPrefix(create, "CREATE USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 4 [") ||
			!strings.Contains(create, acct.Password) {
			t.Fatalf("create %q", create)
		}
		grant := log.stmts[1]
		priv := scopedWritePrivileges
		if ro {
			priv = "SELECT, SHOW VIEW"
		}
		if !strings.HasPrefix(grant, "GRANT "+priv+" ON `site\\_db`.* TO ?@'localhost'") {
			t.Fatalf("grant %q", grant)
		}
		for _, forbidden := range []string{"TRIGGER", "EVENT", "ROUTINE", "ALL", "FILE", "SUPER"} {
			if strings.Contains(grant, forbidden) {
				t.Fatalf("grant includes %s: %q", forbidden, grant)
			}
		}
		st, err := os.Stat(acct.DefaultsFile)
		if err != nil || st.Mode().Perm() != 0o600 || filepath.Dir(acct.DefaultsFile) != dir {
			t.Fatalf("defaults file %v %v", st, err)
		}
		b, _ := os.ReadFile(acct.DefaultsFile)
		if string(b) != "[client]\nuser="+acct.User+"\npassword="+acct.Password+"\ndefault-character-set=utf8mb4\n" {
			t.Fatalf("defaults file %q", b)
		}
		if err := acct.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(acct.DefaultsFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("defaults file left behind")
		}
		if last := log.stmts[len(log.stmts)-1]; !strings.HasPrefix(last, "DROP USER IF EXISTS ?@'localhost' ["+acct.User) {
			t.Fatalf("drop %q", last)
		}
	}
}

// A failed GRANT drops the account it just created.
func TestScopedAccountGrantFailureDrops(t *testing.T) {
	log := &sqlLog{}
	m := &MariaDBAdmin{WorkDir: t.TempDir(), execFn: log.exec("GRANT")}
	if _, err := m.ScopedAccount(context.Background(), "site_db", false); err == nil {
		t.Fatal("no error")
	}
	if len(log.stmts) != 3 || !strings.HasPrefix(log.stmts[2], "DROP USER") {
		t.Fatalf("stmts %v", log.stmts)
	}
}

// The drop runs even when the request's context is already cancelled.
func TestScopedAccountDropsAfterCancel(t *testing.T) {
	var dropCtxErr error = errors.New("not called")
	m := &MariaDBAdmin{WorkDir: t.TempDir(), execFn: func(ctx context.Context, q string, _ ...any) error {
		if strings.HasPrefix(q, "DROP") {
			dropCtxErr = ctx.Err()
		}
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	acct, err := m.ScopedAccount(ctx, "site_db", true)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	acct.Close()
	if dropCtxErr != nil {
		t.Fatalf("drop ran with a dead context: %v", dropCtxErr)
	}
}

func TestMariaDBRejectsBadNames(t *testing.T) {
	fx := &FakeExec{}
	m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: fx, execFn: (&sqlLog{}).exec("")}
	ctx := context.Background()
	for _, bad := range []string{"", "a`b", "a.b", "-x", "a b", strings.Repeat("a", 65)} {
		if _, err := m.ScopedAccount(ctx, bad, true); !errors.Is(err, ErrInvalidDatabase) {
			t.Fatalf("%q: %v", bad, err)
		}
		if err := m.Dump(ctx, bad, &bytes.Buffer{}); !errors.Is(err, ErrInvalidDatabase) {
			t.Fatalf("%q: %v", bad, err)
		}
		if err := m.Import(ctx, bad, strings.NewReader(""), ImportAs{}); !errors.Is(err, ErrInvalidDatabase) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if len(fx.Calls()) != 0 {
		t.Fatal("a tool ran")
	}
}

// R-0.1: the import runs the client sandboxed, as a scoped account, on one
// database — exactly these flags — and drops the account even on failure.
func TestImportIsSandboxedAndScoped(t *testing.T) {
	for _, code := range []int{0, 1} {
		log := &sqlLog{}
		fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Code: code} }}
		m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: fx, execFn: log.exec("")}
		err := m.Import(context.Background(), "site_db", strings.NewReader("SELECT 1;"), ImportAs{})
		if (err != nil) != (code != 0) {
			t.Fatalf("code %d: err = %v", code, err)
		}
		c := fx.Calls()[0]
		if c.Name != "mariadb" || len(c.Args) != 6 ||
			!strings.HasPrefix(c.Args[0], "--defaults-file=") ||
			strings.Join(c.Args[1:], " ") != "--sandbox --local-infile=0 --protocol=socket --one-database site_db" {
			t.Fatalf("argv %v", c.Args)
		}
		if c.Stdin != "SELECT 1;" {
			t.Fatalf("stdin %q", c.Stdin)
		}
		if !strings.HasPrefix(log.stmts[1], "GRANT "+scopedWritePrivileges) {
			t.Fatalf("import account is not read-write scoped: %v", log.stmts)
		}
		if !strings.HasPrefix(log.stmts[len(log.stmts)-1], "DROP USER") {
			t.Fatalf("account not dropped (code %d): %v", code, log.stmts)
		}
		file := strings.TrimPrefix(c.Args[0], "--defaults-file=")
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("defaults file left behind")
		}
	}
}

func TestDumpStreams(t *testing.T) {
	fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: "-- dump\n"} }}
	m := &MariaDBAdmin{Tools: fx}
	var buf bytes.Buffer
	if err := m.Dump(context.Background(), "site_db", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "-- dump\n" || fx.Calls()[0].Line() != "mariadb-dump --protocol=socket --single-transaction site_db" {
		t.Fatalf("%q %v", buf.String(), fx.Calls())
	}
}

func TestFakeMariaDB(t *testing.T) {
	f := NewFakeMariaDB()
	ctx := context.Background()
	acct, _ := f.ScopedAccount(ctx, "db1", true)
	if f.OpenAccounts() != 1 {
		t.Fatal("account not tracked")
	}
	acct.Close()
	if err := f.Import(ctx, "db1", strings.NewReader("x"), ImportAs{}); err != nil || f.Imported["db1"] != "x" || f.OpenAccounts() != 0 {
		t.Fatalf("%v %v %d", err, f.Imported, f.OpenAccounts())
	}
}

// Some tables only: they follow the database in argv, and a name that could
// read as an option never reaches mariadb-dump.
func TestDumpTables(t *testing.T) {
	fx := &FakeExec{}
	m := &MariaDBAdmin{Tools: fx}
	if err := m.Dump(context.Background(), "site_db", &bytes.Buffer{}, "wp_posts", "wp_terms"); err != nil {
		t.Fatal(err)
	}
	if got := fx.Calls()[0].Line(); got != "mariadb-dump --protocol=socket --single-transaction site_db wp_posts wp_terms" {
		t.Fatalf("argv %q", got)
	}
	for _, bad := range []string{"--result-file=/etc/x", "-r", "a b", "", "t`"} {
		if err := m.Dump(context.Background(), "site_db", &bytes.Buffer{}, bad); err == nil {
			t.Fatalf("table %q accepted", bad)
		}
	}
	if len(fx.Calls()) != 1 {
		t.Fatal("a refused dump ran")
	}
}

// A dump or an import moves a whole database: the runner's short default
// (the agent's is a minute) must not kill it; the caller's context does.
func TestDumpAndImportAreNotCutShort(t *testing.T) {
	fx := &FakeExec{}
	m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: fx, execFn: (&sqlLog{}).exec("")}
	ctx := context.Background()
	if err := m.Dump(ctx, "site_db", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(ctx, "site_db", strings.NewReader("SELECT 1;"), ImportAs{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Calls() {
		if c.Timeout != DataTimeout {
			t.Fatalf("%s runs with timeout %v, want %v", c.Name, c.Timeout, DataTimeout)
		}
	}
}

// SR-T1: the root pool never lets the server read a file of this machine
// through LOAD DATA LOCAL INFILE.
func TestRootConfigAllowsNoLocalFile(t *testing.T) {
	cfg := RootConfig("/run/mysqld/mysqld.sock")
	if cfg.AllowAllFiles {
		t.Fatal("the root pool allows every local file")
	}
	if dsn := cfg.FormatDSN(); strings.Contains(strings.ToLower(dsn), "allowallfiles") {
		t.Fatalf("the root DSN enables local files: %s", dsn)
	}
}

// SR-T2: in a GRANT, `_` and `%` are wildcards; the pattern is escaped so a
// grant is on one database.
func TestGrantPattern(t *testing.T) {
	for in, want := range map[string]string{
		"site_1":   `site\_1`,
		"site_12":  `site\_12`,
		"cgv_ab_c": `cgv\_ab\_c`,
		"plain":    "plain",
		`a%b\c`:    `a\%b\\c`,
	} {
		if got := GrantPattern(in); got != want {
			t.Errorf("GrantPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

// SR-R1: the server's own schemas are never a database the adapter dumps,
// imports or grants.
func TestReservedDatabasesRefused(t *testing.T) {
	m := &MariaDBAdmin{WorkDir: t.TempDir(), execFn: (&sqlLog{}).exec("")}
	for _, name := range []string{"mysql", "MySQL", "sys", "performance_schema", "information_schema"} {
		if ValidDatabaseName(name) {
			t.Errorf("%q is valid", name)
		}
		if _, err := m.ScopedAccount(context.Background(), name, true); !errors.Is(err, ErrInvalidDatabase) {
			t.Errorf("ScopedAccount(%q): %v", name, err)
		}
	}
	if !ValidDatabaseName("site_1") {
		t.Error("site_1 is refused")
	}
}

// SR-T7: an import into a site's database runs as the site's own account:
// a second, random password beside its own for the import, its own
// authentication alone again afterwards (also when the import fails), and
// every DEFINER of the dump naming it.
func TestImportAsTheSiteAccount(t *testing.T) {
	for _, code := range []int{0, 1} {
		log := &sqlLog{}
		fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Code: code} }}
		m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: fx, execFn: log.exec(""),
			queryFn: func(_ context.Context, q string, args ...any) ([]string, error) {
				switch args[0] {
				case "$.plugin":
					return []string{"mysql_native_password"}, nil
				case "$.authentication_string":
					return []string{"*F0CCCA2942EB0AAAF443F83E877E3DB9EDCAD34A"}, nil
				}
				return nil, fmt.Errorf("unexpected %s %v", q, args)
			}}
		dump := "CREATE DEFINER=`root`@`localhost` VIEW v AS SELECT 1;"
		err := m.Import(context.Background(), "site_3", strings.NewReader(dump), ImportAs{Site: "site_3"})
		if (err != nil) != (code != 0) {
			t.Fatalf("code %d: %v", code, err)
		}
		c := fx.Calls()[0]
		if c.Stdin != "CREATE DEFINER=`site_3`@`localhost` VIEW v AS SELECT 1;" {
			t.Fatalf("stdin %q", c.Stdin)
		}
		if len(log.stmts) != 2 ||
			!strings.HasPrefix(log.stmts[0], "ALTER USER ?@'localhost' IDENTIFIED VIA mysql_native_password USING ? OR mysql_native_password USING PASSWORD(?) [site_3 *F0CCCA2942EB0AAAF443F83E877E3DB9EDCAD34A ") ||
			log.stmts[1] != "ALTER USER ?@'localhost' IDENTIFIED VIA mysql_native_password USING ? [site_3 *F0CCCA2942EB0AAAF443F83E877E3DB9EDCAD34A]" {
			t.Fatalf("statements %q", log.stmts)
		}
		file := strings.TrimPrefix(c.Args[0], "--defaults-file=")
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("defaults file left behind")
		}
	}
}

// Only a site's own account is borrowed, and only one whose authentication
// can be put back exactly.
func TestImportAsRefusesOtherAccounts(t *testing.T) {
	for _, user := range []string{"root", "mysql", "cgq_0123456789abcdef", "site_0"} {
		m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: &FakeExec{}, execFn: (&sqlLog{}).exec("")}
		if err := m.Import(context.Background(), "site_3", strings.NewReader("SELECT 1;"), ImportAs{Site: user}); err == nil {
			t.Errorf("borrowed %q", user)
		}
	}
	m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: &FakeExec{}, execFn: (&sqlLog{}).exec(""),
		queryFn: func(_ context.Context, _ string, args ...any) ([]string, error) {
			if args[0] == "$.plugin" {
				return []string{"unix_socket"}, nil
			}
			return []string{""}, nil
		}}
	if err := m.Import(context.Background(), "site_3", strings.NewReader("SELECT 1;"), ImportAs{Site: "site_3"}); err == nil {
		t.Error("borrowed a unix_socket account")
	}
}

// The restore test's scratch import may create triggers; its definers are
// the scoped account itself.
func TestImportScratchWithTriggers(t *testing.T) {
	log := &sqlLog{}
	fx := &FakeExec{}
	m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: fx, execFn: log.exec("")}
	if err := m.Import(context.Background(), "cgv_0123456789abcdef", strings.NewReader("CREATE DEFINER=root@localhost TRIGGER t"), ImportAs{Triggers: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(log.stmts[1], "GRANT "+scopedWritePrivileges+", TRIGGER ON `cgv\\_0123456789abcdef`.*") {
		t.Fatalf("grant %q", log.stmts[1])
	}
	if fx.Calls()[0].Stdin != "CREATE DEFINER=CURRENT_USER TRIGGER t" {
		t.Fatalf("stdin %q", fx.Calls()[0].Stdin)
	}
}
