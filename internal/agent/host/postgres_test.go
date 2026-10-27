package host

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestPGNames(t *testing.T) {
	for _, ok := range []string{"shop_data", "cgo_shop_data", "cgv_0123456789abcdef", "a", "a1"} {
		if !ValidPGName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "1a", "Shop", "a-b", "a b", `a"b`, "a;b", "a'b", strings.Repeat("a", 64), "é"} {
		if ValidPGName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := QuotePGIdent(`a"b`); got != `"a""b"` {
		t.Errorf("ident %s", got)
	}
	if got := QuotePGLiteral(`it's`); got != `'it''s'` {
		t.Errorf("literal %s", got)
	}
}

// pg_dump runs in a sandboxed service of a throwaway user, with the password
// and the CA as credential files and in no argument or variable; the files
// are gone after the run.
func TestPGDumpIsSandboxedAndHoldsNoSecretInArgv(t *testing.T) {
	dir := t.TempDir()
	ex := &FakeExec{}
	p := NewPostgres(RemoteServer{ID: 2, Host: "pg.example.com", Port: 5432, TLS: true, CACert: "CA-PEM", AdminUser: "cgadmin", AdminPassword: "s3cret-pw", Kind: "postgres"}, dir, ex)
	var w bytes.Buffer
	if err := p.Dump(context.Background(), "shop_data", &w); err != nil {
		t.Fatal(err)
	}
	calls := ex.Calls()
	if len(calls) != 1 || calls[0].Name != "systemd-run" {
		t.Fatalf("calls %+v", calls)
	}
	line := calls[0].Line()
	for _, want := range []string{"DynamicUser=yes", "ProtectSystem=strict", "ProtectHome=yes", "PrivateTmp=yes", "NoNewPrivileges=yes",
		"CapabilityBoundingSet=", "RestrictAddressFamilies=AF_INET AF_INET6", "LoadCredential=pw:", "LoadCredential=ca:",
		"--setenv=PGHOST=pg.example.com", "--setenv=PGPORT=5432", "--setenv=PGUSER=cgadmin", "--setenv=PGDATABASE=shop_data", "--setenv=PGSSLMODE=verify-full",
		"pg_dump --format=plain --no-owner --no-acl --encoding=UTF8"} {
		if !strings.Contains(line, want) {
			t.Errorf("argv lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "s3cret-pw") || strings.Contains(strings.Join(calls[0].Env, " "), "s3cret-pw") {
		t.Fatalf("the password is in the argument or the environment:\n%s", line)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("credential files stay: %v", left)
	}
	if err := p.Dump(context.Background(), "Bad Name", &w); err == nil {
		t.Fatal("a bad database name was dumped")
	}
	// The restore's role goes in the options, not in the SQL it reads.
	c := PGClient{User: "cgq_x", Password: "pw", Role: "cgo_shop_data"}
	args := strings.Join(p.sandboxArgs(c, "shop_data", "/pw", "/ca", []string{"psql"}), " ")
	if !strings.Contains(args, "--setenv=PGOPTIONS=-c role=cgo_shop_data") {
		t.Fatalf("restore args:\n%s", args)
	}
}
