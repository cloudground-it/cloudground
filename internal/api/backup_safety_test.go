package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// hasBackup reports whether site has a backup of kind whose restore test
// passed (and, when snapshot is not empty, with that snapshot ID).
func hasBackup(t *testing.T, s *Server, siteID int64, kind, snapshot string) bool {
	t.Helper()
	list, err := s.Store.ListBackups(siteID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		if b.Kind == kind && b.VerifyStatus == state.VerifyPassed && (snapshot == "" || b.SnapshotID == snapshot) {
			return true
		}
	}
	return false
}

// A restore must name the site and a known mode; it then takes a safety
// snapshot, restore-tests it, records it as verified, and restores.
func TestRestoreIsConfirmedAndSnapshotsFirst(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("restore.example.com"))
	setBackupRepo(t, s, "/tmp/test-repo", "test-repository-password")
	old, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abcdef1234567890", Repository: "/tmp/test-repo", Kind: "full"})
	if err != nil {
		t.Fatal(err)
	}
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "fedcba0987654321", SizeBytes: 42})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	path := fmt.Sprintf("/api/backups/%d/restore", old.ID)
	for _, bad := range []map[string]string{
		{"confirm": "wrong"},
		{"confirm": site.Domain, "mode": "everything"},
	} {
		if resp, body := admin.do("POST", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("restore %v = %d %s, want 400", bad, resp.StatusCode, body)
		}
	}
	if resp, body := admin.do("POST", path, map[string]string{"confirm": site.Domain}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if !agent.called(agentrpc.RunBackup.Name()) || !agent.called(agentrpc.RestoreSnapshot.Name()) {
		t.Fatalf("calls %v: want a safety snapshot and the restore", agent.calls)
	}
	if !hasBackup(t, s, site.ID, "pre-restore", "fedcba0987654321") {
		t.Fatal("no verified pre-restore snapshot was recorded")
	}
}

// The safety snapshots a restore and a staging database push stand on are
// restore-tested like any backup before production changes, and taken
// under the repository's lock. A table the agent protects, or one staging
// lacks, is refused before any snapshot.
func TestSafetySnapshotsVerifiedAndUnderTheRepositoryLock(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	const repo = "/var/backups/repo"
	prod := seedActiveSite(t, s, "prod.example.com", state.SiteWordPress, "8.4", 0)
	stg := seedActiveSite(t, s, "staging.prod.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(stg.ID, func(st *state.Site) error { st.StagingOf = prod.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	setBackupRepo(t, s, repo, "correct-horse")
	old, _ := s.Store.CreateBackup(state.Backup{SiteID: prod.ID, SnapshotID: "abcdef1234567890", Repository: repo, Kind: "full"})
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "fedcba0987654321", SizeBytes: 42})
	agent.setResult(agentrpc.StagingTables.Name(), rpc.StagingTablesResult{Tables: []rpc.StagingTable{
		{Name: "wp_posts"}, {Name: "wp_wc_orders", Protected: true, Reason: "live store data"},
	}})
	pushPath := fmt.Sprintf("/api/sites/%d/staging/database", prod.ID)
	pushOf := func(table string) map[string]any {
		return map[string]any{"tables": []string{table}, "confirm": prod.Domain}
	}

	for _, table := range []string{"wp_wc_orders", "wp_missing"} {
		if resp, body := admin.do("POST", pushPath, pushOf(table)); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("push of %s = %d %s, want 400", table, resp.StatusCode, body)
		}
	}
	if agent.called(agentrpc.RunBackup.Name()) {
		t.Fatal("a refused push took a safety snapshot")
	}

	// A snapshot that fails its restore test stops both before production.
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: false, Detail: "database dump does not import"})
	if resp, body := admin.do("POST", fmt.Sprintf("/api/backups/%d/restore", old.ID), map[string]string{"confirm": prod.Domain}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d %s", resp.StatusCode, body)
	}
	if resp, body := admin.do("POST", pushPath, pushOf("wp_posts")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if agent.called(agentrpc.RestoreSnapshot.Name()) || agent.called(agentrpc.SyncStagingDB.Name()) {
		t.Fatalf("production changed behind an unverified snapshot: %v", agent.calls)
	}

	// While another job holds the repository, the push waits, queued,
	// without snapshotting.
	agent.forget()
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	release := holdLocks(t, s, jobs.RepoLock(repo))
	defer release()
	resp, body := admin.do("POST", pushPath, pushOf("wp_posts"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push = %d %s", resp.StatusCode, body)
	}
	var accepted struct {
		Task jobs.Task `json:"task"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	snapshotted := agent.called(agentrpc.RunBackup.Name())
	waiting, _ := s.Store.GetJob(accepted.Task.ID)
	release()
	if snapshotted {
		t.Fatal("the push snapshotted while another job held the repository")
	}
	if waiting.Status != state.JobQueued {
		t.Fatalf("the push behind the repository lock is %s, want queued", waiting.Status)
	}
	waitForTasks(t, s)
	if !agent.called(agentrpc.VerifyBackup.Name()) || !agent.called(agentrpc.SyncStagingDB.Name()) {
		t.Fatalf("calls %v: want the snapshot verified, then the tables pushed", agent.calls)
	}
	if !hasBackup(t, s, prod.ID, "pre-database-push", "") {
		t.Fatal("no verified pre-database-push snapshot")
	}
}

// Every backup is restore-tested after it is taken: "verified restores" is
// not a button to remember.
func TestEveryBackupGetsItsRestoreTest(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "verified.example.com", state.SiteStatic, "", 0)
	setBackupRepo(t, s, "/var/backups/repo", "correct-horse")
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "abc123", SizeBytes: 10})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, RestoreMillis: 1200, Detail: "ok"})
	if resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/backups", site.ID), map[string]string{}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("backup = %d %s", resp.StatusCode, body)
	}
	for end := time.Now().Add(jobWait); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		list, _ := s.Store.ListBackups(site.ID, 5)
		if len(list) == 1 && list[0].VerifyStatus == state.VerifyPassed {
			return
		}
	}
	list, _ := s.Store.ListBackups(site.ID, 5)
	t.Fatalf("the backup was not restore-tested: %+v", list)
}

// A repository URL's password is masked wherever it is shown; URLs
// without one are unchanged.
func TestRepositoryPasswordIsMasked(t *testing.T) {
	cases := [][2]string{
		{"rest:https://backup:s3cret@backups.example.com/repo", "rest:https://backup:••••••••@backups.example.com/repo"},
		{"sftp:user@host:/srv/restic", "sftp:user@host:/srv/restic"},
		{"/mnt/backups", "/mnt/backups"},
	}
	for _, c := range cases {
		if got := maskRepositoryCredentials(c[0]); got != c[1] {
			t.Errorf("mask(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}
