package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func migrationSite(t *testing.T, s *Server) state.Site {
	t.Helper()
	site, err := s.Store.CreateSite(state.Site{Domain: "mig.example.com", Type: state.SiteWordPress, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/mig.example.com", Status: state.SiteActive,
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "site_1", User: "site_1"}}})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// lastMigration is the params of the agent's last ImportMigration call.
func lastMigration(f *fakeAgent) rpc.MigrationParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, _ := f.lastParams[agentrpc.ImportMigration.Name()].(rpc.MigrationParams)
	return p
}

// migrate starts a migration and returns its task once finished.
func migrate(t *testing.T, s *Server, c *client, site state.Site, extra map[string]any) (int, state.Job) {
	t.Helper()
	body := map[string]any{"archive": "shared/site.tar.gz", "sql": "shared/db.sql", "confirm": site.Domain}
	for k, v := range extra {
		body[k] = v
	}
	resp, b := c.do("POST", fmt.Sprintf("/api/sites/%d/migration", site.ID), body)
	if resp.StatusCode != http.StatusAccepted {
		return resp.StatusCode, state.Job{}
	}
	var out struct{ Task jobs.Task }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	waitForTasks(t, s)
	task, err := s.Store.GetJob(out.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, task
}

// The expansion limit is an administrator's to change, within 1..1000, and
// reaches the agent; without it the agent's default applies.
func TestMigrationExpansionLimit(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := migrationSite(t, s)

	if code, _ := migrate(t, s, admin, site, map[string]any{"max_ratio": 500}); code != http.StatusAccepted {
		t.Fatalf("admin with max_ratio 500 = %d", code)
	}
	if p := lastMigration(agent); p.MaxRatio != 500 {
		t.Fatalf("the agent got MaxRatio %d", p.MaxRatio)
	}
	if code, _ := migrate(t, s, admin, site, nil); code != http.StatusAccepted {
		t.Fatalf("admin without max_ratio = %d", code)
	}
	if p := lastMigration(agent); p.MaxRatio != 0 {
		t.Fatalf("the default limit was sent as %d", p.MaxRatio)
	}
	for _, bad := range []int{-1, rpc.MaxMigrationRatio + 1} {
		if code, _ := migrate(t, s, admin, site, map[string]any{"max_ratio": bad}); code != http.StatusBadRequest {
			t.Errorf("max_ratio %d = %d, want 400", bad, code)
		}
	}

	// An operator of the site migrates it, but cannot move the limit.
	resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	if resp, body := op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator login = %d %s", resp.StatusCode, body)
	}
	if code, _ := migrate(t, s, op, site, map[string]any{"max_ratio": 500}); code != http.StatusForbidden {
		t.Fatalf("operator with max_ratio = %d, want 403", code)
	}
	if code, _ := migrate(t, s, op, site, nil); code != http.StatusAccepted {
		t.Fatalf("operator without max_ratio = %d", code)
	}
}

// An agent refusal that carries a key is stored in the user's language;
// one with a key this build does not know keeps the agent's English.
func TestMigrationErrorIsTranslated(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := migrationSite(t, s)
	if resp, body := admin.do("PUT", "/api/account/profile", map[string]string{"locale": "de"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("locale = %d %s", resp.StatusCode, body)
	}
	agent.errs[agentrpc.ImportMigration.Name()] = &agentrpc.Error{Code: agentrpc.CodeInvalid,
		Message: "The archive expands to more than 12.0 MiB, 200 times its compressed size, the limit. …",
		Key:     "migration.tooLargeRatio", Args: []string{"12.0 MiB", "200", "1000"}}
	_, task := migrate(t, s, admin, site, nil)
	// Stored: the English, the key and its values.
	if task.Status != state.JobFailed || !strings.HasPrefix(task.Error, "The archive expands") ||
		task.ErrorKey != "migration.tooLargeRatio" || strings.Join(task.ErrorArgs, ",") != "12.0 MiB,200,1000" {
		t.Fatalf("stored task = %s %q %q %v", task.Status, task.Error, task.ErrorKey, task.ErrorArgs)
	}
	read := func() string {
		t.Helper()
		_, body := admin.do("GET", fmt.Sprintf("/api/tasks/%d", task.ID), nil)
		var got jobs.Task
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		_, body = admin.do("GET", "/api/tasks", nil)
		var list []jobs.Task
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatal(err)
		}
		for _, l := range list {
			if l.ID == got.ID && l.Error != got.Error {
				t.Fatalf("the task list shows %q, the task %q", l.Error, got.Error)
			}
		}
		return got.Error
	}
	// Read in the reader's language, whoever started it.
	if got := read(); !strings.Contains(got, "Expansionslimit") || !strings.Contains(got, "200-Fache") || !strings.Contains(got, "1000-Fachen") {
		t.Fatalf("read in German: %q", got)
	}
	if resp, body := admin.do("PUT", "/api/account/profile", map[string]string{"locale": "it"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("locale = %d %s", resp.StatusCode, body)
	}
	if got := read(); !strings.Contains(got, "limite di espansione") || !strings.Contains(got, "200 volte") {
		t.Fatalf("read in Italian: %q", got)
	}

	agent.errs[agentrpc.ImportMigration.Name()] = &agentrpc.Error{Code: agentrpc.CodeInvalid, Message: "the agent's own words", Key: "migration.fromTheFuture"}
	_, task = migrate(t, s, admin, site, nil)
	if got := read(); got != "the agent's own words" {
		t.Fatalf("unknown key: %q", got)
	}
}

// An import replaces a site's uploads and tables, and a stale or wrong
// archive still succeeds: a restore-tested pre-migration snapshot is taken
// before the agent is asked, under the repository's lock, and named in the
// task. One that fails its restore test stops the import; without a
// repository the task says the import cannot be undone, before it runs.
func TestMigrationTakesASafetySnapshotFirst(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := migrationSite(t, s)
	importer := agentrpc.ImportMigration.Name()
	index := func(method string) int {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return slices.Index(agent.calls, method)
	}

	// No repository: the import runs, and says it cannot be undone.
	_, task := migrate(t, s, admin, site, nil)
	if task.Status != state.JobSucceeded || agent.called(agentrpc.RunBackup.Name()) {
		t.Fatalf("without a repository: %s, calls %v", task.Status, agent.calls)
	}
	if log := jobs.View(task).Log; !strings.Contains(log, "this import cannot be undone") {
		t.Fatalf("the task does not say the import cannot be undone: %q", log)
	}
	if slices.Contains(task.Locks, jobs.RepoLock("")) {
		t.Fatalf("locks %v: a repository lock without a repository", task.Locks)
	}

	const repo = "/var/backups/repo"
	setBackupRepo(t, s, repo, "correct-horse")
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "5afe5afe5afe5afe", SizeBytes: 42})

	// A snapshot that fails its restore test stops the import.
	agent.forget()
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: false, Detail: "database dump does not import"})
	if _, task = migrate(t, s, admin, site, nil); task.Status != state.JobFailed || agent.called(importer) {
		t.Fatalf("behind an unverified snapshot: %s, calls %v", task.Status, agent.calls)
	}

	// A verified one comes first, and its ID is in the task.
	agent.forget()
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	_, task = migrate(t, s, admin, site, nil)
	if task.Status != state.JobSucceeded {
		t.Fatalf("migration = %s %q", task.Status, task.Error)
	}
	snap, verify, imp := index(agentrpc.RunBackup.Name()), index(agentrpc.VerifyBackup.Name()), index(importer)
	if snap < 0 || verify < snap || imp < verify {
		t.Fatalf("calls %v: want the snapshot, its restore test, then the import", agent.calls)
	}
	if !hasBackup(t, s, site.ID, "pre-migration", "5afe5afe5afe5afe") {
		t.Fatal("no verified pre-migration snapshot was recorded")
	}
	if !slices.Contains(task.Locks, jobs.RepoLock(repo)) {
		t.Fatalf("locks %v: the snapshot runs outside the repository's lock", task.Locks)
	}
	if log := jobs.View(task).Log; !strings.Contains(log, "safety snapshot 5afe5afe5afe5afe retained") {
		t.Fatalf("the task does not name the safety snapshot: %q", log)
	}

	// A failed import keeps the snapshot and says so; the agent's error is
	// returned as it came.
	agent.errs[importer] = &agentrpc.Error{Code: agentrpc.CodeInvalid, Message: "the imported dump has no wp_options table"}
	_, task = migrate(t, s, admin, site, nil)
	if task.Status != state.JobFailed || task.Error != "the imported dump has no wp_options table" {
		t.Fatalf("failed import = %s %q", task.Status, task.Error)
	}
	if log := jobs.View(task).Log; !strings.Contains(log, "the safety snapshot 5afe5afe5afe5afe is kept") {
		t.Fatalf("the failed import does not name the safety snapshot: %q", log)
	}
}
