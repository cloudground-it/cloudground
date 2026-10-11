package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #25 (SPEC §3.13): a site's rules are normalised, rendered by the agent
// before they are stored, refused when they would hold the server or the
// caller, and an agent refusal stores nothing.
func TestSiteBlocking(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	s.localAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.10")} }
	site := seedActiveSite(t, s, "guarded.example.com", state.SiteWordPress, "8.4", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/blocking", site.ID)
	resp, body := admin.do("PUT", path, map[string]any{
		"addresses":   []string{"203.0.113.9/24", "198.51.100.7", "198.51.100.7"},
		"user_agents": []string{" GPTBot "},
		"countries":   []string{"cn", "RU"},
	})
	var got state.SiteBlocking
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	if fmt.Sprint(got.Addresses, got.UserAgents, got.Countries) != "[198.51.100.7/32 203.0.113.0/24] [GPTBot] [CN RU]" {
		t.Fatalf("stored %+v", got)
	}
	agent.mu.Lock()
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if fmt.Sprint(ref.Site.Config.Blocking) != fmt.Sprint(got) {
		t.Fatalf("the agent rendered %+v", ref.Site.Config.Blocking)
	}
	if st, _ := s.Store.GetSite(site.ID); fmt.Sprint(st.Config.Blocking) != fmt.Sprint(got) {
		t.Fatalf("store has %+v", st.Config.Blocking)
	}

	for _, bad := range []map[string]any{
		{"addresses": []string{"nope"}},
		{"addresses": []string{"192.0.2.0/24"}}, // this server
		{"addresses": []string{"127.0.0.1"}},    // loopback, and the caller here
		{"user_agents": []string{`a"b`}},
		{"countries": []string{"XYZ"}},
		{"allow_countries_only": true},
	} {
		if resp, b := admin.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, b)
		}
	}

	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Wrap(agentrpc.CodeUnavailable, "could not download the country data from db-ip.com", nil)
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"countries": []string{"IT"}, "allow_countries_only": true}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); st.Config.Blocking.AllowCountriesOnly {
		t.Fatal("a refused render was stored")
	}

	// Nothing: the rules go.
	resp, body = admin.do("PUT", path, map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d %s", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.Blocking.Any() || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("after clearing %+v", st.Config.Blocking)
	}
}
