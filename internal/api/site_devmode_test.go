package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #80 (SPEC §3.15): development mode is rendered before it is stored, takes
// 15, 30 or 60 minutes, and ending it (by hand or by the sweep) empties the
// cache and queues the re-warm.
func TestSiteDevMode(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "dev.example.com", state.SiteWordPress, "8.4", 0)
	static := seedActiveSite(t, s, "plain.example.com", state.SiteStatic, "", 0)
	site, err := s.Store.MutateSite(site.ID, func(st *state.Site) error { st.Config.CacheEnabled = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/dev-mode", site.ID)

	for _, bad := range []int{-5, 7, 45, 120} {
		if resp, b := admin.do("PUT", path, map[string]any{"minutes": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%d minutes = %d %s", bad, resp.StatusCode, b)
		}
	}
	if resp, b := admin.do("PUT", fmt.Sprintf("/api/sites/%d/dev-mode", static.ID), map[string]any{"minutes": 15}); resp.StatusCode != http.StatusConflict {
		t.Errorf("a static site = %d %s", resp.StatusCode, b)
	}

	before := time.Now()
	resp, body := admin.do("PUT", path, map[string]any{"minutes": 30})
	var got devModeView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Until == nil {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	if d := got.Until.Sub(before); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("ends in %s", d)
	}
	agent.mu.Lock()
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if ref.Site.Config.DevModeUntil.IsZero() {
		t.Fatal("the agent rendered the site without the mode")
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.DevModeUntil.IsZero() || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("stored %+v", st.Config.DevModeUntil)
	}
	if agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("starting the mode emptied the cache")
	}

	// The sweep leaves a mode that has not ended.
	runJobNow(t, s, jobs.Spec{Kind: "devmode.sweep"})
	if st, _ := s.Store.GetSite(site.ID); st.Config.DevModeUntil.IsZero() {
		t.Fatal("the sweep ended a mode that had time left")
	}

	// A refused render stores nothing.
	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Wrap(agentrpc.CodeUnavailable, "nginx -t failed", nil)
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"minutes": 0}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); st.Config.DevModeUntil.IsZero() {
		t.Fatal("a refused render ended the mode")
	}

	// The time has come: the sweep ends it, empties the cache, re-warms.
	if site, err = s.Store.MutateSiteApplied(site.ID, func(st *state.Site) error {
		st.Config.DevModeUntil = time.Now().Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runJobNow(t, s, jobs.Spec{Kind: "devmode.sweep"})
	st, _ := s.Store.GetSite(site.ID)
	if !st.Config.DevModeUntil.IsZero() || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("after the sweep %+v", st.Config.DevModeUntil)
	}
	if !agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("ending the mode did not empty the cache")
	}
	waitForTasks(t, s)
	if !agent.called(agentrpc.WarmCache.Name()) {
		t.Fatal("ending the mode did not warm the cache")
	}

	// By hand: 0 ends it; 0 again changes nothing.
	if resp, _ := admin.do("PUT", path, map[string]any{"minutes": 15}); resp.StatusCode != http.StatusOK {
		t.Fatal("start again")
	}
	resp, body = admin.do("PUT", path, map[string]any{"minutes": 0})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"dev_mode_until":null`) {
		t.Fatalf("end = %d %q", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(site.ID); !st.Config.DevModeUntil.IsZero() {
		t.Fatal("0 did not end the mode")
	}
	agent.forget()
	admin.do("PUT", path, map[string]any{"minutes": 0})
	if agent.called(agentrpc.ApplySiteConfig.Name()) {
		t.Fatal("ending a mode that was off rendered the site")
	}
}
