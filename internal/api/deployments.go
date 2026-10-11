package api

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/guard"
	"github.com/cloudground-it/cloudground/internal/jobs"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The deployment routes (SPEC.md §4). Each handler checks the request,
// records the deployment in its first status and queues the job that
// drives it (deployflow.go); nothing here talks to the agent.

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request, c *call) {
	deps, err := s.Store.ListDeployments(c.Site.ID, 50)
	if err != nil {
		internalError(w, "ListDeployments", err, "")
		return
	}
	metas, err := s.Store.DeploymentMetas(c.Site.ID)
	if err != nil {
		internalError(w, "DeploymentMetas", err, "")
		return
	}
	// Each deployment with what the machine keeps beside it: where a
	// rollback from it goes, whether a safe push condemned it, its note.
	type view struct {
		state.Deployment
		state.DeploymentMeta
	}
	out := make([]view, len(deps))
	for i, d := range deps {
		out[i] = view{d, metas[d.ID]}
	}
	respond(w, http.StatusOK, out)
}

// handleDeploy builds a release from a directory of the site's own tree:
// an upload, a checkout made in the site's shell, the live release.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		SourceDir string `json:"source_dir"`
	}
	if err := decode(r, &req); err != nil || req.SourceDir == "" {
		respondErr(w, http.StatusBadRequest, "source_dir required")
		return
	}
	if msg := sourceDirProblem(c.Site, req.SourceDir); msg != "" {
		respondErr(w, http.StatusBadRequest, msg)
		return
	}
	dep, err := s.Store.CreateDeployment(state.Deployment{
		SiteID: c.Site.ID, ReleaseID: siteval.NewReleaseID(time.Now()), Status: state.DeployPending,
	})
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not record the deployment")
		return
	}
	s.audit(c.Actor(), "deploy.create", c.Site.Domain, dep.ReleaseID)
	task, err := s.enqueueDeploy("deploy.create", dep, deployPayload{DeploymentID: dep.ID, SourceDir: req.SourceDir}, siteLocks(c.Site.ID))
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task, "release_id": dep.ReleaseID, "deployment_id": dep.ID})
}

// recordInitialRelease gives provisioning's release (site.InitialRelease)
// a deployment record when it has none, in status: promoted for a site just
// created, superseded for one whose rollback or reconcile is about to make
// it live (RecordRollback and SyncLive then promote it). Without a record
// the Releases list never showed it, and after a rollback to it no row was
// live. A failure is only logged: the release still serves.
func (s *Server) recordInitialRelease(siteID int64, status state.DeploymentStatus) {
	_, err := s.Store.RecordInitialRelease(siteID, status)
	s.logIfErr(err, "RecordInitialRelease")
}

// sourceDirProblem is why dir cannot be deployed to site ("" when it can).
// The agent checks again against the resolved path; this gives the clear
// error before anything is queued.
func sourceDirProblem(site state.Site, dir string) string {
	if !filepath.IsAbs(dir) {
		return "source_dir must be absolute"
	}
	rel, err := filepath.Rel(site.RootPath, filepath.Clean(dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "source_dir must be inside the site's directory " + site.RootPath
	}
	return ""
}

// enqueueDeploy queues the job that drives a recorded deployment. If it
// cannot be queued the deployment is failed: no job will ever move it.
func (s *Server) enqueueDeploy(kind string, dep state.Deployment, p deployPayload, locks []string) (jobs.Task, error) {
	task, err := s.enqueue(jobs.Spec{Kind: kind, SiteID: dep.SiteID, Locks: locks, Payload: p})
	if err != nil && dep.Status == state.DeployPending {
		_, moveErr := s.Store.MoveDeployment(dep.ID, nil, state.DeployFailed, state.DeploymentUpdate{})
		s.logIfErr(moveErr, "MoveDeployment")
	}
	return task, err
}

// handlePromote makes a built release live.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request, c *call) {
	dep := c.Deployment
	if !state.DeploymentPromotable(dep.Status) {
		respondErr(w, http.StatusConflict, promoteRefusal(dep))
		return
	}
	s.audit(c.Actor(), "deploy.promote", c.Site.Domain, dep.ReleaseID)
	task, err := s.enqueueDeploy("deploy.promote", dep, deployPayload{DeploymentID: dep.ID, From: dep.Status}, siteLocks(c.Site.ID))
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

func promoteRefusal(dep state.Deployment) string {
	switch dep.Status {
	case state.DeployBlocked, state.DeployFailed, state.DeployCanceled:
		return "release " + dep.ReleaseID + " is " + string(dep.Status) + ": it was never built, so it cannot be promoted"
	}
	return "release " + dep.ReleaseID + " is " + string(dep.Status) + ": wait for it to finish"
}

// handleRollback goes back to a release that was live before: the one
// named (`deployment_id`), or by default the one the live release
// replaced (SPEC.md §4.3). The target is chosen here, never by the agent.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		DeploymentID int64 `json:"deployment_id,omitempty"`
	}
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	var target string
	if req.DeploymentID != 0 {
		d, err := s.Store.GetDeployment(req.DeploymentID)
		if err != nil || d.SiteID != c.Site.ID {
			respondErr(w, http.StatusNotFound, "deployment not found")
			return
		}
		if d.Status == state.DeployPromoted {
			respondErr(w, http.StatusConflict, "release "+d.ReleaseID+" is the live one")
			return
		}
		target = d.ReleaseID
	} else {
		t, err := s.Store.RollbackTarget(c.Site.ID)
		if errors.Is(err, state.ErrNotFound) {
			respondErr(w, http.StatusConflict, "there is no earlier live release to roll back to")
			return
		}
		if err != nil {
			respondErr(w, http.StatusInternalServerError, "could not read the site's deployments")
			return
		}
		target = t
	}
	if err := s.Store.CheckRollbackTarget(c.Site.ID, target); err != nil {
		respondErr(w, http.StatusConflict, "release "+target+" was never live (or was rolled back for a regression): it cannot be rolled back to")
		return
	}
	s.audit(c.Actor(), "deploy.rollback", c.Site.Domain, target)
	task, err := s.enqueue(jobs.Spec{Kind: "deploy.rollback", SiteID: c.Site.ID, Locks: siteLocks(c.Site.ID),
		Payload: rollbackPayload{SiteID: c.Site.ID, Target: target}})
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// handleSafePush measures staging against production and promotes
// staging's code only if it holds up (SPEC.md §4.4).
func (s *Server) handleSafePush(w http.ResponseWriter, r *http.Request, c *call) {
	prod := c.Site
	stg, err := s.stagingSvc().Of(prod)
	if errors.Is(err, state.ErrNotFound) {
		replyFailure(w, errNoStaging)
		return
	}
	if err != nil {
		internalError(w, "staging of", err, "")
		return
	}
	var req struct {
		Only  []string `json:"only,omitempty"`
		Paths []string `json:"paths"`
		Force bool     `json:"force"`
	}
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if len(req.Paths) == 0 {
		req.Paths = guard.DefaultProbePaths
	}
	// The probe requests these paths from the server itself: each must be
	// a plain path of the site, and the list bounded.
	if err := guard.ValidateProbePaths(req.Paths); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Store.PanelSettings(); err != nil {
		internalError(w, "PanelSettings", err, "")
		return
	}
	dep, err := s.Store.CreateDeployment(state.Deployment{
		SiteID: prod.ID, ReleaseID: siteval.NewReleaseID(time.Now()), Status: state.DeployPending,
	})
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not record the deployment")
		return
	}
	s.audit(c.Actor(), "deploy.safe-push", prod.Domain, "")
	task, err := s.enqueueDeploy("deploy.safe-push", dep, deployPayload{
		DeploymentID: dep.ID, StagingID: stg.ID, Only: req.Only, Paths: req.Paths, Force: req.Force,
	}, siteLocks(prod.ID, stg.ID))
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task, "release_id": dep.ReleaseID, "deployment_id": dep.ID})
}
