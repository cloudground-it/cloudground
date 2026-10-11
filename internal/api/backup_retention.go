package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Scheduled backups and retention: two sweeps that queue one job per site
// that is due (internal/jobs/SPEC.md §8–9). Each site's last run is kept
// under the keys the old scheduler used, so an upgrade neither repeats nor
// skips a backup.

// retentionInterval is how often a site's retention window is applied.
const retentionInterval = 24 * time.Hour

func backupScheduleKey(siteID int64) string {
	return "backup.site." + strconv.FormatInt(siteID, 10)
}

func retentionScheduleKey(siteID int64) string {
	return "backup.retention.site." + strconv.FormatInt(siteID, 10)
}

// backupEvery is the spacing a backup policy's schedule asks for.
func backupEvery(schedule string) time.Duration {
	switch schedule {
	case "hourly":
		return time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// hourSlack is how much earlier than a full day or week after the last
// backup one at a fixed hour may run: the sweep comes every five minutes,
// so last week's ran a little after the hour, and this week's slot comes
// a little less than a week later. Half a day also keeps a changed hour
// from running a second backup the same day.
const hourSlack = 12 * time.Hour

// backupDue reports whether a site's scheduled backup is due at now, its
// last one queued at last. Without an hour (or hourly) it is the spacing
// since the last; with one, the newest slot (today at the hour, or
// yesterday's while today's is ahead) must have no backup after it, and
// must come (nearly) a day or a week after the last one, so a weekly one
// keeps its weekday. A slot missed while the panel was down runs at the next sweep.
func backupDue(p state.BackupPolicy, last, now time.Time) bool {
	every := backupEvery(p.Schedule)
	if p.Hour == nil || every < 24*time.Hour {
		return now.Sub(last) >= every
	}
	slot := time.Date(now.Year(), now.Month(), now.Day(), *p.Hour, 0, 0, 0, now.Location())
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return last.Before(slot) && slot.Sub(last) >= every-hourSlack
}

// scheduledBackupSites are the production sites whose backup policy is on.
func (s *Server) scheduledBackupSites() ([]state.Site, error) {
	all, err := s.Store.ListSites()
	if err != nil {
		return nil, err
	}
	var out []state.Site
	for _, site := range all {
		if site.Status == state.SiteActive && site.StagingOf == 0 && site.Config.Backups.Enabled && backedUp(site.Type) {
			out = append(out, site)
		}
	}
	return out, nil
}

// runBackupSweep queues the scheduled backup of every site that is due. The
// key is set when the job is queued, so the next sweep does not queue a
// second while it waits or runs, and a failed backup is tried again at the
// site's next slot, not at every sweep (runSiteBackup).
func (s *Server) runBackupSweep(ctx context.Context, r *jobs.Run) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		return nil // backups not configured
	}
	sites, err := s.scheduledBackupSites()
	if err != nil {
		return err
	}
	queued := 0
	for _, site := range sites {
		key := backupScheduleKey(site.ID)
		last, _ := s.Store.LastScheduleRun(key)
		if !backupDue(site.Config.Backups, last, time.Now()) {
			continue
		}
		if _, err := s.Jobs.Enqueue(jobs.Spec{
			Kind: "backup.scheduled", SiteID: site.ID, Locks: []string{jobs.RepoLock(repo)}, Background: true,
			DedupKey: "backup.scheduled:" + strconv.FormatInt(site.ID, 10),
			Payload:  sitePayload{SiteID: site.ID},
		}); err != nil {
			return err
		}
		s.logIfErr(s.Store.MarkScheduleRun(key), "MarkScheduleRun")
		queued++
	}
	r.Progress(100, fmt.Sprintf("%d scheduled backups queued", queued))
	return nil
}

// runRetentionSweep queues, once a day per site, the retention its backup
// policy names. A site with nothing recorded in the repository yet is left
// for a later sweep (the repository may not even exist). The key is set
// when the job is queued and not cleared on failure: a repository that
// refuses is tried again the next day, not at every sweep.
func (s *Server) runRetentionSweep(ctx context.Context, r *jobs.Run) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		return nil
	}
	sites, err := s.scheduledBackupSites()
	if err != nil {
		return err
	}
	for _, site := range sites {
		if site.Config.Backups.RetentionDays <= 0 {
			continue
		}
		key := retentionScheduleKey(site.ID)
		if last, _ := s.Store.LastScheduleRun(key); time.Since(last) < retentionInterval {
			continue
		}
		if n, err := s.Store.CountBackups(site.ID, repo); err != nil || n == 0 {
			continue
		}
		if _, err := s.enqueueRetention(site.ID, repo, true); err != nil {
			return err
		}
		s.logIfErr(s.Store.MarkScheduleRun(key), "MarkScheduleRun")
	}
	return nil
}

func (s *Server) enqueueRetention(siteID int64, repo string, background bool) (jobs.Task, error) {
	return s.enqueue(jobs.Spec{
		Kind: "backup.retention", SiteID: siteID, Locks: []string{jobs.RepoLock(repo)}, Background: background,
		DedupKey: "backup.retention:" + strconv.FormatInt(siteID, 10),
		Payload:  sitePayload{SiteID: siteID},
	})
}

func (s *Server) runRetention(ctx context.Context, r *jobs.Run) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		return errors.New("backups are not configured")
	}
	if !slices.Contains(r.Job().Locks, jobs.RepoLock(repo)) {
		return jobs.ErrRequeue // the repository changed since the job was queued
	}
	return s.applyBackupRetention(ctx, r.Job().SiteID, repo, password, r.Progress)
}

// applyBackupRetention forgets and prunes a site's snapshots older than its
// window, always keeping the newest verified one whatever its age, and drops
// the records of the snapshots that went. The job holds the repository
// lock: a prune takes the repository's exclusive lock.
func (s *Server) applyBackupRetention(ctx context.Context, siteID int64, repo, password string, progress func(int, string)) error {
	site, err := s.Store.GetSite(siteID)
	if err != nil || site.Status != state.SiteActive {
		progress(100, "Skipped: the site is no longer active")
		return nil
	}
	days := site.Config.Backups.RetentionDays
	if days <= 0 {
		progress(100, "Skipped: "+site.Domain+" has no retention window")
		return nil
	}
	var protect []string
	switch newest, err := s.Store.LatestVerifiedBackup(site.ID, repo); {
	case err == nil:
		protect = []string{newest.SnapshotID}
	case !errors.Is(err, state.ErrNotFound):
		return err
	}
	// A snapshot whose restore test has not run yet is never forgotten:
	// the test is queued (or about to be) and must find it.
	pending, err := s.Store.UnverifiedBackups()
	if err != nil {
		return err
	}
	for _, b := range pending {
		if b.SiteID == site.ID && b.Repository == repo {
			protect = append(protect, b.SnapshotID)
		}
	}
	progress(30, fmt.Sprintf("Removing snapshots of %s older than %d days", site.Domain, days))
	var res rpc.RetentionResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.BackupRetention, rpc.RetentionParams{
		Site: site, Repository: repo, Password: password, KeepWithinDays: days,
		Protect: protect, Env: s.backupEnv(),
	}, &res); err != nil {
		return err
	}
	if _, err := s.Store.DeleteBackupsBySnapshot(site.ID, repo, res.Forgotten); err != nil {
		return fmt.Errorf("%d snapshots were removed but their records could not be: %w", len(res.Forgotten), err)
	}
	if len(res.Forgotten) > 0 {
		// Its forget --prune pruned the repository: deletions included.
		s.logIfErr(s.Store.ClearScheduleRun(pruneKey(repo)), "ClearScheduleRun")
	}
	detail := fmt.Sprintf("%d snapshots removed, %d kept", len(res.Forgotten), len(res.Kept))
	s.audit("system", "backup.retention", site.Domain, detail)
	progress(100, detail)
	return nil
}

// handleBackupRetention applies a site's retention window now rather than
// at the next daily run.
func (s *Server) handleBackupRetention(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	repo, _, ok := s.backupConfig(w)
	if !ok {
		return
	}
	if site.Config.Backups.RetentionDays <= 0 {
		respondErr(w, http.StatusBadRequest, "this site has no backup retention window")
		return
	}
	s.audit(c.Actor(), "backup.retention_run", site.Domain, "")
	task, err := s.enqueueRetention(site.ID, repo, false)
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}
