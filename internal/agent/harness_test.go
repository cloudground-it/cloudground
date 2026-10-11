package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// testHost is the fake machine a test agent runs on.
type testHost struct {
	exec     *host.FakeExec
	systemd  *host.FakeSystemd
	accounts *host.FakeAccounts
	nginx    *host.FakeNginx
	mariadb  *host.FakeMariaDB
	acme     *host.FakeACME
	setup    int // account changes testAgent itself made (www-data)
}

// touched reports whether anything ran or changed on the machine since
// testAgent made it.
func (h *testHost) touched() bool {
	return len(h.exec.Calls()) != 0 || len(h.accounts.Ops()) != h.setup || len(h.systemd.Ops()) != 0
}

// testAgent is an agent whose every path is in a temporary directory and
// whose every adapter is a fake. Site users do not exist where tests run:
// their files belong to whoever runs the tests.
func testAgent(t *testing.T) (*Agent, *testHost) {
	t.Helper()
	root := t.TempDir()
	dir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := os.WriteFile(filepath.Join(dir("bin"), "panel-agent"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Paths{
		SitesRoot: dir("srv"), CacheRoot: dir("cache"), LogRoot: dir("logs"), ACMEWebroot: dir("acme"),
		FallbackCert: filepath.Join(root, "certs", "fallback.pem"), FallbackKey: filepath.Join(root, "certs", "fallback.key"),
		CertLiveDir: dir("letsencrypt"), NginxSites: dir("nginx", "sites-enabled"), NginxConfDir: dir("nginx", "conf.d"),
		PHPSocketDir: dir("phpsock"), PHPMasterDir: dir("php-masters"), SystemdDir: dir("systemd"),
		NginxAuthDir: dir("nginx-auth"), CustomCertDir: dir("custom-certs"), WorkDir: dir("work"), CrontabDir: dir("crontabs"),
		CronDir: dir("cron.d"), CronSpecDir: dir("cron-spec"), BinDir: dir("bin"),
		SiteIndex: filepath.Join(root, "agent", "sites.json"),
	}
	h := &testHost{
		exec:     &host.FakeExec{},
		systemd:  host.NewFakeSystemd(),
		accounts: host.NewFakeAccounts(),
		nginx:    &host.FakeNginx{},
		mariadb:  host.NewFakeMariaDB(),
		acme:     &host.FakeACME{},
	}
	h.systemd.UnitDir = p.SystemdDir
	// The databases and accounts of the sites the tests name, as the
	// agent's record rebuilds them from the server (ops/db SPEC §2).
	seedDatabases(h.mariadb, 1, 20)
	if err := h.accounts.CreateUser(context.Background(), host.UserSpec{Name: "www-data", System: true}); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		Paths: p, Exec: h.exec, Systemd: h.systemd, Accounts: h.accounts, Nginx: h.nginx,
		MariaDB: h.mariadb, ACME: h.acme,
		Renderer:    nginx.Renderer{SitesDir: p.NginxSites, ConfDir: p.NginxConfDir},
		Log:         slog.Default(),
		LookupOwner: func(string) (sitefs.Owner, error) { return sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}, nil },
	}
	h.setup = len(h.accounts.Ops())
	return a, h
}

// staticSite is a static site of the test agent.
func staticSite(a *Agent) state.Site {
	return state.Site{
		ID: 3, Domain: "docs.example.com", Type: state.SiteStatic,
		Profile: state.ProfileBalanced, Engine: state.EngineNginx, SystemUser: "cg-site-3",
		RootPath: filepath.Join(a.Paths.SitesRoot, "docs.example.com"), Status: state.SiteProvisioning,
	}
}

// mustCreate provisions s through the agent's site handlers.
func mustCreate(t *testing.T, a *Agent, s state.Site) {
	t.Helper()
	if _, err := a.sitesHandlers().Create(context.Background(), rpc.CreateSiteParams{Site: s}); err != nil {
		t.Fatalf("create %s: %v", s.Domain, err)
	}
}

// liveRelease is the absolute path of the release a site's current link
// points at.
func liveRelease(t *testing.T, a *Agent, s state.Site) string {
	t.Helper()
	r, err := a.openSite(s)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rel, err := release.Live(r)
	if err != nil {
		t.Fatalf("the site has no live release: %v", err)
	}
	return filepath.Join(s.RootPath, rel)
}

// rpcCall runs a method as a request from the API does, in process: the
// parameters are encoded and decoded, the site validated, the method's lock
// taken and its deadline set before the handler runs.
func rpcCall[P, R any](a *Agent, m agentrpc.Method[P, R], p P) (R, error) {
	var out R
	raw, err := json.Marshal(p)
	if err != nil {
		return out, err
	}
	v, e := a.NewServer(agentrpc.Config{}).Invoke(context.Background(), m.Name(), raw)
	if e != nil {
		return out, e
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// selfSigned is a certificate and its key for names.
func selfSigned(t *testing.T, names ...string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}
