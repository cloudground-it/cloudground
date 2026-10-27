package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// fakeCloudflare is Cloudflare's API: the published ranges and the purge.
type fakeCloudflare struct {
	mu      sync.Mutex
	ips     string // the body of GET /ips
	fail    bool   // purges answer 403
	purges  []string
	auth    []string
	gets    int
	purged  chan struct{}
	zoneHit string
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/ips":
		f.gets++
		_, _ = io.WriteString(w, f.ips)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/purge_cache"):
		b, _ := io.ReadAll(r.Body)
		f.purges = append(f.purges, string(b))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.zoneHit = r.URL.Path
		if f.purged != nil {
			select {
			case f.purged <- struct{}{}:
			default:
			}
		}
		if f.fail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":{"id":"x"}}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCloudflare) take() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, a := f.purges, f.auth
	f.purges, f.auth = nil, nil
	return p, a
}

func withFakeCloudflare(t *testing.T, s *Server) *fakeCloudflare {
	t.Helper()
	f := &fakeCloudflare{ips: `{"success":true,"errors":[],"result":{"ipv4_cidrs":["104.16.0.0/13","172.64.0.0/13"],"ipv6_cidrs":["2606:4700::/32"]}}`}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	s.cloudflareAPI, s.cloudflareClient = ts.URL, ts.Client()
	return f
}

const testZone = "0123456789abcdef0123456789abcdef"

// #22 (SPEC §3.20): settings rendered before they are stored, the
// agent's ranges sent before a vhost names them, the zone checked.
func TestSiteCloudflareSettings(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	withFakeCloudflare(t, s)
	site := seedActiveSite(t, s, "cdn.example.com", state.SiteWordPress, "8.4", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/cloudflare", site.ID)

	for _, bad := range []string{"nope", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdeg"} {
		if resp, b := admin.do("PUT", path, map[string]any{"zone_id": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q = %d %s", bad, resp.StatusCode, b)
		}
	}

	resp, body := admin.do("PUT", path, map[string]any{"real_ip": true, "zone_id": " " + strings.ToUpper(testZone) + " "})
	var got cloudflareView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	if !got.RealIP || got.ZoneID != testZone || got.PurgeReady || got.RangesUpdatedAt != nil {
		t.Fatalf("answer %+v", got)
	}
	agent.mu.Lock()
	var order []string
	for _, c := range agent.history {
		order = append(order, c.method)
	}
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	ranges, _ := agent.lastParams[agentrpc.SetCloudflareRanges.Name()].(rpc.CloudflareRangesParams)
	agent.mu.Unlock()
	if strings.Join(order, ",") != "SetCloudflareRanges,ApplySiteConfig" {
		t.Fatalf("agent calls %v", order)
	}
	if !ref.Site.Config.Cloudflare.RealIP || len(ranges.V4) != 15 {
		t.Fatalf("rendered %+v with ranges %v", ref.Site.Config.Cloudflare, ranges)
	}
	if st, _ := s.Store.GetSite(site.ID); !st.Config.Cloudflare.RealIP || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("stored %+v", st.Config.Cloudflare)
	}

	// With the token, the purge is ready; a field left out keeps its value.
	if err := s.Store.SetSecret(state.SecretCloudflarePurgeToken, "purge-token"); err != nil {
		t.Fatal(err)
	}
	resp, body = admin.do("PUT", path, map[string]any{"real_ip": false})
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got.RealIP || got.ZoneID != testZone || !got.PurgeReady {
		t.Fatalf("off = %d %s", resp.StatusCode, body)
	}
	if agent.count(agentrpc.SetCloudflareRanges.Name()) != 1 {
		t.Fatal("turning it off sent the ranges again")
	}

	// A refused render stores nothing.
	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Wrap(agentrpc.CodeUnavailable, "nginx -t failed", nil)
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"zone_id": ""}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.Cloudflare.ZoneID != testZone {
		t.Fatal("a refused render was stored")
	}
}

// The purge rides on the panel's and the connector's: only the site's
// names, 30 to a request, with the token; a failure never fails it.
func TestCloudflarePurge(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	cf := withFakeCloudflare(t, s)
	site := seedActiveSite(t, s, "shop.example.com", state.SiteWordPress, "8.4", 0)
	site, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Aliases = []string{"www.shop.example.com"}
		st.Config.Cloudflare.ZoneID = testZone
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/purge", site.ID)

	// No token: nothing sent, no cdn field.
	resp, body := admin.do("POST", path, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "cdn") {
		t.Fatalf("no token = %d %s", resp.StatusCode, body)
	}
	if p, _ := cf.take(); len(p) != 0 {
		t.Fatalf("sent without a token: %v", p)
	}

	if err := s.Store.SetSecret(state.SecretCloudflarePurgeToken, "purge-token"); err != nil {
		t.Fatal(err)
	}
	resp, body = admin.do("POST", path, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"cdn":"purged"`) {
		t.Fatalf("everything = %d %s", resp.StatusCode, body)
	}
	p, auth := cf.take()
	if len(p) != 1 || p[0] != `{"hosts":["shop.example.com","www.shop.example.com"]}` || auth[0] != "Bearer purge-token" || cf.zoneHit != "/zones/"+testZone+"/purge_cache" {
		t.Fatalf("sent %v %v to %s", p, auth, cf.zoneHit)
	}

	var urls []string
	for i := range 31 {
		urls = append(urls, fmt.Sprintf("https://shop.example.com/p/%d/", i))
	}
	urls = append(urls, "https://elsewhere.example.org/x/")
	if resp, body := admin.do("POST", path, map[string]any{"urls": urls}); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"cdn":"purged"`) {
		t.Fatalf("urls = %d %s", resp.StatusCode, body)
	}
	p, _ = cf.take()
	if len(p) != 2 || strings.Count(p[0], "https://") != 30 || strings.Count(p[1], "https://") != 1 || strings.Contains(strings.Join(p, ""), "elsewhere") {
		t.Fatalf("batches %v", p)
	}

	cf.mu.Lock()
	cf.fail = true
	cf.mu.Unlock()
	resp, body = admin.do("POST", path, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"cdn":"failed"`) || !strings.Contains(string(body), "Authentication error") {
		t.Fatalf("a Cloudflare failure = %d %s", resp.StatusCode, body)
	}
	cf.mu.Lock()
	cf.fail = false
	cf.purged = make(chan struct{}, 1)
	cf.mu.Unlock()
	cf.take()

	// The connector's purge reaches Cloudflare in the background.
	token := randomToken(24)
	if err := s.Store.SetSecret(state.SiteSecret(site.ID, state.SiteSecretConnectorToken), token); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/connector/purge", bytes.NewReader([]byte(`{"urls":["https://shop.example.com/a/"]}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	cresp, err := http.DefaultClient.Do(req)
	if err != nil || cresp.StatusCode != http.StatusOK {
		t.Fatalf("connector = %v %v", cresp, err)
	}
	cresp.Body.Close()
	select {
	case <-cf.purged:
	case <-time.After(5 * time.Second):
		t.Fatal("the connector's purge never reached Cloudflare")
	}
	if p, _ := cf.take(); len(p) != 1 || p[0] != `{"files":["https://shop.example.com/a/"]}` {
		t.Fatalf("connector sent %v", p)
	}
}

// The refresh job runs only for a site behind Cloudflare, keeps a list
// only if it passes the check, and gives it to the agent.
func TestCloudflareRangesJob(t *testing.T) {
	s, agent, _ := testServer(t)
	cf := withFakeCloudflare(t, s)
	runJobNow(t, s, jobs.Spec{Kind: "cloudflare.ranges"})
	if cf.gets != 0 {
		t.Fatal("fetched with no site behind Cloudflare")
	}
	site := seedActiveSite(t, s, "real.example.com", state.SiteStatic, "", 0)
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error { st.Config.Cloudflare.RealIP = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if j := runJobNow(t, s, jobs.Spec{Kind: "cloudflare.ranges"}); j.Status != state.JobSucceeded {
		t.Fatalf("job %+v", j)
	}
	stored, ok, err := s.Store.CloudflareRanges()
	if err != nil || !ok || strings.Join(stored.V4, ",") != "104.16.0.0/13,172.64.0.0/13" || stored.FetchedAt.IsZero() {
		t.Fatalf("stored %+v %v %v", stored, ok, err)
	}
	agent.mu.Lock()
	sent, _ := agent.lastParams[agentrpc.SetCloudflareRanges.Name()].(rpc.CloudflareRangesParams)
	agent.mu.Unlock()
	if strings.Join(sent.V6, ",") != "2606:4700::/32" {
		t.Fatalf("agent got %+v", sent)
	}
	if !s.cloudflareRanges().Contains(netip.MustParseAddr("172.64.1.1")) || s.cloudflareRanges().Contains(netip.MustParseAddr("173.245.48.1")) {
		t.Fatal("the panel does not use the fetched list")
	}

	// A list that would trust anyone is refused; the good one stays.
	cf.mu.Lock()
	cf.ips = `{"success":true,"result":{"ipv4_cidrs":["0.0.0.0/0"],"ipv6_cidrs":[]}}`
	cf.mu.Unlock()
	if j := runJobNow(t, s, jobs.Spec{Kind: "cloudflare.ranges"}); j.Status != state.JobFailed {
		t.Fatalf("a bad list: job %+v", j)
	}
	if again, _, _ := s.Store.CloudflareRanges(); strings.Join(again.V4, ",") != "104.16.0.0/13,172.64.0.0/13" {
		t.Fatalf("a bad list replaced the good one: %+v", again)
	}
}

// Cloudflare's addresses are never blocked: a failure from one is not
// counted, and a block over one is refused.
func TestCloudflareNeverBlocked(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	if !s.exempt(nil).Has(netip.MustParseAddr("104.16.0.1")) {
		t.Fatal("Cloudflare's address is not exempt")
	}
	resp, body := admin.do("POST", "/api/blocklist/blocks", map[string]any{"address": "104.16.0.0/16"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Cloudflare") {
		t.Fatalf("block over Cloudflare = %d %s", resp.StatusCode, body)
	}
}
