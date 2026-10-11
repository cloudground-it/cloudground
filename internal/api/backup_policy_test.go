package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// R-8.2: the backup policy route (SPEC.md §5.5) stores what it is sent,
// keeps what it is not, renders nothing, and refuses what the sweeps could
// not run or the caller may not change.
func TestBackupPolicy(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "policy.example.com", state.SiteStatic, "", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/backups/policy", site.ID)
	renders := countCalls(agent, agentrpc.ApplySiteConfig.Name())

	resp, body := admin.do("PUT", path, map[string]any{"enabled": true, "retention_days": 30})
	var got state.BackupPolicy
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil ||
		got != (state.BackupPolicy{Enabled: true, Schedule: "daily", RetentionDays: 30}) {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	resp, body = admin.do("PUT", path, map[string]any{"schedule": "hourly"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT schedule = %d %s", resp.StatusCode, body)
	}
	stored, _ := s.Store.GetSite(site.ID)
	if stored.Config.Backups != (state.BackupPolicy{Enabled: true, Schedule: "hourly", RetentionDays: 30}) {
		t.Fatalf("stored = %+v", stored.Config.Backups)
	}
	// Nothing to render: no job, and the site is not left pending.
	waitForTasks(t, s)
	if countCalls(agent, agentrpc.ApplySiteConfig.Name()) != renders || stored.AppliedGeneration != stored.DesiredGeneration {
		t.Fatalf("the policy rendered the site or left it pending (%d/%d)", stored.AppliedGeneration, stored.DesiredGeneration)
	}
	// The sweep now sees it.
	if due, _ := s.scheduledBackupSites(); len(due) != 1 || due[0].ID != site.ID {
		t.Fatalf("scheduled sites = %+v", due)
	}

	for _, tc := range []struct {
		body  any
		code  int
		field string
	}{
		{map[string]any{}, http.StatusBadRequest, ""},
		{map[string]any{"schedule": "monthly"}, http.StatusBadRequest, "schedule"},
		{map[string]any{"retention_days": -1}, http.StatusBadRequest, "retention_days"},
		{map[string]any{"retention_days": maxRetentionDays + 1}, http.StatusBadRequest, "retention_days"},
		{map[string]any{"enabled": "yes"}, http.StatusBadRequest, ""},
		{map[string]any{"keep": 3}, http.StatusBadRequest, ""},
	} {
		resp, body := admin.do("PUT", path, tc.body)
		var a patchAnswer
		_ = json.Unmarshal(body, &a)
		if resp.StatusCode != tc.code || (tc.field != "" && a.Fields[tc.field] == "") {
			t.Errorf("PUT %v = %d %s", tc.body, resp.StatusCode, body)
		}
	}
	if after, _ := s.Store.GetSite(site.ID); after.Config.Backups != stored.Config.Backups {
		t.Fatalf("a refused change was stored: %+v", after.Config.Backups)
	}

	// A staging copy has no schedule of its own.
	stg, err := s.Store.CreateSite(state.Site{Domain: "staging.policy.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "stg", RootPath: "/srv/sites/staging.policy.example.com", Status: state.SiteActive, StagingOf: site.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := admin.do("PUT", fmt.Sprintf("/api/sites/%d/backups/policy", stg.ID), map[string]any{"enabled": true}); resp.StatusCode != http.StatusBadRequest ||
		!jsonHas(body, "error", msgPolicyStaging) {
		t.Fatalf("staging = %d %s", resp.StatusCode, body)
	}

	// An operator of the site may switch it and change the schedule, not
	// the retention window.
	resp, body = admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	if resp, body := op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator login = %d %s", resp.StatusCode, body)
	}
	if resp, body := op.do("PUT", path, map[string]any{"enabled": false, "schedule": "weekly", "retention_days": 30}); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator, same retention = %d %s", resp.StatusCode, body)
	}
	if resp, body := op.do("PUT", path, map[string]any{"retention_days": 1}); resp.StatusCode != http.StatusForbidden || !jsonHas(body, "error", msgPolicyRetention) {
		t.Fatalf("operator retention = %d %s", resp.StatusCode, body)
	}
	if after, _ := s.Store.GetSite(site.ID); after.Config.Backups != (state.BackupPolicy{Enabled: false, Schedule: "weekly", RetentionDays: 30}) {
		t.Fatalf("after the operator = %+v", after.Config.Backups)
	}
	if due, _ := s.scheduledBackupSites(); len(due) != 0 {
		t.Fatalf("a site with its policy off is scheduled: %+v", due)
	}
}
