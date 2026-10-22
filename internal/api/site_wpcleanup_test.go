package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #78 (SPEC §3.17): the schedule is off by default, needs the off-site
// repository, a run logs a dry run first and takes a tested snapshot before
// a site's first cleanup only, and the sweep queues a site once per slot.
func TestWPCleanup(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	wp := seedActiveSite(t, s, "clean.example.com", state.SiteWordPress, "8.4", 0)
	static := seedActiveSite(t, s, "plain.example.com", state.SiteStatic, "", 0)
	if err := s.Store.MarkApplied(wp.ID, wp.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/wp/cleanup", wp.ID)

	var v wpCleanupView
	resp, body := admin.do("GET", path, nil)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || v.Enabled || v.KeepRevisions != 5 || v.BackupsReady || v.NextRun != nil {
		t.Fatalf("default = %d %s", resp.StatusCode, body)
	}
	if resp, b := admin.do("GET", fmt.Sprintf("/api/sites/%d/wp/cleanup", static.ID), nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a static site = %d %s", resp.StatusCode, b)
	}
	for _, bad := range []map[string]any{{}, {"keep_revisions": 0, "enabled": "yes"}, {"keep_revisions": 101}, {"keep_revisions": -1}} {
		if resp, b := admin.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, b)
		}
	}
	// Without the off-site repository the schedule and a run are refused.
	if resp, b := admin.do("PUT", path, map[string]any{"enabled": true}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("enable without backups = %d %s", resp.StatusCode, b)
	}
	if resp, b := admin.do("POST", path+"/run", nil); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("run without backups = %d %s", resp.StatusCode, b)
	}

	setBackupRepo(t, s, "/tmp/test-repo", "test-repository-password")
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "fedcba0987654321", SizeBytes: 42})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	agent.setResult(agentrpc.DBStudioWPCleanup.Name(), rpc.DBStudioCleanupResult{
		ExpiredTransients: 12, ExcessRevisions: 340, DeletedTransients: 12, DeletedRevisions: 340, AutoloadCount: 90, AutoloadBytes: 1 << 20,
		AutoloadTop: []rpc.DBAutoloadOption{}, Executed: true,
	})

	resp, body = admin.do("PUT", path, map[string]any{"enabled": true, "keep_revisions": 3})
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || !v.Enabled || v.KeepRevisions != 3 || !v.BackupsReady || v.NextRun == nil {
		t.Fatalf("enable = %d %s", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(wp.ID); !st.Config.WPCleanup.Enabled || st.Config.WPCleanup.KeepRevisions != 3 || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("stored %+v", st.Config.WPCleanup)
	}

	// A dry run changes nothing and takes no snapshot.
	resp, body = admin.do("POST", path+"/preview", nil)
	var pre rpc.DBStudioCleanupResult
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &pre) != nil || pre.ExcessRevisions != 340 {
		t.Fatalf("preview = %d %s", resp.StatusCode, body)
	}
	if p, _ := agent.last(agentrpc.DBStudioWPCleanup.Name()).(rpc.DBStudioCleanupParams); !p.DryRun || p.KeepRevisions != 3 {
		t.Fatalf("preview asked the agent for %+v", p)
	}
	if agent.called(agentrpc.RunBackup.Name()) {
		t.Fatal("a dry run took a snapshot")
	}

	// The first run: a dry run, a tested snapshot, then the cleanup.
	if resp, b := admin.do("POST", path+"/run", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d %s", resp.StatusCode, b)
	}
	waitForTasks(t, s)
	if agent.count(agentrpc.DBStudioWPCleanup.Name()) != 3 || agent.count(agentrpc.RunBackup.Name()) != 1 || !agent.called(agentrpc.VerifyBackup.Name()) {
		t.Fatalf("calls %v: want preview, dry run, snapshot, cleanup", agent.calls)
	}
	if p, _ := agent.last(agentrpc.DBStudioWPCleanup.Name()).(rpc.DBStudioCleanupParams); p.DryRun {
		t.Fatal("the last call was a dry run")
	}
	if !hasBackup(t, s, wp.ID, "pre-cleanup", "fedcba0987654321") {
		t.Fatal("no verified pre-cleanup snapshot")
	}
	resp, body = admin.do("GET", path, nil)
	if json.Unmarshal(body, &v) != nil || v.LastRun == nil {
		t.Fatalf("after the run: %s", body)
	}

	// The second run is not behind another snapshot.
	if resp, b := admin.do("POST", path+"/run", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d %s", resp.StatusCode, b)
	}
	waitForTasks(t, s)
	if agent.count(agentrpc.RunBackup.Name()) != 1 || agent.count(agentrpc.DBStudioWPCleanup.Name()) != 5 {
		t.Fatalf("second run: %v", agent.calls)
	}

	// The sweep: noted at first, queued once its slot has passed, once.
	agent.forget()
	queue := func(now time.Time) int {
		sites, err := s.Store.ListSites()
		if err != nil {
			t.Fatal(err)
		}
		n, err := s.queueWPCleanups(sites, now)
		if err != nil {
			t.Fatal(err)
		}
		waitForTasks(t, s)
		return n
	}
	if err := s.Store.ClearScheduleRun(wpCleanupKey(wp.ID)); err != nil {
		t.Fatal(err)
	}
	if n := queue(time.Now()); n != 0 || agent.called(agentrpc.DBStudioWPCleanup.Name()) {
		t.Fatal("a site seen for the first time was cleaned at once")
	}
	if last, _ := s.Store.LastScheduleRun(wpCleanupKey(wp.ID)); last.IsZero() {
		t.Fatal("the first sight was not noted")
	}
	if n := queue(time.Now()); n != 0 {
		t.Fatal("cleaned before its slot")
	}
	later := time.Now().Add(8 * 24 * time.Hour)
	if n := queue(later); n != 1 || agent.count(agentrpc.DBStudioWPCleanup.Name()) != 2 {
		t.Fatalf("the sweep after its slot queued %d: %v", n, agent.calls)
	}
	if n := queue(time.Now()); n != 0 {
		t.Fatal("cleaned twice in one slot")
	}

	// Off: the sweep leaves the site alone.
	if resp, b := admin.do("PUT", path, map[string]any{"enabled": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable = %d %s", resp.StatusCode, b)
	}
	if n := queue(later.Add(8 * 24 * time.Hour)); n != 0 {
		t.Fatal("a site with the schedule off was cleaned")
	}
}
