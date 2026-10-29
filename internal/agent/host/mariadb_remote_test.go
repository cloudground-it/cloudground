package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// testCA is a throwaway self-signed CA, PEM.
var testCA = func() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}()

func remote(tlsOn bool, ca string) *MariaDBAdmin {
	return NewRemoteMariaDB(RemoteServer{ID: 3, Host: "db.example.com", Port: 3307, TLS: tlsOn, CACert: ca,
		AdminUser: "cgadmin", AdminPassword: "s3cret pass#word"}, "", nil)
}

// An external server's accounts are @'%', and on a TLS server they require
// TLS; the box's own server is read exactly as before.
func TestRemoteAdapterRewritesTheHostPart(t *testing.T) {
	cases := []struct{ in, tls, plain string }{
		{"DROP USER IF EXISTS ?@'localhost'", "DROP USER IF EXISTS ?@'%'", "DROP USER IF EXISTS ?@'%'"},
		{"CREATE USER IF NOT EXISTS ?@'localhost' IDENTIFIED BY ?", "CREATE USER IF NOT EXISTS ?@'%' IDENTIFIED BY ?", "CREATE USER IF NOT EXISTS ?@'%' IDENTIFIED BY ?"},
		{"CREATE USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 4", "CREATE USER ?@'%' IDENTIFIED BY ? REQUIRE SSL WITH MAX_USER_CONNECTIONS 4", "CREATE USER ?@'%' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 4"},
		{"ALTER USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 50", "ALTER USER ?@'%' IDENTIFIED BY ? REQUIRE SSL WITH MAX_USER_CONNECTIONS 50", "ALTER USER ?@'%' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 50"},
		{"GRANT SELECT ON `db`.* TO ?@'localhost'", "GRANT SELECT ON `db`.* TO ?@'%'", "GRANT SELECT ON `db`.* TO ?@'%'"},
		{"SELECT 1", "SELECT 1", "SELECT 1"},
	}
	for _, c := range cases {
		if got := remote(true, "").q(c.in); got != c.tls {
			t.Errorf("tls: %q -> %q, want %q", c.in, got, c.tls)
		}
		if got := remote(false, "").q(c.in); got != c.plain {
			t.Errorf("plain: %q -> %q, want %q", c.in, got, c.plain)
		}
		if got := (&MariaDBAdmin{}).q(c.in); got != c.in {
			t.Errorf("local: %q -> %q", c.in, got)
		}
	}
	// A value is never rewritten: it is bound.
	m := remote(true, "")
	var seen string
	m.execFn = func(_ context.Context, q string, args ...any) error { seen = q; return nil }
	if err := m.Exec(context.Background(), "ALTER USER ?@'localhost' IDENTIFIED BY ?", "u", "'localhost'"); err != nil || !strings.Contains(seen, "'%'") {
		t.Fatalf("%q %v", seen, err)
	}
	if m.hostPart() != "%" || (&MariaDBAdmin{}).hostPart() != "localhost" {
		t.Fatal("host parts")
	}
}

// The defaults file says where the server is, with TLS the CA beside it, is
// root-only, and both go with the account.
func TestRemoteAdapterDefaultsFile(t *testing.T) {
	dir := t.TempDir()
	m := remote(true, testCA)
	m.WorkDir = dir
	file, err := m.writeFile("cgq_x", "pw")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	for _, want := range []string{"[client]\n", "user=cgq_x\n", "host=db.example.com\n", "port=3307\n", "ssl=1\n", "ssl-ca=" + OptionValue(file+".ca") + "\n", "ssl-verify-server-cert=1\n", "default-character-set=utf8mb4\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("defaults file lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "socket") {
		t.Errorf("a remote defaults file names a socket:\n%s", b)
	}
	for _, f := range []string{file, file + ".ca"} {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if err := removeClientFile(file); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{file, file + ".ca"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s stays", f)
		}
	}
	// The system's authorities when the server has no CA of its own; none
	// of the TLS lines without TLS.
	m = remote(true, "")
	m.WorkDir = dir
	file, _ = m.writeFile("u", "p")
	if b, _ = os.ReadFile(file); !strings.Contains(string(b), "ssl-ca="+OptionValue(SystemCABundle)) {
		t.Errorf("no system bundle:\n%s", b)
	}
	m = remote(false, "")
	m.WorkDir = dir
	file, _ = m.writeFile("u", "p")
	if b, _ = os.ReadFile(file); strings.Contains(string(b), "ssl") || !strings.Contains(string(b), "host=db.example.com") {
		t.Errorf("a plain connection:\n%s", b)
	}
	// The box's own server is as before.
	local := &MariaDBAdmin{Socket: "/run/mysqld/mysqld.sock", WorkDir: dir}
	file, _ = local.writeFile("u", "p")
	if b, _ = os.ReadFile(file); !strings.Contains(string(b), "socket="+OptionValue("/run/mysqld/mysqld.sock")+"\n") || strings.Contains(string(b), "host=") {
		t.Errorf("local:\n%s", b)
	}
}

func TestRemoteAdapterArgsAndDefiner(t *testing.T) {
	m := remote(true, "")
	if got := strings.Join(m.importArgs("/f", "shop_data"), " "); got != "--defaults-file=/f --sandbox --local-infile=0 --protocol=tcp --one-database shop_data" {
		t.Errorf("import args %q", got)
	}
	if got := strings.Join(m.dumpArgs("shop_data", "t1"), " "); got != "--protocol=tcp --single-transaction shop_data t1" {
		t.Errorf("dump args %q", got)
	}
	if got := strings.Join((&MariaDBAdmin{}).importArgs("/f", "d"), " "); got != strings.Join(ImportArgs("/f", "d"), " ") {
		t.Errorf("local import args %q", got)
	}
	if got := m.definerOf(ImportAs{Site: "shop_user"}); got != "`shop_user`@`%`" {
		t.Errorf("definer %q", got)
	}
	if got := m.definerOf(ImportAs{}); got != "CURRENT_USER" {
		t.Errorf("definer %q", got)
	}
	if got := (&MariaDBAdmin{}).definerOf(ImportAs{Site: "site_1"}); got != "`site_1`@`localhost`" {
		t.Errorf("local definer %q", got)
	}
	// adminClientFile signs in as the administrator, for a dump the agent
	// itself issues; an import never uses it (importActor).
	dir := t.TempDir()
	m.WorkDir = dir
	acct, err := m.adminClientFile()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(acct.DefaultsFile)
	if !strings.Contains(string(b), "user=cgadmin\n") || !strings.Contains(string(b), `password="s3cret pass#word"`) {
		t.Errorf("admin file:\n%s", b)
	}
	if err := acct.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Unborrow(context.Background(), nil); err != nil {
		t.Fatalf("unborrow on a remote: %v", err)
	}
}

func TestRemoteServerTLSConfig(t *testing.T) {
	r := RemoteServer{Host: "db.example.com", Port: 3306, TLS: true, CACert: testCA}
	cfg, err := r.TLSConfig()
	if err != nil || cfg.ServerName != "db.example.com" || cfg.RootCAs == nil {
		t.Fatalf("%+v %v", cfg, err)
	}
	if _, err := (RemoteServer{Host: "h", CACert: "not pem"}).TLSConfig(); err == nil {
		t.Fatal("a bad CA was accepted")
	}
	if got := r.Addr(); got != "db.example.com:3306" {
		t.Fatal(got)
	}
	if got := (RemoteServer{Host: "::1", Port: 3306}).Addr(); got != "[::1]:3306" {
		t.Fatal(got)
	}
	cfgd, err := remote(true, "").rootConfig()
	if err != nil || cfgd.Net != "tcp" || cfgd.User != "cgadmin" || cfgd.TLS == nil || cfgd.Addr != "db.example.com:3307" {
		t.Fatalf("%+v %v", cfgd, err)
	}
	if plain, _ := remote(false, "").rootConfig(); plain.TLS != nil {
		t.Fatal("a plain server has a TLS config")
	}
}
