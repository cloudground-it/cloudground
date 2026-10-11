package api

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Cron jobs are stored here and sent to the agent as typed jobs; the agent
// validates them again, renders the crontab and installs it
// (internal/agent/ops/cron). Nothing in this file writes shell text.

// cronLocks serialises, per site, reading the site's jobs and handing them
// to the agent: two changes racing could otherwise install the older list
// last.
var cronLocks sync.Map // site id -> *sync.Mutex

func lockCron(siteID int64) func() {
	m, _ := cronLocks.LoadOrStore(siteID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// checkJob is cronjob's validation of a job about to be stored. The id is
// not known before the insert, so the longest one stands in for it: a job
// that fits with it fits with any.
func checkJob(schedule, command string) error {
	return cronjob.Job{ID: math.MaxInt64, Schedule: schedule, Command: command}.Validate()
}

// applyCron installs the site's crontab from its stored jobs.
func (s *Server) applyCron(ctx context.Context, site state.Site) error {
	defer lockCron(site.ID)()
	stored, err := s.Store.ListCronJobs(site.ID)
	if err != nil {
		return err
	}
	jobs := make([]cronjob.Job, 0, len(stored))
	for _, j := range stored {
		jobs = append(jobs, cronjob.Job{ID: j.ID, Schedule: j.Schedule, Command: j.Command, Enabled: j.Enabled})
	}
	return agentrpc.Into(ctx, s.Agent, agentrpc.ApplyCron, rpc.CronParams{Site: site, Jobs: jobs}, nil)
}

// restoreCron re-installs the site's crontab after a refused change was
// undone in the store.
func (s *Server) restoreCron(ctx context.Context, site state.Site) {
	if err := s.applyCron(context.WithoutCancel(ctx), site); err != nil && s.Log != nil {
		s.Log.Warn("restoring the site's crontab failed", "site", site.Domain, "err", err)
	}
}

// hasCron reports whether a site needs a crontab at all: WordPress's own
// events, or a job of the operator's.
func (s *Server) hasCron(site state.Site) bool {
	if site.Type == state.SiteWordPress || site.Type == state.SiteWooCommerce {
		return true
	}
	jobs, err := s.Store.ListCronJobs(site.ID)
	return err != nil || len(jobs) > 0
}

func (s *Server) handleListCron(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	jobs, err := s.Store.ListCronJobs(site.ID)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not read the cron jobs")
		return
	}
	if jobs == nil {
		jobs = []state.CronJob{}
	}
	s.withScheduledRuns(r.Context(), site, jobs)
	// A command line can hold a token or a password. An account that cannot
	// manage the site sees it as the audit log shows it to everyone.
	if ok, _ := policyManage.admits(c.User.Role, ""); !ok {
		for i := range jobs {
			jobs[i].Command = redactForAudit(jobs[i].Command)
		}
	}
	respond(w, http.StatusOK, jobs)
}

// withScheduledRuns sets each job's last run to its last scheduled one
// when that is later than the last run on demand the store holds: the
// runner records scheduled runs on the agent's side (CronRuns), where the
// panel did not look, so a job that only ever ran on schedule showed no
// last run. An agent that does not answer leaves the store's.
func (s *Server) withScheduledRuns(ctx context.Context, site state.Site, jobs []state.CronJob) {
	if len(jobs) == 0 {
		return
	}
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.CronRuns, rpc.CronRunsParams{Site: site})
	if err != nil {
		if s.Log != nil {
			s.Log.Debug("could not read the scheduled cron runs", "site", site.Domain, "err", err)
		}
		return
	}
	for i := range jobs {
		run, ok := res.Runs[strconv.FormatInt(jobs[i].ID, 10)]
		if !ok || (jobs[i].LastRun != nil && !run.At.After(*jobs[i].LastRun)) {
			continue
		}
		at := run.At
		jobs[i].LastRun, jobs[i].LastStatus = &at, run.Status
	}
}

type cronJobRequest struct {
	Schedule    *string `json:"schedule"`
	Command     *string `json:"command"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
}

// merge applies the fields the request carries to j.
func (q cronJobRequest) merge(j *state.CronJob) {
	if q.Schedule != nil {
		j.Schedule = *q.Schedule
	}
	if q.Command != nil {
		j.Command = *q.Command
	}
	if q.Description != nil {
		j.Description = *q.Description
	}
	if q.Enabled != nil {
		j.Enabled = *q.Enabled
	}
}

const maxDescription = 200

func (s *Server) handleCreateCron(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	var req cronJobRequest
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	job := state.CronJob{SiteID: site.ID, Enabled: true}
	req.merge(&job)
	if err := checkJob(job.Schedule, job.Command); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(job.Description) > maxDescription {
		respondErr(w, http.StatusBadRequest, "the description is too long")
		return
	}
	job, err := s.Store.CreateCronJob(job)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not save the cron job")
		return
	}
	if err := s.applyCron(r.Context(), site); err != nil {
		// What the agent did not install is not kept: the panel would show
		// a job that never runs.
		s.logIfErr(s.Store.DeleteCronJob(job.ID), "DeleteCronJob")
		s.restoreCron(r.Context(), site)
		respondAgentErr(w, http.StatusBadGateway, "", err)
		return
	}
	s.audit(c.Actor(), "cron.create", site.Domain, job.Schedule+" "+redactForAudit(job.Command))
	respond(w, http.StatusCreated, job)
}

// handleUpdateCron changes a job's schedule, command, description or
// enabled flag. A disabled job stays listed but leaves the crontab.
func (s *Server) handleUpdateCron(w http.ResponseWriter, r *http.Request, c *call) {
	old, site := c.Cron, c.Site
	var req cronJobRequest
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	job := old
	req.merge(&job)
	if err := checkJob(job.Schedule, job.Command); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(job.Description) > maxDescription {
		respondErr(w, http.StatusBadRequest, "the description is too long")
		return
	}
	if err := s.Store.UpdateCronJob(job); err != nil {
		respondErr(w, http.StatusInternalServerError, "could not save the cron job")
		return
	}
	if err := s.applyCron(r.Context(), site); err != nil {
		s.logIfErr(s.Store.UpdateCronJob(old), "UpdateCronJob")
		s.restoreCron(r.Context(), site)
		respondAgentErr(w, http.StatusBadGateway, "", err)
		return
	}
	s.audit(c.Actor(), "cron.update", site.Domain, job.Schedule+" "+redactForAudit(job.Command)+cronState(job.Enabled))
	respond(w, http.StatusOK, job)
}

func cronState(enabled bool) string {
	if enabled {
		return " (enabled)"
	}
	return " (disabled)"
}

func (s *Server) handleDeleteCron(w http.ResponseWriter, r *http.Request, c *call) {
	job, site := c.Cron, c.Site
	if err := s.Store.DeleteCronJob(job.ID); err != nil {
		respondErr(w, http.StatusInternalServerError, "could not delete the cron job")
		return
	}
	s.audit(c.Actor(), "cron.delete", site.Domain, job.Schedule+" "+redactForAudit(job.Command))
	if err := s.applyCron(r.Context(), site); err != nil {
		respondAgentErr(w, http.StatusBadGateway, "The job was deleted but the crontab was not updated", err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunCron(w http.ResponseWriter, r *http.Request, c *call) {
	job, site := c.Cron, c.Site
	var res rpc.RunCronResult
	err := agentrpc.Into(r.Context(), s.Agent, agentrpc.RunCron, rpc.RunCronParams{
		Site: site, Job: cronjob.Job{ID: job.ID, Schedule: job.Schedule, Command: job.Command, Enabled: job.Enabled},
	}, &res)
	if err != nil {
		respondAgentErr(w, http.StatusBadGateway, "", err)
		return
	}
	s.logIfErr(s.Store.UpdateCronRun(job.ID, res.Status), "UpdateCronRun")
	s.audit(c.Actor(), "cron.run", site.Domain, job.Description)
	respond(w, http.StatusOK, res)
}
