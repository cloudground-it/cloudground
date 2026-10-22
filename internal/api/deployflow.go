package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/guard"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The deployment state machine's jobs (SPEC.md §4). Every deploy kind runs
// from its payload, so a queued one survives a restart; each job moves its
// deployment through the store (state.MoveDeployment), which refuses a
// transition the machine does not allow. What the site serves is the
// agent's to say (CurrentRelease): after any doubt the records follow it
// (deployreconcile.go).

// deployPayload is what a deploy job runs from. It never holds a secret.
type deployPayload struct {
	DeploymentID int64 `json:"deployment_id"`
	// SourceDir is the tree a deploy.create builds from.
	SourceDir string `json:"source_dir,omitempty"`
	// From is the status a deploy.promote found the release in, and goes
	// back to if the promote fails.
	From state.DeploymentStatus `json:"from,omitempty"`
	// The safe push: the staging copy measured, the paths of a selective
	// push, the probe paths and whether a warning may be overridden.
	StagingID int64    `json:"staging_id,omitempty"`
	Only      []string `json:"only,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Force     bool     `json:"force,omitempty"`
}

// rollbackPayload is a deploy.rollback: the site and the release it goes
// back to, chosen when the rollback was asked for.
type rollbackPayload struct {
	SiteID int64  `json:"site_id"`
	Target string `json:"target"`
}

// prober is Performance Guard as the safe push and a WordPress update
// (wp_updates.go) use it.
type prober interface {
	Compare(ctx context.Context, baseline, candidate guard.Endpoint, paths []string, t guard.Thresholds) (guard.Report, error)
	Verify(ctx context.Context, production guard.Endpoint, baseline []guard.Sample, paths []string, t guard.Thresholds) (guard.Report, error)
	Measure(ctx context.Context, ep guard.Endpoint, paths []string) ([]guard.Sample, error)
}

// newProber is the paired engine (tests replace it).
var newProber = func(target string, workers int) prober {
	return &guard.Engine{Target: target, Workers: workers}
}

// Notes a deployment can carry (state.DeploymentMeta.Note): each raises an
// alert. "drift:<release>" names the release the records had said was live.
const (
	noteDrift      = "drift"
	noteUnverified = "unverified"
	noteRegressed  = "regressed"
)

// registerDeployKinds registers the deploy kinds (SPEC.md §4.5).
func (s *Server) registerDeployKinds(q *jobs.Queue) {
	q.Register(jobs.Kind{Name: "deploy.create", Run: s.runDeployCreate,
		Canceled: s.settleDeployment, Interrupted: s.settleDeployment})
	// A promote is idempotent (the same release made live again), so one
	// a restart cut short runs again.
	q.Register(jobs.Kind{Name: "deploy.promote", Run: s.runDeployPromote, Rerun: true,
		Interrupted: s.settleDeployment})
	// A rollback is not run twice for one request; one a restart cut short
	// leaves the site to be reconciled with what it serves.
	q.Register(jobs.Kind{Name: "deploy.rollback", Run: s.runDeployRollback,
		Interrupted: func(j state.Job) { s.flagReconcile(j.SiteID) }})
	// A safe push resumes from the status it reached.
	q.Register(jobs.Kind{Name: "deploy.safe-push", Run: s.runSafePush, Rerun: true,
		Canceled: s.settleDeployment, Interrupted: s.settleDeployment})
	q.Register(jobs.Kind{Name: "deploy.reconcile", Run: s.runDeployReconcile, Rerun: true})
}

// unverifiedReport is the Guard result of a release whose verification a
// person canceled.
const unverifiedReport = `{"verdict":"unverified","reasons":["verification canceled"]}`

// settleDeployment is the Canceled and Interrupted hook of the deploy
// kinds (SPEC.md §4.3). It settles only what is certain without asking
// the agent (hooks do not); a deployment whose release may or may not be
// live is left as it is and its site flagged, and the reconcile job the
// next start queues asks the agent.
func (s *Server) settleDeployment(j state.Job) {
	var p deployPayload
	if json.Unmarshal([]byte(j.Payload), &p) != nil || p.DeploymentID == 0 {
		return
	}
	d, err := s.Store.GetDeployment(p.DeploymentID)
	if err != nil {
		return
	}
	var to state.DeploymentStatus
	u := state.DeploymentUpdate{}
	switch d.Status {
	case state.DeployPending, state.DeployGuarding:
		to = state.DeployFailed
		if j.Status == state.JobCanceled {
			to = state.DeployCanceled
		}
	case state.DeployBuilding:
		// Its directory may have been published: the reconcile removes it.
		to = state.DeployFailed
		s.flagReconcile(d.SiteID)
	case state.DeployVerifying:
		if j.Status != state.JobCanceled {
			s.flagReconcile(d.SiteID) // verified (or rolled back) by the reconcile
			return
		}
		// A person canceled the verification: live, unverified, said so.
		to, u = state.DeployPromoted, state.DeploymentUpdate{GuardJSON: unverifiedReport, Note: noteUnverified}
	case state.DeployPromoting:
		s.flagReconcile(d.SiteID)
		return
	default:
		return
	}
	_, err = s.Store.MoveDeployment(d.ID, []state.DeploymentStatus{d.Status}, to, u)
	s.logIfErr(err, "MoveDeployment")
}

// deployJob is what every deploy job starts from: its payload, its site as
// it is now, and its deployment.
func (s *Server) deployJob(ctx context.Context, r *jobs.Run) (deployPayload, state.Site, state.Deployment, error) {
	var p deployPayload
	if err := r.Payload(&p); err != nil {
		return p, state.Site{}, state.Deployment{}, err
	}
	d, err := s.Store.GetDeployment(p.DeploymentID)
	if errors.Is(err, state.ErrNotFound) {
		return p, state.Site{}, d, errors.New("the deployment's record is gone")
	}
	if err != nil {
		return p, state.Site{}, d, err
	}
	site, err := s.jobSite(r.Job().SiteID, r.Job().Kind)
	if err != nil {
		return p, site, d, s.settleFailed(&d, err)
	}
	return p, site, d, nil
}

// move is MoveDeployment for a job: a refused move fails the job.
func (s *Server) move(d *state.Deployment, to state.DeploymentStatus, u state.DeploymentUpdate) error {
	moved, err := s.Store.MoveDeployment(d.ID, []state.DeploymentStatus{d.Status}, to, u)
	if err != nil {
		return err
	}
	*d = moved
	return nil
}

// stop ends a deploy job at a step boundary when it was canceled: the
// deployment becomes canceled, unless the panel is stopping, when the
// queue reruns the job or settles it.
func (s *Server) stop(ctx context.Context, d *state.Deployment, progress func(int, string), next string) error {
	err := checkpoint(ctx, progress, next)
	if err == nil || jobs.ShuttingDown(ctx) {
		return err
	}
	s.logIfErr(s.move(d, state.DeployCanceled, state.DeploymentUpdate{}), "MoveDeployment")
	return err
}

// fail moves a deployment to failed and returns the job's error.
func (s *Server) fail(d *state.Deployment, err error) error {
	s.logIfErr(s.move(d, state.DeployFailed, state.DeploymentUpdate{}), "MoveDeployment")
	return err
}

// settleFailed fails a deployment that has not changed production yet.
func (s *Server) settleFailed(d *state.Deployment, err error) error {
	if d.Status == state.DeployPending || d.Status == state.DeployGuarding {
		return s.fail(d, err)
	}
	return err
}

// build has the agent build the deployment's release and records it
// (building → to).
func (s *Server) build(ctx context.Context, d *state.Deployment, params rpc.DeployParams, guardJSON string, to state.DeploymentStatus) error {
	if err := s.move(d, state.DeployBuilding, state.DeploymentUpdate{GuardJSON: guardJSON}); err != nil {
		return err
	}
	params.ReleaseID = d.ReleaseID
	var res rpc.DeployResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.DeployRelease, params, &res); err != nil {
		return s.fail(d, err)
	}
	// The connector the release carries is the panel's: drift compares
	// against what was just written.
	for _, f := range res.Managed {
		s.logIfErr(s.Store.RecordManagedFile(f.Path, f.SHA256), "RecordManagedFile")
	}
	return s.move(d, to, state.DeploymentUpdate{Path: res.Path, Checksum: res.Checksum})
}

// rememberRollbackTo records, before a promotion, the release the site
// serves: where a rollback from this deployment goes.
func (s *Server) rememberRollbackTo(ctx context.Context, site state.Site, d state.Deployment) (string, error) {
	cur, err := s.currentRelease(ctx, site)
	if err != nil {
		return "", err
	}
	if cur != "" && cur != d.ReleaseID {
		s.logIfErr(s.Store.SetRollbackTo(d.ID, cur), "SetRollbackTo")
	}
	return cur, nil
}

// runDeployCreate: pending → building → created.
func (s *Server) runDeployCreate(ctx context.Context, r *jobs.Run) error {
	p, site, d, err := s.deployJob(ctx, r)
	if err != nil {
		return err
	}
	if d.Status != state.DeployPending {
		return fmt.Errorf("the deployment is %s, not waiting to be built", d.Status)
	}
	if err := s.stop(ctx, &d, r.Progress, "building the release"); err != nil {
		return err
	}
	r.Progress(20, "Creating release "+d.ReleaseID)
	if err := s.build(context.WithoutCancel(ctx), &d, rpc.DeployParams{Site: site, SourceDir: p.SourceDir}, "", state.DeployCreated); err != nil {
		return err
	}
	r.Progress(100, "Release "+d.ReleaseID+" staged (not yet live)")
	return nil
}

// runDeployPromote: from → promoting → promoted, or back to from. After a
// refused promote the records follow what the site serves (the agent may
// have swapped current and then failed to reload PHP).
func (s *Server) runDeployPromote(ctx context.Context, r *jobs.Run) error {
	p, site, d, err := s.deployJob(ctx, r)
	if err != nil {
		return err
	}
	switch d.Status {
	case state.DeployPromoting: // a rerun: the promote did not finish
	case p.From:
		if err := checkpoint(ctx, r.Progress, "promoting "+d.ReleaseID); err != nil {
			return err
		}
		if err := s.move(&d, state.DeployPromoting, state.DeploymentUpdate{}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("release %s is now %s (it was %s when the promote was asked for)", d.ReleaseID, d.Status, p.From)
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := s.rememberRollbackTo(ctx, site, d); err != nil {
		s.logIfErr(s.move(&d, p.From, state.DeploymentUpdate{}), "MoveDeployment")
		return fmt.Errorf("could not read the live release, so %s was not promoted: %w", d.ReleaseID, err)
	}
	r.Progress(40, "Promoting "+d.ReleaseID)
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.PromoteRelease, rpc.ReleaseParams{Site: site, ReleaseID: d.ReleaseID}, nil); err != nil {
		s.logIfErr(s.move(&d, p.From, state.DeploymentUpdate{}), "MoveDeployment")
		s.reconcileLive(ctx, site)
		return err
	}
	if err := s.move(&d, state.DeployPromoted, state.DeploymentUpdate{}); err != nil {
		return err
	}
	r.Progress(80, "Purging page cache")
	s.purgeQuietly(ctx, site)
	r.Progress(90, "Warming cache so the first visitor is not cold")
	s.warmInBackground(site)
	r.Progress(100, d.ReleaseID+" is live")
	return nil
}

// runDeployRollback makes live again the release chosen when the rollback
// was asked for (never one the agent picks: the newest older release may
// be one that was built and never promoted, or one a safe push rolled
// back for a regression), and records it.
func (s *Server) runDeployRollback(ctx context.Context, r *jobs.Run) error {
	var p rollbackPayload
	if err := r.Payload(&p); err != nil {
		return err
	}
	site, err := s.jobSite(r.Job().SiteID, r.Job().Kind)
	if err != nil {
		return err
	}
	if p.Target == "" {
		return errors.New("the rollback names no release to go back to")
	}
	if p.Target == siteval.InitialRelease {
		s.recordInitialRelease(site.ID, state.DeploySuperseded)
	}
	if err := s.Store.CheckRollbackTarget(site.ID, p.Target); err != nil {
		return fmt.Errorf("release %s: %w", p.Target, err)
	}
	if err := checkpoint(ctx, r.Progress, "the rollback"); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	r.Progress(40, "Rolling back "+site.Domain+" to "+p.Target)
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RollbackRelease, rpc.ReleaseParams{Site: site, ReleaseID: p.Target}, nil); err != nil {
		s.reconcileLive(ctx, site)
		return err
	}
	if _, err := s.Store.RecordRollback(site.ID, p.Target); err != nil {
		s.reconcileLive(ctx, site)
		return fmt.Errorf("now serving %s, but the rollback could not be recorded: %w", p.Target, err)
	}
	r.Progress(80, "Purging page cache")
	s.purgeQuietly(ctx, site)
	r.Progress(100, "Now serving "+p.Target)
	return nil
}

// purgeQuietly empties a site's page cache; a failure only costs a cold cache.
func (s *Server) purgeQuietly(ctx context.Context, site state.Site) {
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: site}, nil); err != nil {
		s.Log.Warn("cache purge after a release change failed", "site", site.Domain, "err", err)
	}
}

// safePush is one run of a safe push (or the verification the reconcile
// finishes for one).
type safePush struct {
	s          *Server
	p          deployPayload
	prod, stg  state.Site
	d          state.Deployment
	progress   func(int, string)
	engine     prober
	production guard.Endpoint
	thresholds guard.Thresholds
}

// newSafePush prepares the measurement of prod (and stg, when measured).
func (s *Server) newSafePush(prod state.Site, workers int, d state.Deployment, p deployPayload, progress func(int, string)) (*safePush, error) {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return nil, err
	}
	token, err := s.Store.Secret(state.SiteSecret(prod.ID, state.SiteSecretConnectorToken))
	if err != nil {
		return nil, err
	}
	return &safePush{s: s, p: p, prod: prod, d: d, progress: progress,
		engine:     newProber(ps.EffectiveProbeTarget(), workers),
		production: guard.Endpoint{Host: prod.Domain, ProbeKey: guard.ProbeKeyFor(token)},
		thresholds: guard.DefaultThresholds()}, nil
}

// runSafePush: pending → guarding → blocked, or → building → promoting →
// verifying → promoted | rolled_back. A rerun resumes from the status the
// deployment reached (SPEC.md §4.4).
func (s *Server) runSafePush(ctx context.Context, r *jobs.Run) error {
	p, prod, d, err := s.deployJob(ctx, r)
	if err != nil {
		return err
	}
	stg, err := s.Store.GetSite(p.StagingID)
	if err != nil {
		return s.settleFailed(&d, fmt.Errorf("the staging copy: %w", err))
	}
	sp, err := s.newSafePush(prod, guardWorkers(prod, stg), d, p, r.Progress)
	if err != nil {
		return s.settleFailed(&d, err)
	}
	sp.stg = stg
	switch d.Status {
	case state.DeployPending, state.DeployGuarding:
		return sp.measure(ctx)
	case state.DeployBuilding:
		// The build a restart cut short may or may not have finished; its
		// name cannot be built again. The reconcile removes the directory.
		s.flagReconcile(prod.ID)
		return s.fail(&sp.d, errors.New("interrupted by a panel restart while the release was being built; push again"))
	case state.DeployPromoting:
		return sp.promote(context.WithoutCancel(ctx))
	case state.DeployVerifying:
		return sp.verify(context.WithoutCancel(ctx))
	}
	return fmt.Errorf("the deployment is already %s", d.Status)
}

// measure compares staging with production and, when the verdict allows,
// deploys staging's code. A cancel stops it; production is untouched.
func (sp *safePush) measure(ctx context.Context) error {
	s := sp.s
	if err := s.stop(ctx, &sp.d, sp.progress, "the comparison"); err != nil {
		return err
	}
	if sp.d.Status == state.DeployPending {
		if err := s.move(&sp.d, state.DeployGuarding, state.DeploymentUpdate{}); err != nil {
			return err
		}
	}
	// Each round requests both sides, in random order, bypassing the page
	// cache, so whatever the machine does lands on both and the verdict
	// rests on the per-round ratio (internal/guard/SPEC.md §4).
	sp.progress(10, "Comparing staging with production (paired, uncached requests)")
	stgToken, err := s.Store.Secret(state.SiteSecret(sp.stg.ID, state.SiteSecretConnectorToken))
	if err != nil {
		return s.settleFailed(&sp.d, err)
	}
	report, err := sp.engine.Compare(ctx, sp.production,
		guard.Endpoint{Host: sp.stg.Domain, ProbeKey: guard.ProbeKeyFor(stgToken), InternalKey: sp.stg.InternalKey()},
		sp.p.Paths, sp.thresholds)
	if err != nil {
		if ctx.Err() != nil {
			return s.stop(ctx, &sp.d, sp.progress, "the end of the comparison")
		}
		return s.fail(&sp.d, fmt.Errorf("performance comparison: %w", err))
	}
	sp.progress(55, "Comparing performance")
	reportJSON, _ := json.Marshal(report)
	if report.Verdict == guard.VerdictBlock ||
		(report.Verdict == guard.VerdictWarn || report.Verdict == guard.VerdictInconclusive) && !sp.p.Force {
		s.logIfErr(s.move(&sp.d, state.DeployBlocked, state.DeploymentUpdate{GuardJSON: string(reportJSON)}), "MoveDeployment")
		for _, reason := range report.Reasons {
			sp.progress(60, reason)
		}
		if report.Verdict == guard.VerdictInconclusive {
			return errors.New("performance guard: inconclusive, the server was not stable enough to measure — promotion refused (retry when the box is quiet, or force to override)")
		}
		return fmt.Errorf("performance guard verdict: %s — promotion refused", report.Verdict)
	}
	// From here production changes: a cancel stops nothing any more, least
	// of all the verification (cut short, it would roll back).
	if err := s.stop(ctx, &sp.d, sp.progress, "deploying staging's code to production"); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	sp.progress(65, "Guard verdict: "+string(report.Verdict)+" — deploying staging code to production")
	params := rpc.DeployParams{Site: sp.prod, SourceDir: filepath.Join(sp.stg.RootPath, "current"), SourceSite: &sp.stg}
	if len(sp.p.Only) > 0 {
		// Production's own release with only the chosen paths from staging;
		// Guard measured the whole of staging, a superset of what goes live.
		params.SourceDir, params.Overlay = filepath.Join(sp.prod.RootPath, "current"), sp.p.Only
		sp.progress(66, "Selective push: "+strings.Join(sp.p.Only, ", "))
	}
	if err := s.build(ctx, &sp.d, params, string(reportJSON), state.DeployPromoting); err != nil {
		return err
	}
	return sp.promote(ctx)
}

// promote makes the built release live (promoting → verifying) and checks
// it. The release it replaces is recorded first: it is where an automatic
// rollback goes, and without it nothing is promoted.
func (sp *safePush) promote(ctx context.Context) error {
	s := sp.s
	if _, err := s.rememberRollbackTo(ctx, sp.prod, sp.d); err != nil {
		s.logIfErr(s.move(&sp.d, state.DeployCreated, state.DeploymentUpdate{}), "MoveDeployment")
		return fmt.Errorf("could not read the live release, so %s was not promoted: %w", sp.d.ReleaseID, err)
	}
	sp.progress(85, "Promoting "+sp.d.ReleaseID)
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.PromoteRelease, rpc.ReleaseParams{Site: sp.prod, ReleaseID: sp.d.ReleaseID}, nil); err != nil {
		// The agent may have swapped current before it failed (a PHP
		// reload): then the release is live, and is verified now.
		if cur, cerr := s.currentRelease(ctx, sp.prod); cerr == nil && cur == sp.d.ReleaseID {
			if merr := s.move(&sp.d, state.DeployVerifying, state.DeploymentUpdate{}); merr != nil {
				return errors.Join(err, merr)
			}
			sp.progress(-1, "The promote failed after making the release live ("+err.Error()+"); verifying it now")
			return sp.verify(ctx)
		}
		s.logIfErr(s.move(&sp.d, state.DeployCreated, state.DeploymentUpdate{}), "MoveDeployment")
		s.reconcileLive(ctx, sp.prod)
		return err
	}
	if err := s.move(&sp.d, state.DeployVerifying, state.DeploymentUpdate{}); err != nil {
		return err
	}
	s.purgeQuietly(ctx, sp.prod)
	return sp.verify(ctx)
}

// verify measures production against the baseline Guard recorded before
// the promotion and rolls back on a regression. Right after a promotion
// PHP has reloaded and the cache is cold, so latency is not comparable:
// only new errors or a changed status roll back (internal/guard/SPEC.md
// §6). A verification that cannot run at all rolls back too.
func (sp *safePush) verify(ctx context.Context) error {
	s := sp.s
	var before guard.Report
	if err := json.Unmarshal([]byte(sp.d.GuardJSON), &before); err != nil || len(before.Baseline) == 0 {
		return sp.rollback(ctx, "", "the comparison's baseline is unreadable, so production cannot be verified",
			errors.New("post-promotion verification could not run (no baseline); automatically rolled back"))
	}
	paths := sp.p.Paths
	if len(paths) == 0 {
		for _, b := range before.Baseline {
			paths = append(paths, b.Path)
		}
	}
	sp.progress(90, "Verifying production after promotion")
	live, err := sp.engine.Verify(ctx, sp.production, before.Baseline, paths, sp.thresholds)
	if err != nil {
		return sp.rollback(ctx, "", fmt.Sprintf("post-promotion verification failed (%v)", err),
			fmt.Errorf("post-promotion verification failed; automatically rolled back: %w", err))
	}
	liveJSON, _ := json.Marshal(live)
	if live.Verdict == guard.VerdictBlock {
		sp.progress(94, "Production regression detected — rolling back automatically")
		return sp.rollback(ctx, string(liveJSON), "production regressed",
			fmt.Errorf("production regression detected; automatically rolled back: %s", strings.Join(live.Reasons, "; ")))
	}
	if err := s.move(&sp.d, state.DeployPromoted, state.DeploymentUpdate{GuardJSON: string(liveJSON)}); err != nil {
		return err
	}
	s.warmInBackground(sp.prod)
	sp.progress(100, "Safe push verified: "+sp.d.ReleaseID+" is live")
	return nil
}

// rollback undoes a promotion that failed its verification: the release
// that was live before this one (recorded before the promotion) goes live
// again, and this one is rolled_back, marked regressed (never a rollback
// target). If the agent cannot, or there is nothing recorded to go back
// to, the release stays live: promoted, with the report that condemned it
// and a note that raises an alert.
func (sp *safePush) rollback(ctx context.Context, report, what string, rolledBack error) error {
	s := sp.s
	meta, err := s.Store.GetDeploymentMeta(sp.d.ID)
	if err != nil {
		return err
	}
	if meta.RollbackTo == "" {
		s.logIfErr(s.move(&sp.d, state.DeployPromoted, state.DeploymentUpdate{GuardJSON: report, Note: noteRegressed}), "MoveDeployment")
		return fmt.Errorf("%s and there is no release recorded to roll back to", what)
	}
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RollbackRelease, rpc.ReleaseParams{Site: sp.prod, ReleaseID: meta.RollbackTo}, nil); err != nil {
		if cur, cerr := s.currentRelease(ctx, sp.prod); cerr != nil || cur == sp.d.ReleaseID {
			s.logIfErr(s.move(&sp.d, state.DeployPromoted, state.DeploymentUpdate{GuardJSON: report, Note: noteRegressed}), "MoveDeployment")
			return fmt.Errorf("%s and automatic rollback failed: %w", what, err)
		}
		// It went back after all: recorded below, the records then follow
		// what is live.
		defer s.reconcileLive(ctx, sp.prod)
	}
	s.logIfErr(s.move(&sp.d, state.DeployRolledBack, state.DeploymentUpdate{GuardJSON: report, Regressed: true}), "MoveDeployment")
	if _, _, err := s.Store.SyncLive(sp.prod.ID, meta.RollbackTo); err != nil {
		s.logIfErr(err, "SyncLive")
	}
	s.purgeQuietly(ctx, sp.prod)
	return rolledBack
}

// guardWorkers is how many PHP-FPM workers Performance Guard warms up for:
// the larger pool of the sites measured, sized as phpfpm sizes it. A site
// whose memory budget the agent picks (stored as 0) counts at the CPU cap,
// the most workers it can have: a longer warm-up costs seconds, a short
// one leaves cold workers in the measurement.
func guardWorkers(sites ...state.Site) int {
	n := 1
	for _, site := range sites {
		mem, workers := site.Config.Resources.MemoryHighMB, site.Config.Resources.PHPWorkers
		if mem <= 0 && workers <= 0 {
			workers = phpfpm.CPUCount * 3
		}
		n = max(n, phpfpm.SizeWorkers(mem, workers).Max)
	}
	return n
}
