package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func retentionSite(t *testing.T, s *Server, days int) state.Site {
	t.Helper()
	site, err := s.Store.CreateSite(state.Site{
		Domain: "keep.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		SystemUser: "cg-site-1", RootPath: "/srv/sites/keep.example.com", Status: state.SiteActive,
		Config: state.SiteConfig{Backups: state.BackupPolicy{Enabled: true, Schedule: "daily", RetentionDays: days}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// Retention protects the newest verified snapshot, drops the records of the
// snapshots restic forgot, and keeps the rest.
func TestBackupRetentionProtectsVerifiedAndDropsForgottenRecords(t *testing.T) {
	s, agent, _ := testServer(t)
	const repo = "/var/backups/cloudground/repo"
	setBackupRepo(t, s, repo, "correct-horse")
	site := retentionSite(t, s, 14)

	mk := func(snap string, status state.BackupVerify) state.Backup {
		b, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: snap, Repository: repo, Kind: "full"})
		if err != nil {
			t.Fatal(err)
		}
		if status != state.VerifyPending {
			s.Store.SetBackupVerified(b.ID, status, 1)
		}
		return b
	}
	mk("1111111111111111", state.VerifyPassed) // old, superseded verified
	mk("2222222222222222", state.VerifyPassed) // newest verified: protected
	mk("3333333333333333", state.VerifyFailed) // newer, failed verification
	other, _ := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "1111111111111111", Repository: "/elsewhere", Kind: "full"})

	agent.results[agentrpc.BackupRetention.Name()] = rpc.RetentionResult{
		// restic reports full IDs; the panel recorded short ones.
		Forgotten: []string{"1111111111111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Kept:      []string{"2222222222222222", "3333333333333333"},
	}
	if err := s.applyBackupRetention(context.Background(), site.ID, repo, "correct-horse", func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	p, _ := agent.lastParams[agentrpc.BackupRetention.Name()].(rpc.RetentionParams)
	if p.KeepWithinDays != 14 || p.Repository != repo || p.Site.ID != site.ID {
		t.Fatalf("retention params = %+v", p)
	}
	if len(p.Protect) != 1 || p.Protect[0] != "2222222222222222" {
		t.Fatalf("protected = %v, want the newest verified snapshot", p.Protect)
	}
	list, _ := s.Store.ListBackups(site.ID, 50)
	var snaps []string
	for _, b := range list {
		snaps = append(snaps, b.Repository+"@"+b.SnapshotID)
	}
	if fmt.Sprint(snaps) != fmt.Sprint([]string{"/elsewhere@1111111111111111", repo + "@3333333333333333", repo + "@2222222222222222"}) {
		t.Fatalf("records after retention = %v", snaps)
	}
	if _, err := s.Store.GetBackup(other.ID); err != nil {
		t.Fatal("a record in another repository was deleted")
	}
}

// The daily job runs once a day per site with backups enabled, and never for
// a site without a retention window or any recorded backup.
func TestBackupRetentionIsScheduledDaily(t *testing.T) {
	s, agent, _ := testServer(t)
	const repo = "/var/backups/cloudground/repo"
	setBackupRepo(t, s, repo, "correct-horse")
	site := retentionSite(t, s, 30)
	agent.results[agentrpc.BackupRetention.Name()] = rpc.RetentionResult{}

	runJobNow(t, s, jobs.Spec{Kind: "retention.sweep"})
	if agent.called(agentrpc.BackupRetention.Name()) {
		t.Fatal("retention ran for a site with no recorded backups")
	}
	s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abcdef0123456789", Repository: repo, Kind: "full"})
	runJobNow(t, s, jobs.Spec{Kind: "retention.sweep"})
	runJobNow(t, s, jobs.Spec{Kind: "retention.sweep"})
	n := 0
	agent.mu.Lock()
	defer agent.mu.Unlock()
	for _, c := range agent.calls {
		if c == agentrpc.BackupRetention.Name() {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("retention ran %d times in one day, want 1", n)
	}
}

func TestBackupRetentionCanBeRunNow(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "correct-horse")
	site := retentionSite(t, s, 30)
	agent.results[agentrpc.BackupRetention.Name()] = rpc.RetentionResult{}
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/backups/retention", site.ID), map[string]string{}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retention = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if !agent.called(agentrpc.BackupRetention.Name()) {
		t.Fatal("the retention run did not reach the agent")
	}
}
