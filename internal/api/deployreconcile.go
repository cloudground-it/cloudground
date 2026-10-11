package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/guard"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The records follow the server (SPEC.md §4.6): what a site serves is
// what the agent's CurrentRelease reads from its current link. After any
// doubt (an agent call that failed after it may have swapped current, a
// job a restart cut short) the records are aligned with it, and a
// difference is a deploy.drift alert.

// currentRelease asks the agent which release site serves ("" for none).
func (s *Server) currentRelease(ctx context.Context, site state.Site) (string, error) {
	var res rpc.CurrentReleaseResult
	err := agentrpc.Into(ctx, s.Agent, agentrpc.CurrentRelease, rpc.CurrentReleaseParams{Site: site}, &res)
	return res.ReleaseID, err
}

// reconcileKey flags a site whose records must be checked against the
// server at the next start (a hook cannot ask the agent).
func reconcileKey(siteID int64) string {
	return "deploy.reconcile.site." + strconv.FormatInt(siteID, 10)
}

func (s *Server) flagReconcile(siteID int64) {
	if siteID != 0 {
		s.logIfErr(s.Store.MarkScheduleRun(reconcileKey(siteID)), "MarkScheduleRun")
	}
}

// reconcileLive aligns the site's records with what it serves, and notes
// a difference on the release that is live (or, when that has no record,
// on the one the records had said was live).
func (s *Server) reconcileLive(ctx context.Context, site state.Site) {
	cur, err := s.currentRelease(ctx, site)
	if err != nil {
		s.Log.Warn("could not read the live release; the site is reconciled at the next start", "site", site.Domain, "err", err)
		s.flagReconcile(site.ID)
		return
	}
	s.syncLive(site, cur)
}

func (s *Server) syncLive(site state.Site, cur string) {
	if cur == siteval.InitialRelease {
		s.recordInitialRelease(site.ID, state.DeploySuperseded)
	}
	changed, recorded, err := s.Store.SyncLive(site.ID, cur)
	if err != nil || !changed {
		s.logIfErr(err, "SyncLive")
		return
	}
	s.Log.Warn("deploy.drift: the site serves another release than the records said", "site", site.Domain,
		"live", cur, "recorded", recorded)
	note := noteDrift + ":" + recorded
	deps, _ := s.Store.ListDeployments(site.ID, 200)
	for _, d := range deps {
		if d.ReleaseID == cur {
			s.logIfErr(s.Store.SetDeploymentNote(d.ID, note), "SetDeploymentNote")
			return
		}
	}
	for _, d := range deps {
		if d.ReleaseID == recorded {
			s.logIfErr(s.Store.SetDeploymentNote(d.ID, noteDrift+":"+cur), "SetDeploymentNote")
			return
		}
	}
}

// queueDeployReconciles queues, after a start's recovery, the reconcile
// of every site flagged or with a deployment left promoting or verifying.
func (s *Server) queueDeployReconciles() {
	want := map[int64]bool{}
	if inflight, err := s.Store.InFlightDeployments(); err == nil {
		for _, d := range inflight {
			want[d.SiteID] = true
		}
	}
	if sites, err := s.Store.ListSites(); err == nil {
		for _, site := range sites {
			if t, err := s.Store.LastScheduleRun(reconcileKey(site.ID)); err == nil && !t.IsZero() {
				want[site.ID] = true
			}
		}
	}
	for id := range want {
		if _, err := s.Jobs.Enqueue(jobs.Spec{Kind: "deploy.reconcile", SiteID: id, Locks: siteLocks(id), Background: true,
			DedupKey: "deploy.reconcile:" + strconv.FormatInt(id, 10), Payload: sitePayload{SiteID: id}}); err != nil {
			s.Log.Warn("could not queue a deployment reconcile", "site", id, "err", err)
		}
	}
}

// runDeployReconcile is deploy.reconcile: a deployment left promoting or
// verifying is settled from what the site serves (a safe push whose
// release went live unverified is verified now, and rolled back if that
// cannot pass); releases a cut build left behind are removed; the records
// then follow the server.
func (s *Server) runDeployReconcile(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(r.Job().SiteID, r.Job().Kind)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	cur, err := s.currentRelease(ctx, site)
	if err != nil {
		return fmt.Errorf("could not read the live release: %w", err)
	}
	inflight, err := s.Store.InFlightDeployments()
	if err != nil {
		return err
	}
	var failures []string
	for _, d := range inflight {
		if d.SiteID != site.ID {
			continue
		}
		if err := s.settleInFlight(ctx, site, d, cur, r.Progress); err != nil {
			failures = append(failures, d.ReleaseID+": "+err.Error())
		}
		if c, err := s.currentRelease(ctx, site); err == nil {
			cur = c // a verification may have rolled back
		}
	}
	s.removeOrphans(ctx, site, cur, r.Progress)
	s.syncLive(site, cur)
	s.logIfErr(s.Store.ClearScheduleRun(reconcileKey(site.ID)), "ClearScheduleRun")
	r.Progress(100, "The records follow the live release "+cur)
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// settleInFlight settles one deployment a job left promoting or verifying.
func (s *Server) settleInFlight(ctx context.Context, site state.Site, d state.Deployment, cur string, progress func(int, string)) error {
	var report guard.Report
	safePushed := json.Unmarshal([]byte(d.GuardJSON), &report) == nil && len(report.Baseline) > 0
	if d.ReleaseID != cur {
		// It never went live (or went back): built, not live.
		to := state.DeployCreated
		if d.PromotedAt != nil {
			to = state.DeploySuperseded
		}
		_, err := s.Store.MoveDeployment(d.ID, []state.DeploymentStatus{d.Status}, to, state.DeploymentUpdate{})
		return err
	}
	if !safePushed {
		_, err := s.Store.MoveDeployment(d.ID, []state.DeploymentStatus{d.Status}, state.DeployPromoted, state.DeploymentUpdate{})
		return err
	}
	// A safe push's release is live and was never verified: verify it now.
	if d.Status == state.DeployPromoting {
		moved, err := s.Store.MoveDeployment(d.ID, []state.DeploymentStatus{d.Status}, state.DeployVerifying, state.DeploymentUpdate{})
		if err != nil {
			return err
		}
		d = moved
	}
	progress(-1, "Release "+d.ReleaseID+" went live without its verification: verifying it now")
	sp, err := s.newSafePush(site, guardWorkers(site), d, deployPayload{DeploymentID: d.ID}, progress)
	if err != nil {
		// Without what the verification needs it cannot run: roll back.
		sp = &safePush{s: s, prod: site, d: d, progress: progress}
		return sp.rollback(ctx, "", "the verification could not run", fmt.Errorf("verification could not run (%v); rolled back", err))
	}
	return sp.verify(ctx)
}

// removeOrphans removes the releases of failed builds the site still has
// (a build a restart cut short may have published its directory).
func (s *Server) removeOrphans(ctx context.Context, site state.Site, cur string, progress func(int, string)) {
	deps, err := s.Store.ListDeployments(site.ID, 50)
	if err != nil {
		return
	}
	var ids []string
	for _, d := range deps {
		if d.Status == state.DeployFailed && d.ReleaseID != cur {
			ids = append(ids, d.ReleaseID)
		}
	}
	if len(ids) == 0 {
		return
	}
	var res rpc.RemoveReleasesResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RemoveReleases, rpc.RemoveReleasesParams{Site: site, ReleaseIDs: ids}, &res); err != nil {
		s.Log.Warn("could not remove the releases of failed builds", "site", site.Domain, "err", err)
		return
	}
	if len(res.Removed) > 0 {
		progress(-1, "Removed releases of failed builds: "+strings.Join(res.Removed, ", "))
	}
}

// deployConditions is one alert per deployment with a note (SPEC.md §11.1):
// keyed by the deployment, the note is its fingerprint, so a new note on
// the same deployment is a new occurrence.
func deployConditions(noted []state.NotedDeployment) []state.Alert {
	var out []state.Alert
	for _, n := range noted {
		code, arg, _ := strings.Cut(n.Note, ":")
		release := n.ReleaseID
		a := state.Alert{Key: "deploy:" + strconv.FormatInt(n.ID, 10), SiteID: n.SiteID, Level: "warning", Fingerprint: n.Note}
		switch code {
		case noteDrift:
			if arg == "" {
				arg = "none"
			}
			a.Message = fmt.Sprintf("deploy.drift: the site serves a release the panel had not recorded as live (release %s, recorded %s)", release, arg)
			a.Text = state.AlertText{Key: "alert.deployDrift", Args: []string{release, arg}}
		case noteUnverified:
			a.Message = "release " + release + " is live without its post-promotion verification (it was canceled)"
			a.Text = state.AlertText{Key: "alert.deployUnverified", Args: []string{release}}
		case noteRegressed:
			a.Message = "release " + release + " regressed after its promotion and could not be rolled back automatically"
			a.Text = state.AlertText{Key: "alert.deployRegressed", Args: []string{release}}
		default:
			continue
		}
		out = append(out, a)
	}
	return out
}
