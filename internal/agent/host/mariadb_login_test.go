package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Quarry's signed-in users (Fase 9b): a dump and an import as the user
// itself, with its own password in a root-only defaults file that is gone
// afterwards. Nothing is created, granted or borrowed on the server.
func TestDumpAndImportAsASignedInUser(t *testing.T) {
	var files []string
	var modes []os.FileMode
	var contents []string
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		file := strings.TrimPrefix(c.Args[0], "--defaults-file=")
		files = append(files, file)
		if fi, err := os.Stat(file); err == nil {
			modes = append(modes, fi.Mode().Perm())
		}
		b, _ := os.ReadFile(file)
		contents = append(contents, string(b))
		return FakeReply{Stdout: "-- dump\n"}
	}}
	log := &sqlLog{}
	m := &MariaDBAdmin{WorkDir: t.TempDir(), Tools: fx, execFn: log.exec("")}
	as := Credentials{User: "shop_reader", Password: "s3cret-Pass-word"}
	var buf bytes.Buffer
	if err := m.DumpAs(context.Background(), as, "shop_data", &buf, "orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Import(context.Background(), "shop_data", strings.NewReader("CREATE DEFINER=`root`@`localhost` VIEW v AS SELECT 1;"), ImportAs{Login: &as}); err != nil {
		t.Fatal(err)
	}
	calls := fx.Calls()
	if got := strings.Join(calls[0].Args[1:], " "); calls[0].Name != "mariadb-dump" || got != "--protocol=socket --single-transaction shop_data orders" {
		t.Fatalf("dump argv %v", calls[0].Args)
	}
	if got := strings.Join(calls[1].Args[1:], " "); calls[1].Name != "mariadb" || got != "--sandbox --local-infile=0 --protocol=socket --one-database shop_data" {
		t.Fatalf("import argv %v", calls[1].Args)
	}
	if !strings.Contains(calls[1].Stdin, "DEFINER=`shop_reader`@`localhost`") || strings.Contains(calls[1].Stdin, "root") {
		t.Fatalf("the definer is not the signed-in user: %q", calls[1].Stdin)
	}
	for i, c := range calls {
		if strings.Contains(strings.Join(c.Args, " "), as.Password) {
			t.Fatalf("call %d carries the password in argv", i)
		}
		if contents[i] != "[client]\nuser=shop_reader\npassword=s3cret-Pass-word\ndefault-character-set=utf8mb4\n" || modes[i] != 0o600 {
			t.Fatalf("defaults file %d: %q mode %v", i, contents[i], modes[i])
		}
		if _, err := os.Stat(files[i]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("defaults file %d left behind", i)
		}
	}
	if len(log.stmts) != 0 {
		t.Fatalf("a signed-in user's dump or import ran root SQL: %v", log.stmts)
	}
	for _, bad := range []Credentials{{User: "root", Password: "x"}, {User: "shop_reader", Password: ""}, {User: "shop_reader", Password: "a\nuser=root"}} {
		if err := m.DumpAs(context.Background(), bad, "shop_data", &buf); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
		if _, err := m.Import(context.Background(), "shop_data", strings.NewReader(""), ImportAs{Login: &bad}); err == nil {
			t.Fatalf("%+v accepted for an import", bad)
		}
	}
	if len(fx.Calls()) != 2 {
		t.Fatal("a refused credential ran a client")
	}
}

// A person's password can hold what an option file reads specially: a
// `#` would start a comment, spaces at the ends would be dropped, a quote
// would end the value.
func TestOptionValue(t *testing.T) {
	for in, want := range map[string]string{
		"s3cret-Pass-word": "s3cret-Pass-word",
		"a#b":              `"a#b"`,
		" padded ":         `" padded "`,
		`say "hi"`:         `"say \"hi\""`,
		`back\slash\`:      `"back\\slash\\"`,
		"it's":             `"it's"`,
	} {
		if got := OptionValue(in); got != want {
			t.Errorf("OptionValue(%q) = %s, want %s", in, got, want)
		}
	}
}

// The client reads the agent's defaults file and no other option file, so
// a [client] user=root in root's ~/.my.cnf or under /etc/mysql cannot turn
// a tenant's import or dump into root's (unix_socket); the file carries the
// socket and the character set the server's own client files would.
func TestClientReadsOnlyTheAgentsDefaultsFile(t *testing.T) {
	var argv [][]string
	var contents []string
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		argv = append(argv, c.Args)
		b, _ := os.ReadFile(strings.TrimPrefix(c.Args[0], "--defaults-file="))
		contents = append(contents, string(b))
		return FakeReply{}
	}}
	log := &sqlLog{}
	m := &MariaDBAdmin{Socket: "/run/mysqld/mysqld.sock", WorkDir: t.TempDir(), Tools: fx, execFn: log.exec("")}
	as := Credentials{User: "shop_reader", Password: "s3cret-Pass-word"}
	if err := m.DumpAs(context.Background(), as, "shop_data", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Import(context.Background(), "shop_data", strings.NewReader("SELECT 1;"), ImportAs{Login: &as}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Import(context.Background(), "shop_data", strings.NewReader("SELECT 1;"), ImportAs{}); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 3 {
		t.Fatalf("%d client runs", len(argv))
	}
	for i, args := range argv {
		if !strings.HasPrefix(args[0], "--defaults-file=") {
			t.Fatalf("run %d: the first argument is %q, not --defaults-file", i, args[0])
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--defaults-extra-file") || strings.HasPrefix(a, "--defaults-group-suffix") {
				t.Fatalf("run %d reads other option files: %v", i, args)
			}
		}
		if !strings.Contains(contents[i], "\nsocket=\"/run/mysqld/mysqld.sock\"\n") || !strings.HasSuffix(contents[i], "\ndefault-character-set=utf8mb4\n") {
			t.Fatalf("run %d: defaults file %q", i, contents[i])
		}
	}
}
