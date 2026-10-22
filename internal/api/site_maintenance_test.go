package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #79 (SPEC §3.16): the page is rendered by the agent before it is stored,
// its text and addresses are checked, the key is made once and never shown
// in the site, the cache comes back when it ends, and an agent refusal
// stores nothing.
func TestSiteMaintenance(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "away.example.com", state.SiteWordPress, "8.4", 0)
	site, err := s.Store.MutateSite(site.ID, func(st *state.Site) error { st.Config.CacheEnabled = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/maintenance", site.ID)

	for _, bad := range []map[string]any{
		{"enabled": true, "allow": []string{"nope"}},
		{"enabled": true, "title": strings.Repeat("x", 101)},
		{"enabled": true, "message": "bell \x07"},
		{"enabled": true, "retry_after_sec": 5},
		{"enabled": true, "retry_after_sec": 99999999},
	} {
		if resp, b := admin.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, b)
		}
	}

	resp, body := admin.do("PUT", path, map[string]any{
		"enabled": true, "title": "Back soon", "message": "Moving servers.\nThank you.", "retry_after_sec": 1800,
		"allow": []string{"203.0.113.9", "198.51.100.0/24", "203.0.113.9"}, "allow_my_address": true,
	})
	var got maintenanceView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	if !got.Enabled || got.RetryAfterSec != 1800 || fmt.Sprint(got.Allow) != "[127.0.0.1 198.51.100.0/24 203.0.113.9]" {
		t.Fatalf("answer %+v", got)
	}
	st, _ := s.Store.GetSite(site.ID)
	key := st.Config.Maintenance.Key
	if len(key) != 32 || got.PreviewURL != "https://away.example.com/.cg-maintenance/"+key {
		t.Fatalf("key %q, preview %q", key, got.PreviewURL)
	}
	if st.AppliedGeneration != st.DesiredGeneration {
		t.Fatal("the site was stored but not marked rendered")
	}
	agent.mu.Lock()
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if !ref.Site.Config.Maintenance.Enabled || ref.Site.Config.Maintenance.Key != key {
		t.Fatalf("the agent rendered %+v", ref.Site.Config.Maintenance)
	}
	// The key never leaves in the site.
	resp, body = admin.do("GET", fmt.Sprintf("/api/sites/%d", site.ID), nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), key) || !strings.Contains(string(body), `"maintenance"`) {
		t.Fatalf("the site shows %s", body)
	}
	if agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("turning it on emptied the cache")
	}

	// Saving the text again keeps the key.
	if resp, b := admin.do("PUT", path, map[string]any{"title": "Later"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("again = %d %s", resp.StatusCode, b)
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.Maintenance.Key != key || st.Config.Maintenance.Title != "Later" {
		t.Fatalf("after saving %+v", st.Config.Maintenance)
	}

	// A field left out keeps its value; the list can be emptied.
	if resp, b := admin.do("PUT", path, map[string]any{"message": "Kept"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("one field = %d %s", resp.StatusCode, b)
	}
	if st, _ := s.Store.GetSite(site.ID); !st.Config.Maintenance.Enabled || st.Config.Maintenance.Title != "Later" || len(st.Config.Maintenance.Allow) != 3 {
		t.Fatalf("a field left out changed others: %+v", st.Config.Maintenance)
	}

	// A refused render stores nothing.
	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Wrap(agentrpc.CodeUnavailable, "nginx -t failed", nil)
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"enabled": false}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); !st.Config.Maintenance.Enabled {
		t.Fatal("a refused render was stored")
	}

	// Off: the text stays, the cache is emptied and filled again.
	resp, body = admin.do("PUT", path, map[string]any{"enabled": false, "message": "Soon", "allow": []string{}})
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "preview_url") {
		t.Fatalf("off = %d %s", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.Maintenance.Enabled || st.Config.Maintenance.Message != "Soon" || len(st.Config.Maintenance.Allow) != 0 {
		t.Fatalf("after off %+v", st.Config.Maintenance)
	}
	if !agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("turning it off did not empty the cache")
	}
	waitForTasks(t, s)
	if !agent.called(agentrpc.WarmCache.Name()) {
		t.Fatal("turning it off did not warm the cache")
	}
}
