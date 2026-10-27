package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The API's side of the job queue (internal/jobs/SPEC.md §8): the table of
// kinds, the schedules, and the helpers handlers start jobs with.

// jobWork is the work of a job a request starts. site is the job's site
// read at the job's start, not the copy the request saw (zero for a
// server-wide job).
type jobWork func(ctx context.Context, site state.Site, progress func(int, string)) error

// jobReq is a job a request starts with its work in a function.
type jobReq struct {
	kind   string
	siteID int64
	locks  []string
	// payload is stored for the record (and the kind's hooks); the work
	// runs from the function.
	payload  any
	dedupKey string
	canceled func()
}

// startJob enqueues a job whose work is fn and answers with its task view.
// At its start the job reads its site again: one deleted while the job
// waited, or being deleted by another job, fails it without running; a
// cancel that came in between ends it before its first step.
func (s *Server) startJob(req jobReq, fn jobWork) (jobs.Task, error) {
	siteID, kind := req.siteID, req.kind
	if req.payload == nil && siteID != 0 {
		// For the record: the alert of a job that failed on a site deleted
		// since (a creation rolled back) names it by this domain.
		if site, err := s.Store.GetSite(siteID); err == nil {
			req.payload = jobRecord{SiteID: siteID, Domain: site.Domain}
		}
	}
	j, err := s.Jobs.Enqueue(jobs.Spec{
		Kind: kind, SiteID: siteID, Locks: req.locks, Payload: req.payload, Canceled: req.canceled,
		DedupKey: req.dedupKey,
		Func: func(ctx context.Context, r *jobs.Run) error {
			site, err := s.jobSite(siteID, kind)
			if err != nil {
				return err
			}
			if err := checkpoint(ctx, r.Progress, "its first step"); err != nil {
				return err
			}
			return fn(ctx, site, r.Progress)
		},
	})
	if err != nil {
		return jobs.Task{}, err
	}
	return jobs.View(j), nil
}

// enqueue enqueues a job of a kind that runs from its payload.
func (s *Server) enqueue(spec jobs.Spec) (jobs.Task, error) {
	j, err := s.Jobs.Enqueue(spec)
	if err != nil {
		return jobs.Task{}, err
	}
	return jobs.View(j), nil
}

// errSiteGone is how a job about a deleted site ends.
var errSiteGone = errors.New("the site was deleted before this job could run")

// jobSite reads a job's site at its start. Only the deletion itself may
// run on a site being deleted.
func (s *Server) jobSite(id int64, kind string) (state.Site, error) {
	if id == 0 {
		return state.Site{}, nil
	}
	site, err := s.Store.GetSite(id)
	if errors.Is(err, state.ErrNotFound) {
		return state.Site{}, errSiteGone
	}
	if err != nil {
		return state.Site{}, err
	}
	if site.Status == state.SiteDeleting && kind != "site.delete" {
		return state.Site{}, errors.New("the site is being deleted")
	}
	return site, nil
}

// checkpoint ends a job at a step boundary when it was cancelled (or the
// panel is stopping): the steps already done are the log's lines above, and
// this line says where it stopped. A change sent to the agent always runs
// to its end; this is the only place a job gives up, between two of them.
func checkpoint(ctx context.Context, progress func(int, string), next string) error {
	if err := ctx.Err(); err != nil {
		progress(-1, "Stopped before "+next+"; the steps above were done")
		return err
	}
	return nil
}

// respondJobErr answers a job that could not be enqueued.
func respondJobErr(w http.ResponseWriter, err error) {
	if errors.Is(err, jobs.ErrDraining) {
		respondErr(w, http.StatusServiceUnavailable, "the panel is restarting; try again in a moment")
		return
	}
	respondErr(w, http.StatusInternalServerError, "could not start the job")
}

// siteLocks is the lock of each site named (0 is skipped).
func siteLocks(ids ...int64) []string {
	var out []string
	for _, id := range ids {
		if id != 0 {
			out = append(out, jobs.SiteLock(id))
		}
	}
	return out
}

// jobRecord is the payload a function job stores for the record: its site
// and the site's domain.
type jobRecord struct {
	SiteID int64  `json:"site_id"`
	Domain string `json:"domain,omitempty"`
}

// sitePayload names the site a resumable job is about.
type sitePayload struct {
	SiteID int64 `json:"site_id"`
}

// stagingPayload names the staging copy a staging job builds.
type stagingPayload struct {
	StagingID int64 `json:"staging_id,omitempty"`
}

// startJobQueue creates the queue, registers the kinds and settles what a
// previous process left (Init calls it once).
func (s *Server) startJobQueue() {
	s.Jobs = jobs.New(s.Store, jobs.Options{Events: s.Events, Log: s.Log, Recovered: s.afterJobRecovery})
	s.registerJobKinds()
	s.sched = jobs.NewScheduler(s.Jobs, s.schedules())
	s.logIfErr(s.Jobs.Start(), "job recovery")
}

// DrainJobs stops the scheduler and drains the queue (SPEC.md §6). It
// reports how many jobs were still running when it gave up.
func (s *Server) DrainJobs(ctx context.Context) int {
	if s.Jobs == nil {
		return 0
	}
	s.sched.Stop()
	return s.Jobs.Drain(ctx)
}

// Drain is the API's shutdown hook (cmd/panel-api calls it once, with its
// drain timeout, after the HTTP servers stopped): the job queue's drain. A
// job still running when it gives up is settled at the next start.
func (s *Server) Drain(ctx context.Context) error {
	if left := s.DrainJobs(ctx); left > 0 {
		return fmt.Errorf("%d job(s) still running at the shutdown deadline: %w", left, context.DeadlineExceeded)
	}
	return nil
}

// markIfStatus moves a site to error when it is still in status: what an
// interrupted provisioning or deletion leaves for a person to act on.
func (s *Server) markIfStatus(id int64, status state.SiteStatus) {
	site, err := s.Store.GetSite(id)
	if err != nil || site.Status != status {
		return
	}
	s.markSiteError(id)
}

// repoRerunDelay holds back a backup, restore test or retention that a
// restart interrupted, so the restic process the dead panel left running
// can end first (internal/jobs/SPEC.md §6). Site-changing agent calls need
// no delay: the agent's own lock of the site makes a rerun wait behind the
// call still running there.
const repoRerunDelay = 15 * time.Minute

// verifyNoCancel is why a restore test cannot be canceled.
const verifyNoCancel = "A restore test cannot be canceled: a backup counts only once a restore of it has been tested"

// stagingIDOf is the staging copy a staging job's payload names (0 when
// none).
func stagingIDOf(j state.Job) int64 {
	var p stagingPayload
	_ = json.Unmarshal([]byte(j.Payload), &p)
	return p.StagingID
}

// dropIfStatus removes the record of a site that is still in status:
// nothing was provisioned for it, so nothing is left on the server.
func (s *Server) dropIfStatus(id int64, status state.SiteStatus) {
	site, err := s.Store.GetSite(id)
	if err != nil || site.Status != status {
		return
	}
	s.rollbackCreatedSite(site)
}

// repoLocks is ResolveLocks for the kinds that run against the configured
// repository: the lock of the repository as configured now.
func (s *Server) repoLocks(j state.Job) []string {
	repo, _, err := s.backupRepository()
	if err != nil || repo == "" {
		return j.Locks
	}
	return []string{jobs.RepoLock(repo)}
}

func (s *Server) registerJobKinds() {
	q := s.Jobs
	// Kinds whose work is a function of the request (not resumable).
	for _, name := range []string{
		"certificate.issue", "staging.database-push",
		"wp.update", "cache.warm", "wp.cleanup",
		"database.export", "database.import", "dbstudio.export", "dbstudio.import",
		"database.create", "database.drop",
		"migration.import", "panel.certificate", "backup.restore", "php.extension",
		"apt.update", "apt.upgrade",
	} {
		q.Register(jobs.Kind{Name: name})
	}
	// Canceled before its start, a create provisioned nothing: the record
	// goes, as for a create the agent refused. Interrupted once started, the
	// site stays, in error, for a delete to clean up (R-3.13).
	q.Register(jobs.Kind{Name: "site.create",
		Canceled:    func(j state.Job) { s.dropIfStatus(j.SiteID, state.SiteProvisioning) },
		Interrupted: func(j state.Job) { s.markIfStatus(j.SiteID, state.SiteProvisioning) },
	})
	// Canceled before its start, a delete changed nothing: the site gets
	// back the status it had.
	q.Register(jobs.Kind{Name: "site.delete",
		Canceled: func(j state.Job) {
			var p deletePayload
			if json.Unmarshal([]byte(j.Payload), &p) != nil || p.PreviousStatus == "" {
				p.PreviousStatus = state.SiteActive
			}
			if site, err := s.Store.GetSite(j.SiteID); err == nil && site.Status == state.SiteDeleting {
				s.logIfErr(s.setSiteStatus(j.SiteID, p.PreviousStatus), "setSiteStatus")
			}
		},
		Interrupted: func(j state.Job) { s.markIfStatus(j.SiteID, state.SiteDeleting) },
	})
	q.Register(jobs.Kind{Name: "staging.create",
		Canceled: func(j state.Job) { s.dropIfStatus(stagingIDOf(j), state.SiteProvisioning) },
		Interrupted: func(j state.Job) {
			if id := stagingIDOf(j); id != 0 {
				s.markIfStatus(id, state.SiteProvisioning)
			}
		},
	})

	// Recreate changes nothing before its start; once started, a new copy
	// left provisioning goes to error.
	q.Register(jobs.Kind{Name: "staging.recreate", Interrupted: func(j state.Job) {
		if stg, err := s.stagingSvc().Of(state.Site{ID: j.SiteID}); err == nil {
			s.markIfStatus(stg.ID, state.SiteProvisioning)
		}
	}})

	// Resumable kinds.
	s.registerApplyKinds(q)
	q.Register(jobs.Kind{Name: "backup.run", Run: s.runSiteBackup, Rerun: true,
		ResolveLocks: s.repoLocks, RerunDelay: repoRerunDelay})
	q.Register(jobs.Kind{Name: "backup.scheduled", Run: s.runSiteBackup, Rerun: true,
		ResolveLocks: s.repoLocks, RerunDelay: repoRerunDelay,
		// The schedule asks again at its next sweep.
		Canceled: func(j state.Job) {
			s.logIfErr(s.Store.ClearScheduleRun(backupScheduleKey(j.SiteID)), "ClearScheduleRun")
		},
	})
	q.Register(jobs.Kind{Name: "backup.verify", Run: s.runVerifyBackup, Rerun: true,
		RerunDelay: repoRerunDelay, NoCancel: verifyNoCancel})
	q.Register(jobs.Kind{Name: "backup.retention", Run: s.runRetention, Rerun: true,
		ResolveLocks: s.repoLocks, RerunDelay: repoRerunDelay})
	// A deletion is idempotent (a snapshot already gone only drops its
	// record) and runs under the lock of the repository it is in.
	q.Register(jobs.Kind{Name: "backup.forget", Run: s.runForget, Rerun: true, RerunDelay: repoRerunDelay})
	q.Register(jobs.Kind{Name: "backup.prune", Run: s.runPrune, Rerun: true, Quiet: true,
		ResolveLocks: s.repoLocks, RerunDelay: repoRerunDelay})
	s.registerDeployKinds(q)
	// A scheduled WordPress update changes the site: interrupted, it is
	// not run again (the next slot is). Canceled before it ran, the
	// schedule asks again at its next sweep.
	q.Register(jobs.Kind{Name: "wp.update.scheduled", Run: s.runScheduledWPUpdate, ResolveLocks: s.wpUpdateJobLocks,
		Canceled: func(j state.Job) {
			s.logIfErr(s.Store.ClearScheduleRun(wpUpdateScheduleKey(j.SiteID)), "ClearScheduleRun")
		},
	})
	// A check is a read and a lookup: run again after a restart.
	q.Register(jobs.Kind{Name: "wp.vulncheck", Run: s.runVulnCheck, Rerun: true})
	// A scan reads the site; run again after a restart. Canceled before it
	// ran, the schedule asks again at the site's next slot.
	q.Register(jobs.Kind{Name: "site.malwarescan", Run: s.runMalwareScan, Rerun: true})

	// Maintenance (quiet).
	for name, run := range map[string]jobs.Func{
		"maintenance.reconcile": s.runReconcile,
		"metrics.sample":        s.runMetricSample,
		"sites.usage":           s.runSiteUsage,
		"backup.sweep":          s.runBackupSweep,
		"retention.sweep":       s.runRetentionSweep,
		"certificate.sweep":     s.runCertificateSweep,
		"certificate.renew":     s.runCertificateRenew,
		"drift.scan":            s.runDriftScan,
		"alerts.notify":         s.runAlertsNotify,
		"cache.rewarm":          s.runRewarm,
		"jobs.prune":            s.runPruneJobs,
		"security.sweep":        s.runSecuritySweep,
		"devmode.sweep":         s.runDevModeSweep,
		"wpcleanup.sweep":       s.runWPCleanupSweep,
		"wp.update.sweep":       s.runWPUpdateSweep,
		"cloudflare.ranges":     s.runCloudflareRanges,
	} {
		q.Register(jobs.Kind{Name: name, Quiet: true, Run: run, Rerun: true})
	}
}

// deletePayload is what a site.delete needs to be undone before its start.
type deletePayload struct {
	PreviousStatus state.SiteStatus `json:"previous_status"`
	StagingID      int64            `json:"staging_id,omitempty"`
}

// schedules is the panel's recurring work (internal/jobs/SPEC.md §9).
func (s *Server) schedules() []jobs.Schedule {
	every := func(name string, d time.Duration, mode jobs.TickMode, locks ...string) jobs.Schedule {
		return jobs.Schedule{Name: name, Every: d, Tick: mode,
			Spec: func() jobs.Spec { return jobs.Spec{Kind: name, Locks: locks, Background: true} }}
	}
	cert := every("certificate.sweep", 12*time.Hour, jobs.InTickWhenDue, jobs.PanelLock)
	// The key the old loop kept, so an upgrade does not renew early.
	cert.PersistKey = "cert.renew"
	return []jobs.Schedule{
		every("maintenance.reconcile", 5*time.Minute, jobs.InTick, jobs.RenderLock),
		every("metrics.sample", 5*time.Minute, jobs.InTick),
		every("sites.usage", 5*time.Minute, jobs.InTick),
		every("backup.sweep", 5*time.Minute, jobs.InTick),
		every("retention.sweep", 5*time.Minute, jobs.InTick),
		every("security.sweep", 5*time.Minute, jobs.InTick),
		every("devmode.sweep", time.Minute, jobs.InTick),
		every("wpcleanup.sweep", 5*time.Minute, jobs.InTick),
		every("wp.update.sweep", 5*time.Minute, jobs.InTick),
		cert,
		every("drift.scan", 5*time.Minute, jobs.InTick, jobs.RenderLock),
		every("alerts.notify", 5*time.Minute, jobs.AfterTick),
		every("jobs.prune", 6*time.Hour, jobs.NotInTick),
		every("backup.prune", 24*time.Hour, jobs.NotInTick),
		every("cloudflare.ranges", 24*time.Hour, jobs.NotInTick),
	}
}

// afterJobRecovery queues the restore test of every snapshot still waiting
// for one: verification is never skipped, also when the job that took the
// snapshot was canceled or the panel stopped before the test.
func (s *Server) afterJobRecovery() {
	// Deployments a restart cut short are settled from what the sites serve.
	s.queueDeployReconciles()
	pending, err := s.Store.UnverifiedBackups()
	if err != nil {
		s.logIfErr(err, "UnverifiedBackups")
		return
	}
	for _, b := range pending {
		if _, err := s.enqueueVerify(b, true); err != nil {
			s.Log.Warn("could not queue a pending restore test", "backup", b.ID, "err", err)
		}
	}
}

// runPruneJobs drops old jobs (internal/jobs/SPEC.md §7).
func (s *Server) runPruneJobs(ctx context.Context, r *jobs.Run) error {
	now := time.Now()
	n, err := s.Store.PruneJobs(now.Add(-24*time.Hour), now.Add(-30*24*time.Hour))
	if err != nil {
		return err
	}
	r.Progress(100, fmt.Sprintf("%d old jobs removed", n))
	return nil
}
