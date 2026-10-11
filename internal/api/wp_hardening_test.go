package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #75: the hardening is replaced as a whole, each item on its own; the
// exceptions are stored canonical and once; bad input is refused before
// the agent is asked; a refusal stores nothing.
func TestWPHardening(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "blog.example.com", state.SiteWordPress, "8.4", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/wp/hardening", site.ID)
	sent := func() state.WPHardening {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
		return ref.Site.Config.WPHardening
	}

	resp, body := admin.do("PUT", path, map[string]any{"xmlrpc_off": true, "xmlrpc_allow": []string{"192.0.2.9/24", "192.0.2.0/24", "2001:db8::1"},
		"file_edit_off": true, "hide_version": true, "block_readme": true})
	var got state.WPHardening
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("set = %d %s", resp.StatusCode, body)
	}
	want := state.WPHardening{XMLRPCOff: true, XMLRPCAllow: []string{"192.0.2.0/24", "2001:db8::1"}, FileEditOff: true, HideVersion: true, BlockReadme: true}
	if !hardeningEqual(got, want) || !hardeningEqual(sent(), want) {
		t.Fatalf("answered %+v, rendered %+v", got, sent())
	}
	if st, _ := s.Store.GetSite(site.ID); !hardeningEqual(st.Config.WPHardening, want) || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("stored %+v", st.Config.WPHardening)
	}

	if !agent.called(agentrpc.PurgeCache.Name()) {
		t.Error("the page cache was not purged after a change")
	}
	agent.forget()
	if resp, _ := admin.do("PUT", path, want); resp.StatusCode != http.StatusOK || agent.called(agentrpc.ApplySiteConfig.Name()) {
		t.Fatalf("the same hardening again = %d, rendered %v", resp.StatusCode, agent.called(agentrpc.ApplySiteConfig.Name()))
	}

	for _, bad := range []map[string]any{
		{"login_path": "wp-admin"},
		{"login_path": "a/b"},
		{"login_path": "Door"},
		{"xmlrpc_off": true, "xmlrpc_allow": []string{"example.com"}},
		{"xmlrpc_off": true, "xmlrpc_allow": []string{"192.0.2.1; deny all"}},
		{"unknown": true},
	} {
		if resp, body := admin.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, body)
		}
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("192.0.2.%d", i)
	}
	if resp, _ := admin.do("PUT", path, map[string]any{"xmlrpc_allow": many}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("51 exceptions = %d", resp.StatusCode)
	}

	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Invalid("nginx refused it")
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"login_path": "my-door"}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); st.Config.WPHardening.LoginPath != "" {
		t.Fatalf("a refused render was stored: %+v", st.Config.WPHardening)
	}

	// Each item on its own: only the login path now.
	if resp, body := admin.do("PUT", path, map[string]any{"login_path": "my-door"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login path = %d %s", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(site.ID); !hardeningEqual(st.Config.WPHardening, state.WPHardening{LoginPath: "my-door"}) {
		t.Fatalf("stored %+v", st.Config.WPHardening)
	}
	if hardeningSummary(state.WPHardening{}) != "none" || hardeningSummary(want) != "xmlrpc,file_edit,version,readme" {
		t.Fatal(hardeningSummary(want))
	}

	static := seedActiveSite(t, s, "static.example.com", state.SiteStatic, "", 0)
	if resp, _ := admin.do("PUT", fmt.Sprintf("/api/sites/%d/wp/hardening", static.ID), map[string]any{"block_readme": true}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a static site = %d", resp.StatusCode)
	}
}
