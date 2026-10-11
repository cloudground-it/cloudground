package certs

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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

type fixture struct {
	*Ops
	acme    *host.FakeACME
	renders []string
	mu      sync.Mutex
	dir     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{dir}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := &fixture{acme: &host.FakeACME{}, dir: dir}
	f.Ops = &Ops{
		Paths: Paths{
			CertLiveDir:   mk("letsencrypt", "live"),
			CustomCertDir: mk("certs", "custom"),
			ACMEWebroot:   mk("acme"),
			PanelCert:     filepath.Join(mk("certs"), "panel.pem"),
		},
		ACME:   f.acme,
		CertMu: &sync.Mutex{},
		// No test reaches the network: a preflight must be routed to a
		// test server explicitly.
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Errorf("unexpected request to %s", r.URL)
			return nil, errors.New("no network in tests")
		})},
		LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.7"}, nil },
	}
	f.Render = func(ctx context.Context, s state.Site) ([]rpc.ManagedFile, error) {
		// The certbot lock is never held across a render.
		if !f.CertMu.TryLock() {
			t.Error("render ran under the certbot lock")
		} else {
			f.CertMu.Unlock()
		}
		f.mu.Lock()
		f.renders = append(f.renders, s.Domain)
		f.mu.Unlock()
		return []rpc.ManagedFile{{Path: "/etc/nginx/sites-enabled/" + s.Domain + ".conf", SHA256: "x"}}, nil
	}
	return f
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// serveWebroot routes every preflight request to a server that serves the
// ACME webroot as nginx would, whatever host it names.
func (f *fixture) serveWebroot(t *testing.T, handler http.Handler) {
	if handler == nil {
		handler = http.FileServer(http.Dir(f.Paths.ACMEWebroot))
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	f.HTTP = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func testSite() state.Site {
	return state.Site{ID: 3, Domain: "shop.example.com", Aliases: []string{"www.shop.example.com"},
		Type: state.SiteStatic, SystemUser: "cg-site-3", RootPath: "/srv/sites/shop.example.com"}
}

func codeOf(err error) agentrpc.Code {
	var e *agentrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return agentrpc.CodeInternal
}

func TestIssueCertificateOverHTTP01(t *testing.T) {
	f := newFixture(t)
	f.serveWebroot(t, nil)
	res, err := f.IssueCertificate(context.Background(), rpc.CertificateParams{Site: testSite(), Email: "ops@example.com", Staging: true})
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.acme.Requests()
	if len(reqs) != 1 {
		t.Fatalf("certbot runs: %d", len(reqs))
	}
	r := reqs[0]
	if r.Name != "shop.example.com" || strings.Join(r.Domains, " ") != "shop.example.com www.shop.example.com" ||
		r.Webroot != f.Paths.ACMEWebroot || r.CloudflareCredentials != "" || !r.Staging {
		t.Fatalf("request %+v", r)
	}
	if len(f.renders) != 1 || len(res.Files) != 1 {
		t.Fatalf("the site was not re-rendered: %v %v", f.renders, res.Files)
	}
	// The preflight token does not outlive the call.
	left, _ := os.ReadDir(filepath.Join(f.Paths.ACMEWebroot, ".well-known", "acme-challenge"))
	if len(left) != 0 {
		t.Fatalf("preflight files left: %v", left)
	}
}

// The webroot directories and the token must be readable by nginx whatever
// the agent's umask (UMask=0027 made every validation answer 403).
func TestPreflightModesIgnoreTheUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	f := newFixture(t)
	// The webroot itself is missing too: the agent makes it, and nginx's
	// workers must traverse it.
	if err := os.Remove(f.Paths.ACMEWebroot); err != nil {
		t.Fatal(err)
	}
	var seen os.FileMode
	f.serveWebroot(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(f.Paths.ACMEWebroot, filepath.FromSlash(r.URL.Path))
		fi, err := os.Stat(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		seen = fi.Mode().Perm()
		http.ServeFile(w, r, p)
	}))
	if err := f.Preflight(context.Background(), []string{"shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if seen != 0o644 {
		t.Fatalf("token mode %v, want 0644", seen)
	}
	for _, d := range []string{".", ".well-known", ".well-known/acme-challenge"} {
		if fi, _ := os.Stat(filepath.Join(f.Paths.ACMEWebroot, d)); fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v", d, fi.Mode().Perm())
		}
	}
}

// A name that does not reach this server fails before certbot runs, with
// where its DNS points.
func TestPreflightFailureStopsIssuance(t *testing.T) {
	f := newFixture(t)
	f.serveWebroot(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "proxied elsewhere", http.StatusForbidden)
	}))
	_, err := f.IssueCertificate(context.Background(), rpc.CertificateParams{Site: testSite(), Email: "ops@example.com"})
	if codeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "192.0.2.7") {
		t.Fatalf("err = %v", err)
	}
	if len(f.acme.Requests()) != 0 || len(f.renders) != 0 {
		t.Fatal("certbot ran or the site was rendered after a failed preflight")
	}
}

func TestIssueCertificateRefusesBadEmails(t *testing.T) {
	f := newFixture(t)
	for _, email := range []string{"", "-oops@example.com", "not an address", "Ops <ops@example.com>"} {
		_, err := f.IssueCertificate(context.Background(), rpc.CertificateParams{Site: testSite(), Email: email})
		if codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%q: %v", email, err)
		}
	}
	if len(f.acme.Requests()) != 0 {
		t.Fatal("certbot ran")
	}
}

// A wildcard goes through Cloudflare DNS for the domain and *.domain, with a
// 0600 credentials file, and needs no HTTP preflight.
func TestWildcardUsesDNS01(t *testing.T) {
	f := newFixture(t)
	p := rpc.CertificateParams{Site: testSite(), Email: "ops@example.com", Wildcard: true}
	if _, err := f.IssueCertificate(context.Background(), p); codeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a wildcard without a token: %v", err)
	}
	p.CloudflareToken = strings.Repeat("a", 40)
	if _, err := f.IssueCertificate(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r := f.acme.Requests()[0]
	creds := filepath.Join(filepath.Dir(f.Paths.CertLiveDir), "cloudground-cloudflare.ini")
	if strings.Join(r.Domains, " ") != "shop.example.com *.shop.example.com" || r.CloudflareCredentials != creds || r.Webroot != "" {
		t.Fatalf("request %+v", r)
	}
	fi, err := os.Stat(creds)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(creds); string(b) != "dns_cloudflare_api_token = "+p.CloudflareToken+"\n" {
		t.Fatalf("credentials content %q", b)
	}
}

// A certbot failure is internal (its stderr is detail, not message) and
// leaves the site as it was.
func TestIssueFailureRendersNothing(t *testing.T) {
	f := newFixture(t)
	f.acme.OnCertOnly = func(context.Context, host.CertRequest) error {
		return &host.ExitError{Name: "certbot", Code: 1, Stderr: "Some challenges have failed. /etc/letsencrypt/..."}
	}
	_, err := f.IssueCertificate(context.Background(), rpc.CertificateParams{Site: testSite(), Email: "ops@example.com",
		Wildcard: true, CloudflareToken: strings.Repeat("a", 40)})
	if err == nil || codeOf(err) != agentrpc.CodeInternal {
		t.Fatalf("err = %v", err)
	}
	if len(f.renders) != 0 {
		t.Fatal("rendered after a failed issuance")
	}
}

// The certbot lock covers certbot only: while another issuance holds it, a
// custom certificate (no certbot) goes through.
func TestCustomCertificateDoesNotWaitForCertbot(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	running := make(chan struct{})
	f.acme.OnCertOnly = func(context.Context, host.CertRequest) error {
		close(running)
		<-release
		return nil
	}
	other := testSite()
	other.Domain = "other.example.com"
	go f.IssueCertificate(context.Background(), rpc.CertificateParams{Site: other, Email: "ops@example.com",
		Wildcard: true, CloudflareToken: strings.Repeat("a", 40)})
	<-running
	defer close(release)
	// The other site's issuance holds the certbot lock on purpose here.
	f.Render = func(context.Context, state.Site) ([]rpc.ManagedFile, error) { return nil, nil }
	cert, key := selfSigned(t, "shop.example.com", time.Now().Add(90*24*time.Hour))
	done := make(chan error, 1)
	go func() {
		_, err := f.CustomCertificate(context.Background(), rpc.CustomCertParams{Site: testSite(), Fullchain: cert, Key: key})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a custom certificate waited for another site's certbot run")
	}
}

func TestCustomCertificate(t *testing.T) {
	f := newFixture(t)
	s := testSite()
	now := time.Now()
	good, goodKey := selfSigned(t, s.Domain, now.Add(90*24*time.Hour))
	other, otherKey := selfSigned(t, "elsewhere.example.com", now.Add(90*24*time.Hour))
	for name, p := range map[string]rpc.CustomCertParams{
		"another domain":  {Site: s, Fullchain: other, Key: otherKey},
		"someone's key":   {Site: s, Fullchain: good, Key: otherKey},
		"not a pem":       {Site: s, Fullchain: "hello", Key: "world"},
		"expired already": {Site: s, Fullchain: good, Key: goodKey},
	} {
		if name == "expired already" {
			f.Now = func() time.Time { return now.Add(100 * 24 * time.Hour) }
		}
		if _, err := f.CustomCertificate(context.Background(), p); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
		f.Now = nil
	}
	if len(f.renders) != 0 {
		t.Fatal("a refused certificate was rendered")
	}
	if _, err := f.CustomCertificate(context.Background(), rpc.CustomCertParams{Site: s, Fullchain: good, Key: goodKey}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.Paths.CustomCertDir, s.Domain)
	for name, mode := range map[string]os.FileMode{"privkey.pem": 0o600, "fullchain.pem": 0o644} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	st, _ := f.CertStatus(context.Background(), rpc.CertStatusParams{Names: []string{s.Domain}})
	if c := st.Certs[s.Domain]; !c.Exists || !c.Custom {
		t.Fatalf("status after install: %+v", c)
	}
	if _, err := f.CustomCertificate(context.Background(), rpc.CustomCertParams{Site: s, Remove: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("custom certificate still there: %v", err)
	}
	if len(f.renders) != 2 {
		t.Fatalf("renders %v", f.renders)
	}
}

// CertStatus reads what nginx serves: the brought certificate over Let's
// Encrypt, and for the panel the installed copy, not certbot's lineage.
func TestCertStatus(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	write := func(path string, notAfter time.Time) {
		cert, _ := selfSigned(t, "shop.example.com", notAfter)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(cert), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	le := now.Add(20 * 24 * time.Hour)
	write(filepath.Join(f.Paths.CertLiveDir, "shop.example.com", "fullchain.pem"), le)
	write(filepath.Join(f.Paths.CertLiveDir, "custom.example.com", "fullchain.pem"), le)
	custom := now.Add(10 * 24 * time.Hour)
	write(filepath.Join(f.Paths.CustomCertDir, "custom.example.com", "fullchain.pem"), custom)
	// certbot's timer renewed the panel lineage; the copy nginx serves is
	// the old one.
	write(filepath.Join(f.Paths.CertLiveDir, PanelLineage, "fullchain.pem"), now.Add(89*24*time.Hour))
	installed := now.Add(5 * 24 * time.Hour)
	write(f.Paths.PanelCert, installed)
	os.MkdirAll(filepath.Join(f.Paths.CertLiveDir, "garbage.example.com"), 0o755)
	os.WriteFile(filepath.Join(f.Paths.CertLiveDir, "garbage.example.com", "fullchain.pem"), []byte("not a cert"), 0o644)

	res, err := f.CertStatus(context.Background(), rpc.CertStatusParams{Names: []string{
		"shop.example.com", "custom.example.com", PanelLineage, "missing.example.com", "garbage.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]rpc.CertInfo{
		"shop.example.com":    {Exists: true, NotAfter: le, Issuer: "shop.example.com"},
		"custom.example.com":  {Exists: true, NotAfter: custom, Issuer: "shop.example.com", Custom: true},
		PanelLineage:          {Exists: true, NotAfter: installed, Issuer: "shop.example.com"},
		"missing.example.com": {},
		"garbage.example.com": {},
	} {
		got := res.Certs[name]
		if got.Exists != want.Exists || !got.NotAfter.Equal(want.NotAfter) || got.Issuer != want.Issuer || got.Custom != want.Custom {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	for _, bad := range []string{"../../etc/shadow", "-x", "Shop.example.com", "a b"} {
		if _, err := f.CertStatus(context.Background(), rpc.CertStatusParams{Names: []string{"shop.example.com", bad}}); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// Deleting a site removes its lineage and the certificate the operator
// brought, never following a symlink, and leaves other sites' alone.
func TestRemoveSite(t *testing.T) {
	f := newFixture(t)
	s := testSite()
	cert, key := selfSigned(t, s.Domain, time.Now().Add(90*24*time.Hour))
	if _, err := f.CustomCertificate(context.Background(), rpc.CustomCertParams{Site: s, Fullchain: cert, Key: key}); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(f.Paths.CustomCertDir, "other.example.com")
	os.MkdirAll(otherDir, 0o700)
	os.WriteFile(filepath.Join(otherDir, "privkey.pem"), []byte("k"), 0o600)
	// No lineage exists: not an error.
	if err := f.RemoveSite(context.Background(), s.Domain); err != nil {
		t.Fatal(err)
	}
	if d := f.acme.Deleted(); len(d) != 1 || d[0] != s.Domain {
		t.Fatalf("certbot delete %v", d)
	}
	if _, err := os.Lstat(filepath.Join(f.Paths.CustomCertDir, s.Domain)); !os.IsNotExist(err) {
		t.Fatalf("custom certificate outlived the site: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherDir, "privkey.pem")); err != nil {
		t.Fatal("another site's certificate was removed")
	}
	// A symlink at the name is removed, not followed.
	victim := t.TempDir()
	os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600)
	os.Symlink(victim, filepath.Join(f.Paths.CustomCertDir, s.Domain))
	if err := f.RemoveSite(context.Background(), s.Domain); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatal("removal followed a symlink")
	}
	if err := f.RemoveSite(context.Background(), "../etc"); codeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a path was accepted: %v", err)
	}
}

// selfSigned is a certificate for dns (key included) valid until notAfter.
func selfSigned(t *testing.T, dns string, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: dns},
		DNSNames: []string{dns}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}
