package panel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/certs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

type certFixture struct {
	*Ops
	acme       *host.FakeACME
	ngx        *host.FakeNginx
	sd         *host.FakeSystemd
	preflights []string
}

func newCertFixture(t *testing.T) *certFixture {
	t.Helper()
	dir := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{dir}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	certDir := mk("etc", "cloudground", "certs")
	f := &certFixture{acme: &host.FakeACME{}, ngx: &host.FakeNginx{Build: "nginx version: nginx/1.26.3 (Ubuntu)"}, sd: host.NewFakeSystemd()}
	f.Ops = &Ops{
		Paths: Paths{
			NginxSites:  mk("nginx", "sites-enabled"),
			ACMEWebroot: mk("acme"),
			CertLiveDir: mk("letsencrypt", "live"),
			PanelCert:   filepath.Join(certDir, "panel.pem"),
			PanelKey:    filepath.Join(certDir, "panel.key"),
			PanelAccess: "/etc/cloudground/panel-access.conf",
			PanelGroup:  "cloudground",
			APISocket:   "/run/cloudground/api.sock",
			BinDir:      mk("bin"),
			SystemdDir:  mk("systemd"),
			WorkDir:     mk("state", "work"),
		},
		ACME: f.acme, Nginx: f.ngx, Systemd: f.sd,
		CertMu: &sync.Mutex{}, NginxMu: &sync.Mutex{},
		LookupGroup: func(string) (int, error) { return os.Getgid(), nil },
	}
	f.Preflight = func(_ context.Context, domains []string) error {
		// The nginx lock is free while the public challenge is fetched.
		if !f.NginxMu.TryLock() {
			t.Error("preflight ran under the nginx lock")
		} else {
			f.NginxMu.Unlock()
		}
		f.preflights = append(f.preflights, domains...)
		return nil
	}
	// The installer's self-signed pair is in place.
	selfCert, selfKey := pemPair(t, "cloudground-panel", time.Now().Add(3650*24*time.Hour))
	os.WriteFile(f.Paths.PanelCert, selfCert, 0o640)
	os.WriteFile(f.Paths.PanelKey, selfKey, 0o640)
	f.issueAs(t, "panel.example.com")
	return f
}

// issueAs makes "certbot" write a lineage for domain; the nginx lock must be
// free while it runs.
func (f *certFixture) issueAs(t *testing.T, domain string) {
	chain, key := pemPair(t, domain, time.Now().Add(90*24*time.Hour))
	f.acme.OnCertOnly = func(_ context.Context, r host.CertRequest) error {
		if !f.NginxMu.TryLock() {
			t.Error("certbot ran under the nginx lock")
		} else {
			f.NginxMu.Unlock()
		}
		live := filepath.Join(f.Paths.CertLiveDir, r.Name)
		os.MkdirAll(live, 0o755)
		os.WriteFile(filepath.Join(live, "fullchain.pem"), chain, 0o644)
		os.WriteFile(filepath.Join(live, "privkey.pem"), key, 0o600)
		return nil
	}
}

func (f *certFixture) vhost(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(f.Paths.NginxSites, nginx.PanelVhostName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPanelCertificateIssuesAndInstalls(t *testing.T) {
	f := newCertFixture(t)
	out, err := f.PanelCertificate(context.Background(), rpc.PanelCertParams{Domain: "panel.example.com", Email: "ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "issued" || out["domain"] != "panel.example.com" {
		t.Fatalf("result %v", out)
	}
	r := f.acme.Requests()[0]
	if r.Name != certs.PanelLineage || strings.Join(r.Domains, ",") != "panel.example.com" || r.Webroot != f.Paths.ACMEWebroot {
		t.Fatalf("certbot request %+v", r)
	}
	if len(f.preflights) != 1 {
		t.Fatalf("preflights %v", f.preflights)
	}
	v := f.vhost(t)
	if !strings.Contains(v, "http2 on;") || strings.Contains(v, "ssl http2") {
		t.Fatalf("nginx 1.26 should get the http2 directive:\n%s", v)
	}
	live, _ := os.ReadFile(filepath.Join(f.Paths.CertLiveDir, certs.PanelLineage, "fullchain.pem"))
	if got, _ := os.ReadFile(f.Paths.PanelCert); string(got) != string(live) {
		t.Fatal("the issued certificate was not installed")
	}
	for _, p := range []string{f.Paths.PanelCert, f.Paths.PanelKey} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
			t.Fatalf("%s mode %v", p, fi.Mode().Perm())
		}
	}
	tests, reloads := f.ngx.Counts()
	if tests != 2 || reloads != 2 {
		t.Fatalf("nginx tests %d reloads %d, want 2 and 2 (vhost, certificate)", tests, reloads)
	}
	// The API is not restarted: it never presents this certificate.
	if ops := f.sd.Ops(); len(ops) != 0 {
		t.Fatalf("systemd was asked to %v", ops)
	}

	// The scheduler's next call: certbot has nothing new, nothing changes.
	out, err = f.PanelCertificate(context.Background(), rpc.PanelCertParams{Domain: "panel.example.com", Email: "ops@example.com"})
	if err != nil || out["status"] != "unchanged" {
		t.Fatalf("second call: %v %v", out, err)
	}
	if tests, reloads := f.ngx.Counts(); tests != 2 || reloads != 2 {
		t.Fatalf("an unchanged certificate reloaded nginx (%d tests, %d reloads)", tests, reloads)
	}
}

func TestPanelVhostFollowsTheNginxVersion(t *testing.T) {
	f := newCertFixture(t)
	f.ngx.Build = "nginx version: nginx/1.24.0 (Ubuntu)"
	if _, err := f.PanelCertificate(context.Background(), rpc.PanelCertParams{Domain: "panel.example.com", Email: "ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	if v := f.vhost(t); !strings.Contains(v, "listen 443 ssl http2;") || strings.Contains(v, "http2 on;") {
		t.Fatalf("nginx 1.24 must get the listen parameter:\n%s", v)
	}
}

// A vhost nginx refuses is taken back out, and certbot is not run.
func TestPanelVhostRefusedByNginxIsRolledBack(t *testing.T) {
	f := newCertFixture(t)
	path := filepath.Join(f.Paths.NginxSites, nginx.PanelVhostName)
	f.ngx.TestErr = &host.ConfigError{Output: "nginx: [emerg] something"}
	if _, err := f.PanelCertificate(context.Background(), rpc.PanelCertParams{Domain: "panel.example.com", Email: "ops@example.com"}); err == nil {
		t.Fatal("accepted")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("the refused vhost was left in place")
	}
	os.WriteFile(path, []byte("# previous\n"), 0o644)
	f.PanelCertificate(context.Background(), rpc.PanelCertParams{Domain: "panel.example.com", Email: "ops@example.com"})
	if b, _ := os.ReadFile(path); string(b) != "# previous\n" {
		t.Fatalf("the previous vhost was not restored: %q", b)
	}
	if len(f.acme.Requests()) != 0 {
		t.Fatal("certbot ran after nginx refused the vhost")
	}
}

// A new pair nginx refuses is replaced by the previous one.
func TestPanelCertificateRefusedByNginxIsRolledBack(t *testing.T) {
	f := newCertFixture(t)
	prevCert, _ := os.ReadFile(f.Paths.PanelCert)
	prevKey, _ := os.ReadFile(f.Paths.PanelKey)
	issue := f.acme.OnCertOnly
	f.acme.OnCertOnly = func(ctx context.Context, r host.CertRequest) error {
		f.ngx.TestErr = &host.ConfigError{Output: "SSL_CTX_use_PrivateKey failed"}
		return issue(ctx, r)
	}
	if _, err := f.PanelCertificate(context.Background(), rpc.PanelCertParams{Domain: "panel.example.com", Email: "ops@example.com"}); !errors.Is(err, host.ErrConfigInvalid) {
		t.Fatalf("err = %v", err)
	}
	if c, _ := os.ReadFile(f.Paths.PanelCert); string(c) != string(prevCert) {
		t.Fatal("the previous certificate was not restored")
	}
	if k, _ := os.ReadFile(f.Paths.PanelKey); string(k) != string(prevKey) {
		t.Fatal("the previous key was not restored")
	}
}

func TestPanelCertificateValidates(t *testing.T) {
	f := newCertFixture(t)
	for _, p := range []rpc.PanelCertParams{
		{Domain: "panel.example.com; include /x", Email: "ops@example.com"},
		{Domain: "", Email: "ops@example.com"},
		{Domain: "panel.example.com", Email: ""},
		{Domain: "panel.example.com", Email: "-x@example.com"},
	} {
		_, err := f.PanelCertificate(context.Background(), p)
		var e *agentrpc.Error
		if !errors.As(err, &e) || e.Code != agentrpc.CodeInvalid {
			t.Errorf("%+v: %v", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.Paths.NginxSites, nginx.PanelVhostName)); !os.IsNotExist(err) {
		t.Fatal("a refused request wrote the vhost")
	}
}

func pemPair(t *testing.T, cn string, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		DNSNames: []string{cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}
