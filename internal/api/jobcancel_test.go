package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// blockMethod makes the fake agent wait in method until release; entered
// is closed at the first call.
func blockMethod(agent *fakeAgent, method string) (entered <-chan struct{}, release func()) {
	in, rel := make(chan struct{}), make(chan struct{})
	var once, relOnce sync.Once
	agent.setHook(func(m string) {
		if m == method {
			once.Do(func() { close(in) })
			<-rel
		}
	})
	return in, func() { relOnce.Do(func() { close(rel) }) }
}

func cancelTask(t *testing.T, c *client, id int64) (int, jobs.Task, string) {
	t.Helper()
	resp, body := c.do("POST", fmt.Sprintf("/api/tasks/%d/cancel", id), map[string]any{})
	var tk jobs.Task
	_ = json.Unmarshal(body, &tk)
	return resp.StatusCode, tk, string(body)
}

// fillSlots holds the global limit of jobs, so the next job a request
// starts stays queued.
func fillSlots(t *testing.T, s *Server) (release func()) {
	t.Helper()
	var rel []func()
	for i := 0; i < jobs.DefaultLimit; i++ {
		rel = append(rel, holdLocks(t, s, fmt.Sprintf("filler:%d", i)))
	}
	return func() {
		for _, r := range rel {
			r()
		}
	}
}

// R-4.2 review: a cancel during a running restore's safety snapshot stops
// it before the restore. The snapshot (a change already sent) completes
// and is restore-tested; production is never touched.
func TestCancelRunningRestoreStopsBeforeTheRestore(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "restore.example.com", state.SiteStatic, "", 0)
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "correct-horse")
	old, _ := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abcdef1234567890", Repository: "/var/backups/cloudground/repo", Kind: "full"})
	agent.mu.Lock()
	agent.results[agentrpc.RunBackup.Name()] = rpc.BackupResult{SnapshotID: "fedcba0987654321"}
	agent.results[agentrpc.VerifyBackup.Name()] = rpc.VerifyResult{Passed: true, Detail: "ok"}
	agent.mu.Unlock()
	entered, release := blockMethod(agent, agentrpc.RunBackup.Name())
	defer release()
	resp, body := c.do("POST", fmt.Sprintf("/api/backups/%d/restore", old.ID), map[string]string{"confirm": site.Domain})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d %s", resp.StatusCode, body)
	}
	task := taskOf(t, body)
	<-entered
	if code, tk, b := cancelTask(t, c, task.ID); code != http.StatusOK || !tk.CancelRequested {
		t.Fatalf("cancel of the running restore = %d %s", code, b)
	}
	release()
	j := waitTaskEnd(t, s, task.ID)
	if j.Status != state.JobCanceled {
		t.Fatalf("restore canceled mid-snapshot = %s %q", j.Status, j.Error)
	}
	if agent.called(agentrpc.RestoreSnapshot.Name()) {
		t.Fatal("RestoreSnapshot ran after the cancel")
	}
	if !agent.called(agentrpc.VerifyBackup.Name()) {
		t.Fatal("the safety snapshot taken before the cancel was not restore-tested")
	}
	if log := jobs.View(j).Log; !strings.Contains(log, "Stopped before the restore") {
		t.Fatalf("the log does not say where the job stopped: %q", log)
	}
}

// A restore test can never be canceled, queued or running (AGENTS.md rule
// 4): 409 with the reason.
func TestRestoreTestsCannotBeCanceled(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "verify.example.com", state.SiteStatic, "", 0)
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "correct-horse")
	b, _ := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abcdef1234567890", Repository: "/var/backups/cloudground/repo", Kind: "full"})
	release := holdLocks(t, s, jobs.RepoLock("/var/backups/cloudground/repo"))
	defer release()
	resp, body := c.do("POST", fmt.Sprintf("/api/backups/%d/verify", b.ID), map[string]any{})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("verify = %d %s", resp.StatusCode, body)
	}
	task := taskOf(t, body)
	code, _, msg := cancelTask(t, c, task.ID)
	if code != http.StatusConflict || !strings.Contains(msg, "restore test cannot be canceled") {
		t.Fatalf("cancel of a restore test = %d %s, want 409 with the reason", code, msg)
	}
	release()
	if j := waitTaskEnd(t, s, task.ID); j.Status == state.JobCanceled {
		t.Fatal("the restore test was canceled")
	}
}

// Retention never forgets a snapshot whose restore test has not run.
func TestRetentionProtectsUnverifiedSnapshots(t *testing.T) {
	s, agent, _ := testServer(t)
	const repo = "/var/backups/cloudground/repo"
	setBackupRepo(t, s, repo, "correct-horse")
	site := retentionSite(t, s, 7)
	pending, _ := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "1111111111111111", Repository: repo, Kind: "full"})
	agent.results[agentrpc.BackupRetention.Name()] = rpc.RetentionResult{}
	j := runJobNow(t, s, jobs.Spec{Kind: "backup.retention", SiteID: site.ID, Locks: []string{jobs.RepoLock(repo)}})
	if j.Status != state.JobSucceeded {
		t.Fatalf("retention = %s %q", j.Status, j.Error)
	}
	p, _ := agent.lastParams[agentrpc.BackupRetention.Name()].(rpc.RetentionParams)
	found := false
	for _, id := range p.Protect {
		found = found || id == pending.SnapshotID
	}
	if !found {
		t.Fatalf("protected %v: the unverified snapshot %s is not among them", p.Protect, pending.SnapshotID)
	}
}

// A create or a delete canceled before it started changed nothing: the
// created site's record goes, the deleted one gets its status back. A
// second delete of the same site joins the first.
func TestCanceledQueuedCreateAndDeleteLeaveNothingBehind(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	release := fillSlots(t, s)
	defer release()

	resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "never.example.com", "type": "static"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	create := taskOf(t, body)
	if code, _, b := cancelTask(t, c, create.ID); code != http.StatusOK {
		t.Fatalf("cancel create = %d %s", code, b)
	}
	if _, err := s.Store.GetSiteByDomain("never.example.com"); err == nil {
		t.Fatal("a create canceled before it ran left its site record")
	}

	site := seedActiveSite(t, s, "keep.example.com", state.SiteStatic, "", 0)
	resp, body = c.do("DELETE", fmt.Sprintf("/api/sites/%d", site.ID), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	del := taskOf(t, body)
	_, body = c.do("DELETE", fmt.Sprintf("/api/sites/%d", site.ID), nil)
	if again := taskOf(t, body); again.ID != del.ID {
		t.Fatalf("a second delete made task %d beside %d", again.ID, del.ID)
	}
	if code, _, b := cancelTask(t, c, del.ID); code != http.StatusOK {
		t.Fatalf("cancel delete = %d %s", code, b)
	}
	if got, _ := s.Store.GetSite(site.ID); got.Status != state.SiteActive {
		t.Fatalf("a site whose delete was canceled is %s, want active", got.Status)
	}
	release()
	waitForTasks(t, s)
	if agent.called(agentrpc.CreateSite.Name()) || agent.called(agentrpc.DeleteSite.Name()) {
		t.Fatal("a canceled job reached the agent")
	}
}

// A canceled settings change with nothing else pending leaves the site
// applied: its fields are back to what is rendered.
func TestCanceledChangeLeavesTheSiteApplied(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "applied.example.com", state.SiteStatic, "", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	release := holdLocks(t, s, jobs.SiteLock(site.ID))
	defer release()
	resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"cache_ttl_sec": 42})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PUT config = %d %s", resp.StatusCode, body)
	}
	if code, _, b := cancelTask(t, c, taskOf(t, body).ID); code != http.StatusOK {
		t.Fatalf("cancel = %d %s", code, b)
	}
	got, _ := s.Store.GetSite(site.ID)
	if got.Config.CacheTTLSec != 0 || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("after the cancel: ttl %d, applied %d of %d; want the old value, applied",
			got.Config.CacheTTLSec, got.AppliedGeneration, got.DesiredGeneration)
	}
	release()
	waitForTasks(t, s)
	if applies, _ := applyCalls(agent); len(applies) != 0 {
		t.Fatalf("%d renders for a canceled change", len(applies))
	}
}

// A canceled scheduled backup is due again at the next sweep.
func TestCanceledScheduledBackupClearsItsKey(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "correct-horse")
	site := retentionSite(t, s, 0)
	release := holdLocks(t, s, jobs.RepoLock("/var/backups/cloudground/repo"))
	defer release()
	runJobNowQueued(t, s, jobs.Spec{Kind: "backup.sweep"})
	if last, _ := s.Store.LastScheduleRun(backupScheduleKey(site.ID)); last.IsZero() {
		t.Fatal("the sweep did not mark the site's schedule")
	}
	list, _ := s.Store.ListJobs(10, false)
	var queued int64
	for _, j := range list {
		if j.Kind == "backup.scheduled" {
			queued = j.ID
		}
	}
	if code, _, b := cancelTask(t, c, queued); code != http.StatusOK {
		t.Fatalf("cancel scheduled backup = %d %s", code, b)
	}
	if last, _ := s.Store.LastScheduleRun(backupScheduleKey(site.ID)); !last.IsZero() {
		t.Fatal("a canceled scheduled backup left its schedule marked: the next sweep would skip the site")
	}
}

// runJobNowQueued runs a job and waits for it alone (not for what it
// queued).
func runJobNowQueued(t *testing.T, s *Server, spec jobs.Spec) {
	t.Helper()
	j, err := s.Jobs.Enqueue(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitTaskEnd(t, s, j.ID)
}

// Run-now that would wait long answers 202 with the jobs to poll.
func TestRunMaintenanceAnswers202WhenItWouldWaitLong(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	s.maintenanceWait = 200 * time.Millisecond
	seedActiveSite(t, s, "slow.example.com", state.SiteWordPress, "8.4", 0)
	if err := s.Store.SetRenderedVersion(version.Version + "/7"); err != nil {
		t.Fatal(err)
	}
	entered, release := blockApply(agent)
	defer release()
	done := make(chan struct{})
	var code int
	var body []byte
	go func() {
		defer close(done)
		r, b := c.do("POST", "/api/system/maintenance", map[string]string{})
		code, body = r.StatusCode, b
	}()
	<-entered
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run-now held the request while the reconcile ran")
	}
	var out struct {
		Ran  bool    `json:"ran"`
		Jobs []int64 `json:"jobs"`
	}
	_ = json.Unmarshal(body, &out)
	if code != http.StatusAccepted || out.Ran || len(out.Jobs) == 0 {
		t.Fatalf("run-now = %d %s, want 202 with the job IDs", code, body)
	}
	release()
	for _, id := range out.Jobs {
		waitTaskEnd(t, s, id)
	}
	waitForTasks(t, s)
}
