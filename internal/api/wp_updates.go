package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/guard"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// WordPress updates with a snapshot and a rollback, and their schedule
// (SPEC.md §6.2.3). An update runs only with a tested snapshot to go back
// to; Performance Guard measures the site before it and checks it after,
// and a site the update broke is restored from that snapshot.

// wpUpdateTargets are what an update may name (ops/wp's updateSteps).
var wpUpdateTargets = []string{"core", "plugins", "themes", "all"}

// wpUpdatePaths are the pages the health check requests: the home page
// (cached and uncached as Guard's defaults ask) and the sign-in page, which
// a broken plugin often takes down alone.
var wpUpdatePaths = append(slices.Clone(guard.DefaultProbePaths), "/wp-login.php")

// Messages of the update routes.
const (
	msgWPUpdateWhat     = "what must be core, plugins, themes or all"
	msgWPUpdateRepo     = "configure backup_repository and backup_password in settings first"
	msgWPUpdateStaging  = "a staging site has no update schedule of its own"
	msgWPUpdateEmpty    = "name at least one of enabled, schedule, hour, what"
	msgWPUpdateSchedule = "schedule must be daily or weekly"
)

// wpUpdatePayload is what an update job records and a scheduled one runs
// from. Domain names the site in a failure's alert once it is gone.
type wpUpdatePayload struct {
	SiteID int64  `json:"site_id"`
	Domain string `json:"domain,omitempty"`
	What   string `json:"what"`
	// WithoutSnapshot: asked for with no backup repository; nothing to
	// roll back to.
	WithoutSnapshot bool `json:"without_snapshot,omitempty"`
}

// wpUpdateLocks are an update's locks: the site's and the repository's
// it is snapshotted to.
func wpUpdateLocks(siteID int64, repo string) []string {
	return append(siteLocks(siteID), jobs.RepoLock(repo))
}

// update starts the update of core, plugins, themes or all of them behind
// a tested snapshot (SPEC.md §6.2.3). With no backup repository it runs
// only when the caller asked for it without a snapshot: checked, but with
// nothing to roll back to.
func (p wordPress) update(site state.Site, what string, withoutSnapshot bool) (jobs.Task, error) {
	s := p.s
	if !isWordPress(site.Type) {
		return jobs.Task{}, errNotWordPress
	}
	if !slices.Contains(wpUpdateTargets, what) {
		return jobs.Task{}, decline(http.StatusBadRequest, msgWPUpdateWhat)
	}
	repo, password, err := s.backupRepository()
	if err != nil {
		return jobs.Task{}, stored(err)
	}
	if repo == "" || password == "" {
		if !withoutSnapshot {
			return jobs.Task{}, decline(http.StatusPreconditionFailed, msgWPUpdateRepo)
		}
		return s.runJob(jobReq{kind: "wp.update", siteID: site.ID, locks: siteLocks(site.ID),
			payload: wpUpdatePayload{SiteID: site.ID, Domain: site.Domain, What: what, WithoutSnapshot: true}},
			func(ctx context.Context, site state.Site, step func(int, string)) error {
				return s.safeWPUpdate(ctx, site, what, "", step)
			})
	}
	return s.runJob(jobReq{kind: "wp.update", siteID: site.ID, locks: wpUpdateLocks(site.ID, repo),
		payload: wpUpdatePayload{SiteID: site.ID, Domain: site.Domain, What: what}},
		func(ctx context.Context, site state.Site, step func(int, string)) error {
			return s.safeWPUpdate(ctx, site, what, repo, step)
		})
}

// safeWPUpdate is an update's steps: the baseline, a tested snapshot, the
// update, the health check, and the rollback when that fails. A cancel
// stops it until the update is sent; from there it runs to its end.
// lockedRepo is the repository whose lock the job holds; "" is an update
// asked for without a snapshot: no snapshot, no rollback.
func (s *Server) safeWPUpdate(ctx context.Context, site state.Site, what, lockedRepo string, step func(int, string)) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if lockedRepo != "" && (repo == "" || password == "") {
		return errBackupsOff
	}
	if lockedRepo != "" && repo != lockedRepo {
		return errors.New("the backup repository changed while the update waited; nothing was changed, start it again")
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return err
	}
	token, err := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretConnectorToken))
	if err != nil {
		return err
	}
	engine := newProber(ps.EffectiveProbeTarget(), guardWorkers(site))
	ep := guard.Endpoint{Host: site.Domain, ProbeKey: guard.ProbeKeyFor(token)}
	thresholds := guard.DefaultThresholds()

	step(5, "Measuring "+site.Domain+" before the update")
	baseline, err := engine.Measure(ctx, ep, wpUpdatePaths)
	if err != nil {
		if ctx.Err() != nil {
			return checkpoint(ctx, step, "the snapshot")
		}
		return fmt.Errorf("the site could not be measured before the update, so nothing was changed: %w", err)
	}
	var snap state.Backup
	next := "the update"
	if lockedRepo != "" {
		if snap, err = s.safetySnapshot(ctx, site, repo, password, "pre-update", step); err != nil {
			return err
		}
		next += " (the snapshot " + snap.SnapshotID + " is kept)"
	} else {
		step(-1, "No backup repository: updating without a snapshot, so a broken update cannot be undone automatically")
	}
	if err := checkpoint(ctx, step, next); err != nil {
		return err
	}
	// From here the site changes: the health check and a rollback must
	// run whatever happens to the job.
	ctx = context.WithoutCancel(ctx)
	step(50, "Updating "+what+" on "+site.Domain)
	updateErr := agentrpc.Into(ctx, s.Agent, agentrpc.WPUpdate, rpc.WPParams{Site: site, What: what}, nil)
	if updateErr != nil {
		step(-1, "The update reported an error ("+firstLine(updateErr.Error())+"); checking the site before deciding")
	}
	s.purgeQuietly(ctx, site)

	step(70, "Health check: comparing "+site.Domain+" with before the update")
	report, err := engine.Verify(ctx, ep, baseline, wpUpdatePaths, thresholds)
	var why string
	switch {
	case err != nil:
		why = "the health check could not run (" + err.Error() + ")"
	case report.Verdict == guard.VerdictBlock:
		why = strings.Join(report.Reasons, "; ")
	}
	if why == "" {
		for _, r := range report.Reasons {
			step(-1, r)
		}
		if updateErr != nil {
			return updateErr
		}
		s.warmInBackground(site)
		step(100, "Update complete; the health check passed")
		return nil
	}

	if lockedRepo == "" {
		s.audit("system", "wp.update.broken", site.Domain, why)
		return fmt.Errorf("the update broke the site and there was no snapshot to roll back to: %s", why)
	}
	step(80, "The update broke the site ("+why+"); restoring snapshot "+snap.SnapshotID)
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RestoreSnapshot, rpc.RestoreParams{
		Env: s.backupEnv(), Site: site, Repository: snap.Repository, Password: password, SnapshotID: snap.SnapshotID, Mode: "full",
	}, nil); err != nil {
		s.audit("system", "wp.update.rollback", site.Domain, "failed: "+why)
		return fmt.Errorf("the update broke the site and the automatic rollback failed (snapshot %s is kept; restore it from Backups): %w", snap.SnapshotID, err)
	}
	s.purgeQuietly(ctx, site)
	s.audit("system", "wp.update.rollback", site.Domain, why)
	step(100, "Rolled back to snapshot "+snap.SnapshotID)
	return fmt.Errorf("the update broke the site and was rolled back to snapshot %s: %s", snap.SnapshotID, why)
}

// ---------------------------------------------------------------- schedule --

func wpUpdateScheduleKey(siteID int64) string {
	return "wp.update.site." + strconv.FormatInt(siteID, 10)
}

// wpUpdatePolicyAsBackup is a schedule as backupDue reads one (SPEC.md
// §5.5); an empty schedule is weekly.
func wpUpdatePolicyAsBackup(p state.WPUpdatePolicy) state.BackupPolicy {
	schedule := p.Schedule
	if schedule == "" {
		schedule = "weekly"
	}
	return state.BackupPolicy{Enabled: p.Enabled, Schedule: schedule, Hour: p.Hour}
}

// wpUpdateTarget is what a schedule updates; empty is all.
func wpUpdateTarget(p state.WPUpdatePolicy) string {
	if p.What == "" {
		return "all"
	}
	return p.What
}

// scheduledUpdate reports whether a site's updates run on a schedule.
func scheduledUpdate(site state.Site) bool {
	return site.Status == state.SiteActive && site.StagingOf == 0 && isWordPress(site.Type) && site.Config.WPUpdates.Enabled
}

// runWPUpdateSweep queues the scheduled update of every site that is due.
// The key is set when the job is queued, so a failure waits for the site's
// next slot.
func (s *Server) runWPUpdateSweep(ctx context.Context, r *jobs.Run) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		return nil // no snapshot to go back to: nothing is updated
	}
	all, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	queued := 0
	for _, site := range all {
		if !scheduledUpdate(site) {
			continue
		}
		key := wpUpdateScheduleKey(site.ID)
		last, _ := s.Store.LastScheduleRun(key)
		if !backupDue(wpUpdatePolicyAsBackup(site.Config.WPUpdates), last, time.Now()) {
			continue
		}
		if _, err := s.Jobs.Enqueue(jobs.Spec{
			Kind: "wp.update.scheduled", SiteID: site.ID, Locks: wpUpdateLocks(site.ID, repo), Background: true,
			DedupKey: "wp.update.scheduled:" + strconv.FormatInt(site.ID, 10),
			Payload:  wpUpdatePayload{SiteID: site.ID, Domain: site.Domain, What: wpUpdateTarget(site.Config.WPUpdates)},
		}); err != nil {
			return err
		}
		s.logIfErr(s.Store.MarkScheduleRun(key), "MarkScheduleRun")
		queued++
	}
	r.Progress(100, fmt.Sprintf("%d scheduled updates queued", queued))
	return nil
}

// wpUpdateJobLocks is ResolveLocks for a queued scheduled update: the
// site's lock and the repository's as configured now.
func (s *Server) wpUpdateJobLocks(j state.Job) []string {
	repo, _, err := s.backupRepository()
	if err != nil || repo == "" {
		return j.Locks
	}
	return wpUpdateLocks(j.SiteID, repo)
}

// runScheduledWPUpdate is wp.update.scheduled: the site as stored now, its
// schedule still on, and something to update.
func (s *Server) runScheduledWPUpdate(ctx context.Context, r *jobs.Run) error {
	var p wpUpdatePayload
	if err := r.Payload(&p); err != nil {
		return err
	}
	site, err := s.Store.GetSite(r.Job().SiteID)
	if errors.Is(err, state.ErrNotFound) || err == nil && !scheduledUpdate(site) {
		r.Progress(100, "Skipped: the site's updates are no longer scheduled")
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
	// Plugins and themes say whether an update is pending; core does not
	// (the inventory has its version only), so it is always run.
	if p.What == "plugins" || p.What == "themes" {
		inv, err := agentrpc.Call(ctx, s.Agent, agentrpc.WPPlugins, rpc.WPParams{Site: site})
		if err != nil {
			return err
		}
		list := inv.Plugins
		if p.What == "themes" {
			list = inv.Themes
		}
		if !slices.ContainsFunc(list, func(x rpc.WPPlugin) bool { return x.Update == "available" }) {
			r.Progress(100, "Nothing to update")
			return nil
		}
	}
	return s.safeWPUpdate(ctx, site, p.What, repo, r.Progress)
}

// wpUpdatePolicyChange is the body of PUT /api/sites/{id}/wp/updates. A
// nil field keeps its value.
type wpUpdatePolicyChange struct {
	Enabled  *bool   `json:"enabled"`
	Schedule *string `json:"schedule"`
	// Hour is 0–23, or -1 for no fixed hour.
	Hour *int    `json:"hour"`
	What *string `json:"what"`
}

// check says what is wrong with the change for site, field by field.
func (pc wpUpdatePolicyChange) check(site state.Site) (kind problemKind, msg string, fields map[string]string) {
	if site.StagingOf != 0 {
		return probInvalid, msgWPUpdateStaging, nil
	}
	if !isWordPress(site.Type) {
		return probInvalid, "not a WordPress site", nil
	}
	if pc.Enabled == nil && pc.Schedule == nil && pc.Hour == nil && pc.What == nil {
		return probInvalid, msgWPUpdateEmpty, nil
	}
	fields = map[string]string{}
	if pc.Schedule != nil && *pc.Schedule != "daily" && *pc.Schedule != "weekly" {
		fields["schedule"] = msgWPUpdateSchedule
	}
	if pc.Hour != nil && (*pc.Hour < -1 || *pc.Hour > 23) {
		fields["hour"] = msgPolicyHour
	}
	if pc.What != nil && !slices.Contains(wpUpdateTargets, *pc.What) {
		fields["what"] = msgWPUpdateWhat
	}
	for _, name := range []string{"schedule", "hour", "what"} {
		if m, ok := fields[name]; ok {
			return probInvalid, m, fields
		}
	}
	return 0, "", nil
}

// apply writes the change into a policy.
func (pc wpUpdatePolicyChange) apply(p *state.WPUpdatePolicy) {
	if pc.Enabled != nil {
		p.Enabled = *pc.Enabled
	}
	if pc.Schedule != nil {
		p.Schedule = *pc.Schedule
	}
	if pc.What != nil {
		p.What = *pc.What
	}
	if pc.Hour != nil {
		p.Hour = nil
		if h := *pc.Hour; h >= 0 {
			p.Hour = &h
		}
	}
	if p.Schedule == "" {
		p.Schedule = "weekly"
	}
	if p.What == "" {
		p.What = "all"
	}
}

// handleWPUpdatePolicy is PUT /api/sites/{id}/wp/updates. It answers the
// schedule as stored.
func (s *Server) handleWPUpdatePolicy(w http.ResponseWriter, r *http.Request, c *call) {
	var pc wpUpdatePolicyChange
	if err := decode(r, &pc); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	if kind, msg, fields := pc.check(c.Site); msg != "" {
		replyFailure(w, &problem{kind: kind, msg: msg, fields: fields})
		return
	}
	if pc.Enabled != nil && *pc.Enabled {
		if _, _, ok := s.backupConfig(w); !ok {
			return
		}
	}
	site, err := s.Store.MutateSiteApplied(c.Site.ID, func(st *state.Site) error {
		pc.apply(&st.Config.WPUpdates)
		return nil
	})
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "site not found")
		return
	}
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	p := site.Config.WPUpdates
	hour := "any"
	if p.Hour != nil {
		hour = fmt.Sprintf("%02d:00", *p.Hour)
	}
	s.audit(c.Actor(), "wp.update_policy", site.Domain,
		fmt.Sprintf("enabled=%t schedule=%s hour=%s what=%s", p.Enabled, p.Schedule, hour, p.What))
	respond(w, http.StatusOK, p)
}
