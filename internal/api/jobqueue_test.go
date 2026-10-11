package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// blockApply makes the fake agent's ApplySiteConfig wait until release is
// called; entered is closed at the first call.
func blockApply(agent *fakeAgent) (entered <-chan struct{}, release func()) {
	in, rel := make(chan struct{}), make(chan struct{})
	var once, relOnce sync.Once
	agent.setHook(func(method string) {
		if method == agentrpc.ApplySiteConfig.Name() {
			once.Do(func() { close(in) })
			<-rel
		}
	})
	return in, func() { relOnce.Do(func() { close(rel) }) }
}

func taskOf(t *testing.T, body []byte) jobs.Task {
	t.Helper()
	var out struct {
		Task jobs.Task `json:"task"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Task.ID == 0 {
		t.Fatalf("no task in %s", body)
	}
	return out.Task
}

func getTask(t *testing.T, c *client, id int64) jobs.Task {
	t.Helper()
	resp, body := c.do("GET", fmt.Sprintf("/api/tasks/%d", id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET task %d = %d %s", id, resp.StatusCode, body)
	}
	var tk jobs.Task
	_ = json.Unmarshal(body, &tk)
	return tk
}

func waitTaskEnd(t *testing.T, s *Server, id int64) state.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, err := s.Jobs.Wait(ctx, id)
	if err != nil {
		t.Fatalf("job %d did not end: %v", id, err)
	}
	return j
}

// R-4.2: two changes to one site run one after the other; the second waits
// (pending in the task view), it neither fails nor runs beside the first. A
// queued one can be canceled, which puts back the field it had stored.
func TestSiteJobsAreSerialisedAndAQueuedOneCanBeCanceled(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "jobs.example.com", state.SiteStatic, "", 0)
	entered, release := blockApply(agent)
	defer release()

	put := func(ttl int) jobs.Task {
		resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"cache_ttl_sec": ttl})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("PUT config = %d %s", resp.StatusCode, body)
		}
		return taskOf(t, body)
	}
	first := put(100)
	<-entered
	second := put(200)
	third := put(300)
	time.Sleep(100 * time.Millisecond)
	for _, id := range []int64{second.ID, third.ID} {
		if tk := getTask(t, c, id); tk.Status != "pending" || tk.JobStatus != state.JobQueued || tk.StartedAt != nil {
			t.Fatalf("a job waiting for the site = %+v, want pending/queued and not started", tk)
		}
	}
	if tk := getTask(t, c, first.ID); tk.Status != "running" {
		t.Fatalf("first job = %s", tk.Status)
	}

	resp, body := c.do("POST", fmt.Sprintf("/api/tasks/%d/cancel", third.ID), map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel = %d %s", resp.StatusCode, body)
	}
	if tk := getTask(t, c, third.ID); tk.JobStatus != state.JobCanceled || tk.Status != "failed" || tk.Error != "Canceled" {
		t.Fatalf("canceled job = %+v", tk)
	}
	if got, _ := s.Store.GetSite(site.ID); got.Config.CacheTTLSec != 200 {
		t.Fatalf("cache_ttl_sec after canceling the third change = %d, want 200 (the change before it)", got.Config.CacheTTLSec)
	}
	if resp, _ := c.do("POST", fmt.Sprintf("/api/tasks/%d/cancel", third.ID), map[string]any{}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409", resp.StatusCode)
	}

	release()
	for _, id := range []int64{first.ID, second.ID} {
		if j := waitTaskEnd(t, s, id); j.Status != state.JobSucceeded {
			t.Fatalf("job %d = %s %s", id, j.Status, j.Error)
		}
	}
	applies, _ := applyCalls(agent)
	if len(applies) != 2 {
		t.Fatalf("%d renders, want 2: the canceled change never ran", len(applies))
	}
	if applies[1].Site.Config.CacheTTLSec != 200 {
		t.Fatalf("the second render had cache_ttl_sec %d", applies[1].Site.Config.CacheTTLSec)
	}
	// The list carries the new fields too.
	resp, body = c.do("GET", "/api/tasks", nil)
	var list []jobs.Task
	if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != http.StatusOK || len(list) < 3 || list[0].JobStatus == "" {
		t.Fatalf("GET /api/tasks = %d %s", resp.StatusCode, body)
	}
}

// Maintenance jobs are not tasks: not listed, not readable by an operator.
func TestQuietJobsAreNotListed(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	j := runJobNow(t, s, jobs.Spec{Kind: "metrics.sample"})
	_, body := c.do("GET", "/api/tasks", nil)
	var list []jobs.Task
	_ = json.Unmarshal(body, &list)
	for _, tk := range list {
		if tk.ID == j.ID {
			t.Fatal("a maintenance job is listed as a task")
		}
	}
	if resp, _ := c.do("GET", fmt.Sprintf("/api/tasks/%d", j.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("an admin reading a maintenance job = %d", resp.StatusCode)
	}
}

// While the queue drains, a request that would start a job is refused with
// 503 and nothing is stored.
func TestDrainingRefusesNewJobs(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "drain.example.com", state.SiteStatic, "", 0)
	if left := s.DrainJobs(context.Background()); left != 0 {
		t.Fatalf("drain left %d", left)
	}
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/warm", site.ID), map[string]any{})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("warm while draining = %d %s, want 503", resp.StatusCode, body)
	}
	if list, _ := s.Store.ListJobs(10, true); len(list) != 0 {
		t.Fatalf("%d jobs stored while draining", len(list))
	}
}

// A restart settles every job the old process left: a site render that was
// running runs again; a provisioning whose work died with the process
// leaves its site in error for a delete to clean up, never "provisioning"
// forever; a snapshot never restore-tested gets its test queued.
func TestRestartSettlesJobsAndQueuesPendingVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	agent := &fakeAgent{results: map[string]any{}, errs: map[string]error{}}
	old := &Server{Store: store, Agent: agent, Log: slog.Default()}
	old.Init()
	site := seedActiveSite(t, old, "restart.example.com", state.SiteStatic, "", 0)
	if _, err := old.Store.MutateSite(site.ID, func(st *state.Site) error { st.Config.CacheTTLSec = 77; return nil }); err != nil {
		t.Fatal(err)
	}
	entered, release := blockApply(agent)
	defer release()
	apply, err := old.enqueueApply("site.apply-config", site, site)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	creating, err := old.Store.CreateSite(state.Site{Domain: "new.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-9", RootPath: "/srv/sites/new.example.com", Status: state.SiteProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	never := make(chan struct{})
	defer close(never)
	create, err := old.startJob(jobReq{kind: "site.create", siteID: creating.ID, locks: siteLocks(creating.ID)},
		func(ctx context.Context, _ state.Site, progress func(int, string)) error { <-never; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); old.Jobs.Running() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the jobs did not start")
		}
	}
	setBackupRepo(t, old, "/var/backups/repo", "correct-horse")
	snap, _ := old.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "0123456789abcdef", Repository: "/var/backups/repo", Kind: "full"})

	// The old process freezes: it starts nothing more and its running jobs
	// never end (a drain without a deadline holds it there), as if it had
	// died. A new one opens the same database.
	go old.Jobs.Drain(context.Background())
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := old.Jobs.Enqueue(jobs.Spec{Kind: "metrics.sample"}); err != nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the old queue did not stop")
		}
	}
	store2, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	agent2 := &fakeAgent{results: map[string]any{}, errs: map[string]error{}}
	fresh := &Server{Store: store2, Agent: agent2, Log: slog.Default()}
	ts := httptest.NewServer(fresh.Routes())
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fresh.DrainJobs(ctx)
		store2.Close()
	})

	if j := waitTaskEnd(t, fresh, apply.ID); j.Status != state.JobSucceeded || j.Attempts != 2 {
		t.Fatalf("the interrupted render = %s after %d attempts (%s)", j.Status, j.Attempts, j.Error)
	}
	if applies, _ := applyCalls(agent2); len(applies) != 1 || applies[0].Site.Config.CacheTTLSec != 77 {
		t.Fatalf("the resumed render ran %d times", len(applies))
	}
	if j, _ := store2.GetJob(create.ID); j.Status != state.JobFailed || j.Error != "interrupted by panel restart" {
		t.Fatalf("the interrupted provisioning = %s %q", j.Status, j.Error)
	}
	if got, _ := store2.GetSite(creating.ID); got.Status != state.SiteError {
		t.Fatalf("the site whose provisioning was interrupted is %s, want error", got.Status)
	}
	verifies := 0
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		list, _ := store2.ListJobs(20, false)
		verifies = 0
		for _, j := range list {
			if j.Kind == "backup.verify" && j.Status.Terminal() {
				verifies++
			}
		}
		if verifies > 0 {
			break
		}
	}
	if verifies != 1 || !agent2.called(agentrpc.VerifyBackup.Name()) {
		t.Fatalf("%d restore tests ran for the snapshot left unverified", verifies)
	}
	if b, _ := store2.GetBackup(snap.ID); b.VerifyStatus == state.VerifyPending {
		t.Fatal("the snapshot is still unverified after the restart")
	}
	release()
}
