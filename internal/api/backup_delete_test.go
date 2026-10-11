package api

import (
	"errors"
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

func backupRecord(t *testing.T, s *Server, site state.Site, repo, snap string, verdict state.BackupVerify) state.Backup {
	t.Helper()
	b, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: snap, Repository: repo, Kind: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if verdict != state.VerifyPending {
		if err := s.Store.SetBackupVerified(b.ID, verdict, 10); err != nil {
			t.Fatal(err)
		}
	}
	b, _ = s.Store.GetBackup(b.ID)
	return b
}

// R-4.3: deleting one snapshot is an admin's, queued under the
// repository's lock; the agent forgets it and the record goes. The newest
// verified snapshot of the site and one whose restore test has not run are
// refused with 409, before anything is queued.
func TestDeleteSnapshot(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "snaps.example.com", state.SiteStatic, "", 0)
	const repo = "/var/backups/cloudground/repo"

	old := backupRecord(t, s, site, repo, "aaaaaaaa11111111", state.VerifyPassed)
	if resp, _ := c.do("DELETE", fmt.Sprintf("/api/backups/%d", old.ID), nil); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("delete without a repository = %d, want 412", resp.StatusCode)
	}
	setBackupRepo(t, s, repo, "correct-horse")
	failed := backupRecord(t, s, site, repo, "bbbbbbbb22222222", state.VerifyFailed)
	newest := backupRecord(t, s, site, repo, "cccccccc33333333", state.VerifyPassed)
	pending := backupRecord(t, s, site, repo, "dddddddd44444444", state.VerifyPending)

	for _, b := range []state.Backup{newest, pending} {
		resp, body := c.do("DELETE", fmt.Sprintf("/api/backups/%d", b.ID), nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("delete %s (%s) = %d %s, want 409", b.SnapshotID, b.VerifyStatus, resp.StatusCode, body)
		}
	}
	if agent.called(agentrpc.ForgetSnapshot.Name()) {
		t.Fatal("a refused deletion reached the agent")
	}

	// Queued behind whatever holds the repository.
	agent.mu.Lock()
	agent.results[agentrpc.ForgetSnapshot.Name()] = rpc.ForgetSnapshotResult{Forgotten: []string{"bbbbbbbb22222222ffff", "eeeeeeee55555555"}}
	agent.mu.Unlock()
	release := holdLocks(t, s, jobs.RepoLock(repo))
	resp, body := c.do("DELETE", fmt.Sprintf("/api/backups/%d", failed.ID), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete a failed snapshot = %d %s", resp.StatusCode, body)
	}
	task := taskOf(t, body)
	time.Sleep(50 * time.Millisecond)
	if j, _ := s.Store.GetJob(task.ID); j.Status != state.JobQueued || agent.called(agentrpc.ForgetSnapshot.Name()) {
		t.Fatalf("the deletion ran while another job held the repository: %s", j.Status)
	}
	release()
	if j := waitTaskEnd(t, s, task.ID); j.Status != state.JobSucceeded {
		t.Fatalf("deletion = %s %q", j.Status, j.Error)
	}
	p, _ := agent.lastParams[agentrpc.ForgetSnapshot.Name()].(rpc.ForgetSnapshotParams)
	if p.SnapshotID != failed.SnapshotID || p.Repository != repo || p.Password != "correct-horse" || p.Site.ID != site.ID {
		t.Fatalf("ForgetSnapshot params = %+v", p)
	}
	if _, err := s.Store.GetBackup(failed.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("the deleted snapshot's record is still there")
	}
	if _, err := s.Store.GetBackup(old.ID); err != nil {
		t.Fatal("another snapshot's record went with it")
	}
	// The deletion forgot without pruning: the daily prune prunes, once.
	if agent.called(agentrpc.PruneRepository.Name()) {
		t.Fatal("the deletion pruned the repository itself")
	}
	for range 2 {
		if j := runJobNow(t, s, jobs.Spec{Kind: "backup.prune", Locks: []string{jobs.RepoLock(repo)}}); j.Status != state.JobSucceeded {
			t.Fatalf("prune = %s %q", j.Status, j.Error)
		}
	}
	if n := countCalls(agent, agentrpc.PruneRepository.Name()); n != 1 {
		t.Fatalf("the repository was pruned %d times, want once", n)
	}

	// A snapshot already gone from the repository only loses its record.
	agent.mu.Lock()
	agent.results[agentrpc.ForgetSnapshot.Name()] = rpc.ForgetSnapshotResult{Forgotten: []string{}, Missing: true}
	agent.mu.Unlock()
	resp, body = c.do("DELETE", fmt.Sprintf("/api/backups/%d", old.ID), nil)
	if j := waitTaskEnd(t, s, taskOf(t, body).ID); resp.StatusCode != http.StatusAccepted || j.Status != state.JobSucceeded {
		t.Fatalf("delete a missing snapshot = %d %s", resp.StatusCode, j.Status)
	}
	if _, err := s.Store.GetBackup(old.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("the record of a snapshot the repository no longer has stayed")
	}
}

// R-4.3: the refusal is checked again when the job runs, and a deletion the
// agent refuses keeps the record.
func TestDeleteSnapshotRechecksAndKeepsTheRecordOnFailure(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "recheck.example.com", state.SiteStatic, "", 0)
	const repo = "/var/backups/cloudground/repo"
	setBackupRepo(t, s, repo, "correct-horse")
	older := backupRecord(t, s, site, repo, "aaaaaaaa11111111", state.VerifyPassed)
	newest := backupRecord(t, s, site, repo, "bbbbbbbb22222222", state.VerifyPassed)

	release := holdLocks(t, s, jobs.RepoLock(repo))
	resp, body := c.do("DELETE", fmt.Sprintf("/api/backups/%d", older.ID), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	// While it waits the newest one goes: the older is now the newest
	// verified snapshot, and must stay.
	if err := s.Store.DeleteBackup(newest.ID); err != nil {
		t.Fatal(err)
	}
	release()
	j := waitTaskEnd(t, s, taskOf(t, body).ID)
	if j.Status != state.JobFailed || !strings.Contains(j.Error, "newest backup") || agent.called(agentrpc.ForgetSnapshot.Name()) {
		t.Fatalf("deletion of what became the newest verified snapshot = %s %q", j.Status, j.Error)
	}

	agent.mu.Lock()
	agent.errs[agentrpc.ForgetSnapshot.Name()] = agentrpc.Conflict("the backup repository is locked by another restic process")
	agent.mu.Unlock()
	backupRecord(t, s, site, repo, "cccccccc33333333", state.VerifyPassed) // a newer verified one again
	resp, body = c.do("DELETE", fmt.Sprintf("/api/backups/%d", older.ID), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	if j := waitTaskEnd(t, s, taskOf(t, body).ID); j.Status != state.JobFailed {
		t.Fatalf("deletion the agent refused = %s", j.Status)
	}
	if _, err := s.Store.GetBackup(older.ID); err != nil {
		t.Fatal("a snapshot the agent did not forget lost its record")
	}
}
