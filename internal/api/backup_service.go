package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The backup service (SPEC.md §5): snapshots, their restore tests,
// restores, the deletion of one snapshot. Everything that runs restic is
// a job holding the repository's lock; nothing here knows about HTTP.

// errBackupsOff is a backup job finding the repository unset.
var errBackupsOff = errors.New("backups are not configured")

// backupRepository is the configured repository and its password, ""
// for whichever is unset.
func (s *Server) backupRepository() (repo, password string, err error) {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return "", "", err
	}
	if password, err = s.Store.Secret(state.SecretBackupPassword); err != nil {
		return "", "", err
	}
	return ps.BackupRepository, password, nil
}

// backupEnv is restic's credentials from the settings (S3, R2, B2, the
// SFTP key: backupEnvFor); the ones not set are left out.
func (s *Server) backupEnv() map[string]string {
	ps, err := s.Store.PanelSettings()
	s.logIfErr(err, "PanelSettings")
	return s.backupEnvFor(ps, nil)
}

// backupPayload names the record a verify or a deletion is about.
type backupPayload struct {
	BackupID int64 `json:"backup_id"`
}

// runSiteBackup is backup.run and backup.scheduled: a snapshot, then its
// restore test in the same job. A scheduled one that fails keeps its
// schedule key: the next attempt is at the site's next slot. Clearing it,
// as before, retried at every sweep (five minutes), and a backup that
// cannot pass (a site with nothing to restore-test) made a new snapshot
// and a new alert every five minutes for ever.
func (s *Server) runSiteBackup(ctx context.Context, r *jobs.Run) error {
	return s.snapshotAndVerify(ctx, r, r.Job().Kind == "backup.scheduled")
}

func (s *Server) snapshotAndVerify(ctx context.Context, r *jobs.Run, scheduled bool) error {
	site, err := s.Store.GetSite(r.Job().SiteID)
	if errors.Is(err, state.ErrNotFound) || err == nil && scheduled && site.Status != state.SiteActive {
		r.Progress(100, "Skipped: the site is no longer active")
		return nil
	}
	if err != nil {
		return err
	}
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		return errBackupsOff
	}
	if !slices.Contains(r.Job().Locks, jobs.RepoLock(repo)) {
		return jobs.ErrRequeue // the repository changed while the job waited
	}
	if scheduled {
		r.Progress(20, "Scheduled backup of "+site.Domain)
	} else {
		r.Progress(20, "Snapshotting "+site.Domain+" (files + database)")
	}
	b, err := s.snapshot(ctx, site, repo, password, "full")
	if err != nil {
		return err
	}
	// The restore test runs whatever happens to the job's context: a
	// recorded snapshot is never left untested.
	return s.verifyBackup(context.WithoutCancel(ctx), site, b, password, r.Progress, 60)
}

// snapshot has the agent take a full snapshot of site and records it,
// pending its restore test, under kind. A snapshot stored but not recorded
// is never tested or offered: that is an error, not a success.
func (s *Server) snapshot(ctx context.Context, site state.Site, repo, password, kind string) (state.Backup, error) {
	var res rpc.BackupResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RunBackup, rpc.BackupParams{
		Env: s.backupEnv(), Site: site, Repository: repo, Password: password, Kind: "full",
	}, &res); err != nil {
		return state.Backup{}, err
	}
	b, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: res.SnapshotID, Repository: repo,
		SizeBytes: res.SizeBytes, Kind: kind, Databases: res.Databases})
	if err != nil {
		return state.Backup{}, fmt.Errorf("snapshot %s was stored but could not be recorded: %w", res.SnapshotID, err)
	}
	return b, nil
}

// verifyBackup is the restore test: the agent restores the snapshot into
// a scratch place and checks it; the verdict and the time it took are
// recorded. A test that cannot run marks the snapshot failed, never
// leaves it pending.
func (s *Server) verifyBackup(ctx context.Context, site state.Site, b state.Backup, password string, progress func(int, string), from int) error {
	progress(from, "Restore test: restoring "+b.SnapshotID+" into a scratch environment")
	var res rpc.VerifyResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.VerifyBackup, rpc.RestoreParams{
		Env: s.backupEnv(), Site: site, Repository: b.Repository, Password: password, SnapshotID: b.SnapshotID,
	}, &res); err != nil {
		s.logIfErr(s.Store.SetBackupVerified(b.ID, state.VerifyFailed, 0), "SetBackupVerified")
		return fmt.Errorf("restore test of %s: %w", b.SnapshotID, err)
	}
	verdict := state.VerifyFailed
	if res.Passed {
		verdict = state.VerifyPassed
	}
	if err := s.Store.SetBackupVerified(b.ID, verdict, res.RestoreMillis); err != nil {
		return err
	}
	if !res.Passed {
		return errors.New("restore test failed: " + res.Detail)
	}
	progress(100, "Snapshot "+b.SnapshotID+" stored off-site; restore test passed: "+res.Detail)
	return nil
}

// enqueueVerify queues a recorded snapshot's restore test, at most one at
// a time per snapshot.
func (s *Server) enqueueVerify(b state.Backup, background bool) (jobs.Task, error) {
	return s.enqueue(jobs.Spec{Kind: "backup.verify", SiteID: b.SiteID, Locks: []string{jobs.RepoLock(b.Repository)},
		Background: background, DedupKey: "backup.verify:" + strconv.FormatInt(b.ID, 10),
		Payload: backupPayload{BackupID: b.ID}})
}

// runVerifyBackup is backup.verify, which cannot be canceled.
func (s *Server) runVerifyBackup(ctx context.Context, r *jobs.Run) error {
	var p backupPayload
	if err := r.Payload(&p); err != nil {
		return err
	}
	b, err := s.Store.GetBackup(p.BackupID)
	if errors.Is(err, state.ErrNotFound) {
		r.Progress(100, "Skipped: the snapshot's record is gone")
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(r.Job().Locks, jobs.RepoLock(b.Repository)) {
		return jobs.ErrRequeue // the record moved to another repository (§5.9)
	}
	site, err := s.Store.GetSite(b.SiteID)
	if err != nil {
		return err
	}
	_, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if password == "" {
		return errBackupsOff
	}
	return s.verifyBackup(ctx, site, b, password, r.Progress, 20)
}

// safetySnapshot is the snapshot a destructive operation (a restore, a
// staging database push) is undone with, restore-tested before anything
// changes: a safety net never tested is not one. The caller's job holds
// the repository lock. A cancel before the snapshot stops the operation;
// once it is recorded its test runs whatever happens.
func (s *Server) safetySnapshot(ctx context.Context, site state.Site, repo, password, kind string, progress func(int, string)) (state.Backup, error) {
	if err := ctx.Err(); err != nil {
		return state.Backup{}, err
	}
	progress(10, "Creating an off-site safety snapshot")
	b, err := s.snapshot(ctx, site, repo, password, kind)
	if err != nil {
		return state.Backup{}, fmt.Errorf("safety snapshot failed; production was not changed: %w", err)
	}
	// The test's own 100% would read as the whole job's: here it is a step.
	step := func(pct int, msg string) { progress(min(pct, 40), msg) }
	if err := s.verifyBackup(context.WithoutCancel(ctx), site, b, password, step, 20); err != nil {
		return state.Backup{}, fmt.Errorf("safety snapshot %s failed; production was not changed: %w", b.SnapshotID, err)
	}
	return b, nil
}

// restoreWork is a backup.restore job: a tested safety snapshot, then the
// restore of b in mode over the live site (of databases only, when any are
// named; else every database the backup holds and the site still has),
// then an empty page cache.
func (s *Server) restoreWork(b state.Backup, mode, password string, databases, paths []string) jobWork {
	return func(ctx context.Context, site state.Site, progress func(int, string)) error {
		safety, err := s.safetySnapshot(ctx, site, b.Repository, password, "pre-restore", progress)
		if err != nil {
			return err
		}
		if err := checkpoint(ctx, progress, "the restore (the safety snapshot "+safety.SnapshotID+" is kept)"); err != nil {
			return err
		}
		progress(45, "Restoring and validating "+b.SnapshotID+" ("+mode+")")
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.RestoreSnapshot, rpc.RestoreParams{
			Env: s.backupEnv(), Site: site, Repository: b.Repository, Password: password, SnapshotID: b.SnapshotID, Mode: mode,
			Databases: databases, Paths: paths,
		}, nil); err != nil {
			return err
		}
		progress(90, "Purging restored site cache")
		s.purgeQuietly(ctx, site)
		progress(100, "Restore complete; rollback snapshot "+safety.SnapshotID+" is retained")
		return nil
	}
}

// deleteRefusal is why a snapshot may not be deleted now ("" when it
// may): its restore test has not run (it is queued and must find it), or
// it is the site's newest one known to restore.
func (s *Server) deleteRefusal(b state.Backup, domain string) (string, error) {
	if b.VerifyStatus == state.VerifyPending {
		return "the restore test of this snapshot has not run yet; it can be deleted once it has", nil
	}
	newest, err := s.Store.LatestVerifiedBackup(b.SiteID, b.Repository)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return "", nil
	case err != nil:
		return "", err
	case newest.ID == b.ID:
		return "this is the newest backup of " + domain + " whose restore test passed; it can be deleted once a newer one has passed its test", nil
	}
	return "", nil
}

// enqueueForget queues the deletion of one snapshot, under its
// repository's lock.
func (s *Server) enqueueForget(b state.Backup) (jobs.Task, error) {
	return s.enqueue(jobs.Spec{Kind: "backup.forget", SiteID: b.SiteID, Locks: []string{jobs.RepoLock(b.Repository)},
		DedupKey: "backup.forget:" + strconv.FormatInt(b.ID, 10), Payload: backupPayload{BackupID: b.ID}})
}

// runForget is backup.forget: the agent forgets the snapshot (and its
// database half) and prunes, then the record goes. The refusals are
// checked again here, under the lock: the newest verified snapshot may
// have changed while the job waited.
func (s *Server) runForget(ctx context.Context, r *jobs.Run) error {
	var p backupPayload
	if err := r.Payload(&p); err != nil {
		return err
	}
	b, err := s.Store.GetBackup(p.BackupID)
	if errors.Is(err, state.ErrNotFound) {
		r.Progress(100, "Skipped: the snapshot's record is already gone")
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(r.Job().Locks, jobs.RepoLock(b.Repository)) {
		return jobs.ErrRequeue // the record moved to another repository (§5.9)
	}
	site, err := s.Store.GetSite(b.SiteID)
	if err != nil {
		return err
	}
	if why, err := s.deleteRefusal(b, site.Domain); err != nil || why != "" {
		return errors.Join(err, errorsIf(why))
	}
	_, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if password == "" {
		return errBackupsOff
	}
	if err := checkpoint(ctx, r.Progress, "deleting snapshot "+b.SnapshotID); err != nil {
		return err
	}
	// Once sent, the forget and its prune run to their end.
	ctx = context.WithoutCancel(ctx)
	r.Progress(30, "Deleting snapshot "+b.SnapshotID+" of "+site.Domain+" and pruning its data")
	var res rpc.ForgetSnapshotResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.ForgetSnapshot, rpc.ForgetSnapshotParams{
		Site: site, Repository: b.Repository, Password: password, SnapshotID: b.SnapshotID, Env: s.backupEnv(),
	}, &res); err != nil {
		return err
	}
	if err := s.Store.DeleteBackup(b.ID); err != nil && !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("snapshot %s was deleted but its record could not be: %w", b.SnapshotID, err)
	}
	if _, err := s.Store.DeleteBackupsBySnapshot(site.ID, b.Repository, append(res.Forgotten, res.Originals...)); err != nil {
		return fmt.Errorf("snapshot %s was deleted but its records could not be: %w", b.SnapshotID, err)
	}
	if len(res.Forgotten) > 0 {
		// Forgotten, not pruned: the data goes at the daily prune.
		s.logIfErr(s.Store.MarkScheduleRun(pruneKey(b.Repository)), "MarkScheduleRun")
	}
	detail := fmt.Sprintf("snapshot %s deleted (%d restic snapshots forgotten; their data goes at the next prune)", b.SnapshotID, len(res.Forgotten))
	if res.Missing {
		detail = "snapshot " + b.SnapshotID + " was no longer in the repository; its record is removed"
	}
	s.audit("system", "backup.forget", site.Domain, detail)
	r.Progress(100, detail)
	return nil
}

// pruneKey flags a repository where snapshots were forgotten without a
// prune (a deletion): the daily backup.prune prunes it.
func pruneKey(repo string) string { return "backup.prune." + jobs.RepoLock(repo) }

// runPrune is backup.prune (daily, under the repository's lock): when a
// deletion forgot snapshots of the configured repository since the last
// prune, their data is pruned now. A prune can take hours, which is why a
// deletion does not wait for one.
func (s *Server) runPrune(ctx context.Context, r *jobs.Run) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		r.Progress(100, "Skipped: backups are not configured")
		return nil
	}
	if !slices.Contains(r.Job().Locks, jobs.RepoLock(repo)) {
		return jobs.ErrRequeue
	}
	if t, err := s.Store.LastScheduleRun(pruneKey(repo)); err != nil || t.IsZero() {
		r.Progress(100, "Nothing to prune")
		return err
	}
	r.Progress(20, "Pruning the data of deleted snapshots")
	if err := agentrpc.Into(context.WithoutCancel(ctx), s.Agent, agentrpc.PruneRepository,
		rpc.TestBackupParams{Repository: repo, Password: password, Env: s.backupEnv()}, nil); err != nil {
		return err
	}
	s.logIfErr(s.Store.ClearScheduleRun(pruneKey(repo)), "ClearScheduleRun")
	r.Progress(100, "Repository pruned")
	return nil
}

// errorsIf is an error with text msg, nil for "".
func errorsIf(msg string) error {
	if msg == "" {
		return nil
	}
	return errors.New(msg)
}
