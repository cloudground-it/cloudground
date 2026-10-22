package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/guard"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// wpUpdateNow asks for an update of what and waits for its job.
func wpUpdateNow(t *testing.T, s *Server, c *client, site state.Site, what string) state.Job {
	t.Helper()
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/wp/update", site.ID), map[string]string{"what": what})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update = %d %s", resp.StatusCode, body)
	}
	return waitTaskEnd(t, s, taskOf(t, body).ID)
}

// methodsCalled is the agent's calls among those named, in order.
func methodsCalled(agent *fakeAgent, names ...string) []string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	var out []string
	for _, c := range agent.calls {
		if slices.Contains(names, c) {
			out = append(out, c)
		}
	}
	return out
}

// §6.2.3: an update names a known target and needs a repository to
// snapshot to; neither refusal takes a snapshot.
func TestWPUpdateRefusals(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "upd.example.com", state.SiteWordPress, "8.4", 0)
	plain := seedActiveSite(t, s, "plain.example.com", state.SitePHP, "8.4", 0)
	path := fmt.Sprintf("/api/sites/%d/wp/update", site.ID)
	if resp, body := c.do("POST", path, map[string]string{"what": "plugins"}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("update with no repository = %d %s, want 412", resp.StatusCode, body)
	}
	setBackupRepo(t, s, "/var/backups/repo", "pw")
	if resp, body := c.do("POST", path, map[string]string{"what": "everything"}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), msgWPUpdateWhat) {
		t.Fatalf("unknown target = %d %s, want 400", resp.StatusCode, body)
	}
	if resp, _ := c.do("POST", fmt.Sprintf("/api/sites/%d/wp/update", plain.ID), map[string]string{"what": "all"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("update of a PHP site = %d, want 400", resp.StatusCode)
	}
	if agent.called(agentrpc.RunBackup.Name()) || agent.called(agentrpc.WPUpdate.Name()) {
		t.Fatalf("a refused update reached the agent: %v", agent.calls)
	}
}

// §6.2.3: a healthy update runs behind a tested pre-update snapshot,
// under the site's and the repository's locks, and keeps the site.
func TestWPUpdateKeepsAHealthySite(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "ok.example.com", state.SiteWordPress, "8.4", 0)
	setBackupRepo(t, s, "/var/backups/repo", "pw")
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "1111222233334444", SizeBytes: 9})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	useProber(t, &fakeProber{verify: guard.Report{Verdict: guard.VerdictPass}})

	j := wpUpdateNow(t, s, c, site, "plugins")
	if j.Status != state.JobSucceeded {
		t.Fatalf("healthy update = %s %q", j.Status, j.Error)
	}
	if !slices.Contains(j.Locks, jobs.SiteLock(site.ID)) || !slices.Contains(j.Locks, jobs.RepoLock("/var/backups/repo")) {
		t.Fatalf("locks %v: want the site's and the repository's", j.Locks)
	}
	got := methodsCalled(agent, agentrpc.RunBackup.Name(), agentrpc.VerifyBackup.Name(), agentrpc.WPUpdate.Name(), agentrpc.RestoreSnapshot.Name())
	want := []string{agentrpc.RunBackup.Name(), agentrpc.VerifyBackup.Name(), agentrpc.WPUpdate.Name()}
	if !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if !hasBackup(t, s, site.ID, "pre-update", "1111222233334444") {
		t.Fatal("no verified pre-update snapshot was recorded")
	}
}

// §6.2.3: an update that breaks the site is restored from its snapshot,
// in full, and the job fails saying why; one whose snapshot fails its
// restore test never runs.
func TestWPUpdateRollsBackABrokenSite(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "broken.example.com", state.SiteWordPress, "8.4", 0)
	setBackupRepo(t, s, "/var/backups/repo", "pw")
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "aaaabbbbccccdddd", SizeBytes: 9})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	// The update itself reports an error too: the check decides.
	agent.setErr(agentrpc.WPUpdate.Name(), errors.New("plugin update partially failed"))
	useProber(t, &fakeProber{verify: guard.Report{Verdict: guard.VerdictBlock, Reasons: []string{"BLOCK: /: production now answers HTTP 500 (was HTTP 200)"}}})

	j := wpUpdateNow(t, s, c, site, "all")
	if j.Status != state.JobFailed || !strings.Contains(j.Error, "rolled back to snapshot aaaabbbbccccdddd") || !strings.Contains(j.Error, "HTTP 500") {
		t.Fatalf("broken update = %s %q", j.Status, j.Error)
	}
	p, _ := agent.last(agentrpc.RestoreSnapshot.Name()).(rpc.RestoreParams)
	if p.SnapshotID != "aaaabbbbccccdddd" || p.Mode != "full" {
		t.Fatalf("rollback restored %+v, want the pre-update snapshot in full", p)
	}
	events, _ := s.Store.ListAuditEvents(20)
	if !slices.ContainsFunc(events, func(e state.AuditEvent) bool { return e.Action == "wp.update.rollback" && e.Subject == site.Domain }) {
		t.Fatal("no wp.update.rollback audit row")
	}

	// A failing restore test: production is not changed.
	agent.forget()
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: false, Detail: "tables missing"})
	j = wpUpdateNow(t, s, c, site, "core")
	if j.Status != state.JobFailed || !strings.Contains(j.Error, "production was not changed") || agent.called(agentrpc.WPUpdate.Name()) {
		t.Fatalf("update over an untested snapshot = %s %q, calls %v", j.Status, j.Error, agent.calls)
	}

	// A failing rollback says the snapshot is kept.
	agent.forget()
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	agent.setErr(agentrpc.RestoreSnapshot.Name(), errors.New("restic: repository locked"))
	j = wpUpdateNow(t, s, c, site, "themes")
	if j.Status != state.JobFailed || !strings.Contains(j.Error, "automatic rollback failed") || !strings.Contains(j.Error, "is kept") {
		t.Fatalf("failed rollback = %s %q", j.Status, j.Error)
	}
}

// §6.2.3: the schedule's refusals, and what it stores.
func TestWPUpdatePolicy(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "sched.example.com", state.SiteWordPress, "8.4", 0)
	path := fmt.Sprintf("/api/sites/%d/wp/updates", site.ID)
	if resp, _ := c.do("PUT", path, map[string]any{"enabled": true}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("turned on with no repository = %d, want 412", resp.StatusCode)
	}
	setBackupRepo(t, s, "/var/backups/repo", "pw")
	for _, bad := range []map[string]any{{}, {"schedule": "hourly"}, {"hour": 24}, {"what": "everything"}} {
		if resp, body := c.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s, want 400", bad, resp.StatusCode, body)
		}
	}
	resp, body := c.do("PUT", path, map[string]any{"enabled": true, "hour": 4, "what": "plugins"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("schedule = %d %s", resp.StatusCode, body)
	}
	got, _ := s.Store.GetSite(site.ID)
	if p := got.Config.WPUpdates; !p.Enabled || p.Schedule != "weekly" || p.Hour == nil || *p.Hour != 4 || p.What != "plugins" {
		t.Fatalf("stored %+v", p)
	}
	stg := seedActiveSite(t, s, "staging.sched.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(stg.ID, func(st *state.Site) error { st.StagingOf = site.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp, _ := c.do("PUT", fmt.Sprintf("/api/sites/%d/wp/updates", stg.ID), map[string]any{"enabled": true}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("staging schedule = %d, want 400", resp.StatusCode)
	}
}

// §6.2.3: the sweep queues a due site's scheduled update once, and the
// job skips plugins with nothing pending.
func TestWPUpdateSweep(t *testing.T) {
	s, agent, _ := testServer(t)
	site := seedActiveSite(t, s, "sweep.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Config.WPUpdates = state.WPUpdatePolicy{Enabled: true, Schedule: "daily", What: "plugins"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// No repository: nothing is queued.
	runJobNow(t, s, jobs.Spec{Kind: "wp.update.sweep"})
	if last, _ := s.Store.LastScheduleRun(wpUpdateScheduleKey(site.ID)); !last.IsZero() {
		t.Fatal("an update was queued with no repository")
	}
	setBackupRepo(t, s, "/var/backups/repo", "pw")
	agent.setResult(agentrpc.WPPlugins.Name(), rpc.WPPluginsResult{Plugins: []rpc.WPPlugin{{Name: "akismet", Update: "none"}}})
	runJobNow(t, s, jobs.Spec{Kind: "wp.update.sweep"})
	waitForTasks(t, s)
	last, _ := s.Store.LastScheduleRun(wpUpdateScheduleKey(site.ID))
	if time.Since(last) > time.Minute {
		t.Fatal("the due site's update was not queued")
	}
	list, _ := s.Store.ListJobs(50, true)
	n := 0
	for _, j := range list {
		if j.Kind == "wp.update.scheduled" {
			n++
			if j.Status != state.JobSucceeded {
				t.Fatalf("scheduled update = %s %q", j.Status, j.Error)
			}
		}
	}
	if n != 1 || agent.called(agentrpc.RunBackup.Name()) {
		t.Fatalf("%d scheduled updates, snapshot taken %v: nothing was pending", n, agent.called(agentrpc.RunBackup.Name()))
	}
	// Not due again the same day.
	runJobNow(t, s, jobs.Spec{Kind: "wp.update.sweep"})
	waitForTasks(t, s)
	list, _ = s.Store.ListJobs(50, true)
	if c := countJobKind(list, "wp.update.scheduled"); c != 1 {
		t.Fatalf("%d scheduled updates after a second sweep", c)
	}
}

func countJobKind(list []state.Job, kind string) int {
	n := 0
	for _, j := range list {
		if j.Kind == kind {
			n++
		}
	}
	return n
}
