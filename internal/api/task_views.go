package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/i18n"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Tasks: the queue's jobs as the interface shows them (SPEC.md §9,
// internal/jobs/SPEC.md §7).

// canSeeJob reports whether the caller may see j: a maintenance job is an
// admin's alone, an operator sees its own sites' jobs, admins and read-only
// accounts see the rest.
func canSeeJob(c *call, j state.Job) bool {
	switch role := c.User.Role; {
	case role == "":
		return false
	case j.Quiet:
		return role == roleAdmin
	case role == roleOperator:
		return j.SiteID != 0 && c.CanAccess(j.SiteID)
	}
	return true
}

// taskView is j as a reader sees it: its error, when it has a translation
// key, in the reader's language (localize.go).
func (s *Server) taskView(j state.Job, lang string) jobs.Task {
	t := jobs.View(j)
	localizeTask(&t, lang)
	return t
}

// visibleJob reads the job the path names, if the caller may see it.
func (s *Server) visibleJob(r *http.Request, c *call) (state.Job, error) {
	id, err := pathID(r)
	if err != nil {
		return state.Job{}, decline(http.StatusBadRequest, "%s", err.Error())
	}
	j, err := s.Store.GetJob(id)
	switch {
	case errors.Is(err, state.ErrNotFound), err == nil && !canSeeJob(c, j):
		return state.Job{}, decline(http.StatusNotFound, "task not found")
	case err != nil:
		s.logIfErr(err, "GetJob")
		return state.Job{}, decline(http.StatusInternalServerError, "could not read the task")
	}
	return j, nil
}

// cancelJob cancels a queued job at once or asks a running one to stop
// (internal/jobs/SPEC.md §6). A job of no site is an admin's to cancel.
func (s *Server) cancelJob(c *call, j state.Job) (state.Job, error) {
	if j.SiteID == 0 && c.User.Role != roleAdmin {
		return state.Job{}, decline(http.StatusForbidden, "only an administrator can cancel a server-wide task")
	}
	got, err := s.Jobs.Cancel(j.ID)
	switch {
	case errors.Is(err, jobs.ErrFinished):
		return state.Job{}, decline(http.StatusConflict, "the task has already finished")
	case errors.Is(err, jobs.ErrNotCancelable):
		return state.Job{}, decline(http.StatusConflict, "%s", err.Error())
	case err != nil:
		s.logIfErr(err, "Cancel")
		return state.Job{}, decline(http.StatusInternalServerError, "could not cancel the task")
	}
	return got, nil
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.Store.ListJobs(100, false)
	if err != nil {
		s.logIfErr(err, "ListJobs")
		respondErr(w, http.StatusInternalServerError, "could not list the tasks")
		return
	}
	lang := readerLanguage(r, c)
	tasks := make([]jobs.Task, 0, len(list))
	for _, j := range list {
		if canSeeJob(c, j) {
			tasks = append(tasks, s.taskView(j, lang))
		}
	}
	respond(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request, c *call) {
	j, err := s.visibleJob(r, c)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, s.taskView(j, readerLanguage(r, c)))
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request, c *call) {
	j, err := s.visibleJob(r, c)
	if err == nil {
		j, err = s.cancelJob(c, j)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(c.Actor(), "task.cancel", fmt.Sprintf("%s #%d", j.Kind, j.ID), string(j.Status))
	respond(w, http.StatusOK, s.taskView(j, readerLanguage(r, c)))
}

// handleTaskNames is every task kind in words in the reader's language: the
// names the task list, the alerts and the webhook use, for the interface to
// name the kinds of live events, which carry no language.
func (s *Server) handleTaskNames(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, i18n.TaskNames(readerLanguage(r, c)))
}
