package api

import (
	"errors"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A scheduled backup that fails is tried again at the site's next slot,
// not at the next sweep: before, a backup that could not pass made a new
// snapshot and a new alert every five minutes for ever.
func TestFailedScheduledBackupWaitsForTheNextSlot(t *testing.T) {
	s, agent, ts := testServer(t)
	setupAdmin(t, s, ts)
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "correct-horse")
	site := retentionSite(t, s, 0)
	agent.setErr(agentrpc.RunBackup.Name(), errors.New("restic exploded"))
	runJobNowQueued(t, s, jobs.Spec{Kind: "backup.sweep"})
	waitForTasks(t, s)
	if !agent.called(agentrpc.RunBackup.Name()) {
		t.Fatal("the scheduled backup did not run")
	}
	if last, _ := s.Store.LastScheduleRun(backupScheduleKey(site.ID)); last.IsZero() {
		t.Fatal("a failed scheduled backup cleared its schedule: the next sweep retries it")
	}
	agent.forget()
	runJobNowQueued(t, s, jobs.Spec{Kind: "backup.sweep"})
	waitForTasks(t, s)
	if agent.called(agentrpc.RunBackup.Name()) {
		t.Fatal("the next sweep backed the site up again")
	}
}

// The failures of a job the schedule repeats are one alert per site: the
// newest, raised again only when the reason changes (its fingerprint).
func TestScheduledBackupFailuresAreOneAlert(t *testing.T) {
	now := time.Now()
	list := []state.Job{
		{ID: 10, Kind: "backup.scheduled", SiteID: 3, Status: state.JobFailed, Error: "the dump imported no tables", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 11, Kind: "backup.scheduled", SiteID: 3, Status: state.JobFailed, Error: "restic exploded", CreatedAt: now.Add(-time.Hour)},
		{ID: 12, Kind: "backup.scheduled", SiteID: 4, Status: state.JobFailed, Error: "x", CreatedAt: now},
		{ID: 13, Kind: "backup.run", SiteID: 3, Status: state.JobFailed, Error: "y", CreatedAt: now},
	}
	got := taskConditions(list, map[int64]bool{3: true, 4: true}, now)
	keys := map[string]state.Alert{}
	for _, a := range got {
		keys[a.Key] = a
	}
	if len(got) != 3 || keys["task:backup.scheduled:site:3"].Fingerprint != "restic exploded" ||
		keys["task:backup.scheduled:site:4"].Key == "" || keys["task:13"].Key == "" {
		t.Fatalf("alerts %+v", got)
	}
}
