package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// fakeDNS answers lookups from a table of name → addresses (both
// families); a name in fail times out, and every name asked is kept.
type fakeDNS struct {
	mu    sync.Mutex
	zone  map[string][]string
	fail  map[string]bool
	asked []string
}

func (f *fakeDNS) lookup(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, network+" "+host)
	if f.fail[host] {
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	var out []netip.Addr
	for _, a := range f.zone[host] {
		ip := netip.MustParseAddr(a)
		if ip.Is4() == (network == "ip4") {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

func dnsTestServer(t *testing.T) (*Server, *client, *fakeDNS, *httptest.Server) {
	t.Helper()
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	s.localAddrs = func() []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.5"),
			netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10"), netip.MustParseAddr("fe80::1")}
	}
	dns := &fakeDNS{zone: map[string][]string{
		"here.example.com":      {"192.0.2.10", "2001:db8::10"},
		"v4only.example.com":    {"192.0.2.10"},
		"partial.example.com":   {"192.0.2.10", "2001:db8::99"},
		"elsewhere.example.com": {"198.51.100.7"},
		"inward.example.com":    {"10.0.0.9"},
		"proxied.example.com":   {"104.16.1.1", "2606:4700::1"},
	}, fail: map[string]bool{"slow.example.com": true}}
	s.dnsLookup = dns.lookup
	return s, admin, dns, ts
}

func getDNSCheck(t *testing.T, c *client, name string) (int, dnsCheck) {
	t.Helper()
	resp, body := c.do("GET", "/api/sites/dns-check?domain="+name, nil)
	var out dnsCheck
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: %v %s", name, err, body)
		}
	}
	return resp.StatusCode, out
}

// #132 (SPEC §3.14): the verdict per name, compared with this machine's
// public addresses only; a private address is not named; a CDN's ranges
// say the proxy hides the origin.
func TestDNSCheckVerdicts(t *testing.T) {
	_, admin, _, _ := dnsTestServer(t)
	for name, want := range map[string]string{
		"here.example.com":      dnsHere,
		"HERE.example.com.":     dnsHere,
		"v4only.example.com":    dnsHere,
		"partial.example.com":   dnsPartial,
		"elsewhere.example.com": dnsElsewhere,
		"inward.example.com":    dnsElsewhere,
		"proxied.example.com":   dnsProxied,
		"missing.example.com":   dnsUnresolved,
		"slow.example.com":      dnsUnknown,
	} {
		code, got := getDNSCheck(t, admin, name)
		if code != http.StatusOK || got.Status != want {
			t.Errorf("%s = %d %q, want %q (%+v)", name, code, got.Status, want, got)
		}
	}
	_, got := getDNSCheck(t, admin, "here.example.com")
	if !slices.Equal(got.Server, []string{"192.0.2.10", "2001:db8::10"}) {
		t.Errorf("server addresses %v: only the public ones", got.Server)
	}
	if got.Domain != "here.example.com" || len(got.Addresses) != 2 || got.Addresses[0].Family != 4 || !got.Addresses[0].Own || got.Addresses[1].Family != 6 {
		t.Errorf("addresses %+v", got.Addresses)
	}
	_, got = getDNSCheck(t, admin, "inward.example.com")
	if len(got.Addresses) != 1 || got.Addresses[0].Address != "" || !got.Addresses[0].Private {
		t.Errorf("a private address was named: %+v", got.Addresses)
	}
	_, got = getDNSCheck(t, admin, "proxied.example.com")
	if got.CDN != "cloudflare" {
		t.Errorf("proxied: cdn %q", got.CDN)
	}
}

// A server with no public address of its own (behind NAT) cannot tell.
func TestDNSCheckNoPublicAddress(t *testing.T) {
	s, admin, _, _ := dnsTestServer(t)
	s.localAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("10.0.0.5")} }
	if code, got := getDNSCheck(t, admin, "elsewhere.example.com"); code != http.StatusOK || got.Status != dnsUnknown || len(got.Server) != 0 {
		t.Fatalf("= %d %+v", code, got)
	}
}

// Only a host name is looked up: an address, a URL or anything else is
// 400 and never reaches the resolver.
func TestDNSCheckRefusesWhatIsNotAHostName(t *testing.T) {
	_, admin, dns, _ := dnsTestServer(t)
	for _, bad := range []string{"", "192.0.2.10", "localhost", "http%3A%2F%2Fexample.com", "example.com%3A8080", "a..example.com", "-x.example.com", "*.example.com", "%5B2001%3Adb8%3A%3A1%5D"} {
		if code, _ := getDNSCheck(t, admin, bad); code != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", bad, code)
		}
	}
	if len(dns.asked) != 0 {
		t.Fatalf("the resolver was asked %v", dns.asked)
	}
}

// Admins and operators may ask; a read-only account may not. The limit is
// per account, and a 429 asks nothing.
func TestDNSCheckAccessAndLimit(t *testing.T) {
	_, admin, dns, ts := dnsTestServer(t)
	resp, body := admin.do("POST", "/api/users", map[string]any{"email": "ro@example.com", "password": "readonly-password!", "role": "readonly"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create the read-only account = %d %s", resp.StatusCode, body)
	}
	ro := signIn(t, ts, "ro@example.com", "readonly-password!")
	if code, _ := getDNSCheck(t, ro, "here.example.com"); code != http.StatusForbidden {
		t.Fatalf("read-only = %d, want 403", code)
	}

	for i := range dnsChecksPerUser {
		if code, _ := getDNSCheck(t, admin, "here.example.com"); code != http.StatusOK {
			t.Fatalf("check %d = %d", i, code)
		}
	}
	before := len(dns.asked)
	if code, _ := getDNSCheck(t, admin, "here.example.com"); code != http.StatusTooManyRequests {
		t.Fatalf("check %d = %d, want 429", dnsChecksPerUser+1, code)
	}
	if len(dns.asked) != before {
		t.Fatal("a refused check reached the resolver")
	}
}

// The lookup is bounded by the check's timeout.
func TestDNSCheckTimeout(t *testing.T) {
	s, _, _, _ := dnsTestServer(t)
	s.dnsLookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := s.checkDNS(ctx, "here.example.com")
	if got.Status != dnsUnknown || time.Since(start) > time.Second {
		t.Fatalf("= %+v after %v", got, time.Since(start))
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("the context did not expire")
	}
}

// #132: aliases given at creation are stored with the site, checked like
// the edit path's (another site's name is 409), and the first
// certificate is asked for every name.
func TestCreateWithAliases(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.ACMEEmail = "ops@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	body := staticSite("aliased.example.com")
	body["aliases"] = []string{" WWW.aliased.example.com ", "shop.example.net"}
	site := createViaAPI(t, s, c, body)
	if !slices.Equal(site.Aliases, []string{"www.aliased.example.com", "shop.example.net"}) {
		t.Fatalf("stored aliases %v", site.Aliases)
	}
	var certified []string
	for _, p := range issueCalls(agent) {
		if p.Site.Domain == site.Domain {
			certified = p.Site.Aliases
		}
	}
	if !slices.Equal(certified, site.Aliases) {
		t.Fatalf("the first certificate was asked for aliases %v", certified)
	}

	other := staticSite("second.example.com")
	other["aliases"] = []string{"shop.example.net"}
	if resp, b := c.do("POST", "/api/sites", other); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a taken alias = %d %s", resp.StatusCode, b)
	}
	many := staticSite("many.example.com")
	var names []string
	for i := range maxAliases + 1 {
		names = append(names, fmt.Sprintf("a%d.many.example.com", i))
	}
	many["aliases"] = names
	if resp, b := c.do("POST", "/api/sites", many); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d aliases = %d %s", len(names), resp.StatusCode, b)
	}
	dup := staticSite("third.example.com")
	dup["aliases"] = []string{"third.example.com"}
	if resp, b := c.do("POST", "/api/sites", dup); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the site's own domain as alias = %d %s", resp.StatusCode, b)
	}
}
