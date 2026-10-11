package jobs

import (
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Task is a job as the API's /api/tasks and the task.* events show it
// (SPEC.md §7): the fields the UI has always read, with the status in the
// task vocabulary, plus the job's own status.
type Task struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	SiteID   int64  `json:"site_id,omitempty"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Log      string `json:"log"`
	Error    string `json:"error,omitempty"`
	// ErrorKey and ErrorArgs name Error in the API's catalogue: the API
	// writes Error in each reader's language from them.
	ErrorKey        string          `json:"error_key,omitempty"`
	ErrorArgs       []string        `json:"error_args,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
	JobStatus       state.JobStatus `json:"job_status"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
}

// TaskStatus maps a job status to the task vocabulary the UI renders.
func TaskStatus(st state.JobStatus) string {
	switch st {
	case state.JobQueued:
		return "pending"
	case state.JobCanceled:
		return "failed"
	default:
		return string(st)
	}
}

// View is the task view of a job.
func View(j state.Job) Task {
	return Task{
		ID: j.ID, Kind: j.Kind, SiteID: j.SiteID, Status: TaskStatus(j.Status),
		Progress: j.Progress, Message: j.Message, Log: composeLog(j.LogHead, j.LogTail, j.LogDropped),
		Error: j.Error, ErrorKey: j.ErrorKey, ErrorArgs: j.ErrorArgs, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		JobStatus: j.Status, CancelRequested: j.CancelRequested,
	}
}

func (q *Queue) publishCreated(j state.Job) {
	if j.Quiet {
		return
	}
	v := View(j)
	v.Log = ""
	q.events.Publish(events.TaskCreated, j.SiteID, v)
}

func (q *Queue) publishProgress(j state.Job, pct int, msg string) {
	q.events.Publish(events.TaskProgress, j.SiteID, map[string]any{
		"id": j.ID, "kind": j.Kind, "status": TaskStatus(state.JobRunning), "progress": pct, "message": msg,
	})
}

func (q *Queue) publishFinished(j state.Job) {
	if j.Quiet {
		return
	}
	data := map[string]any{"id": j.ID, "kind": j.Kind, "status": TaskStatus(j.Status), "job_status": j.Status, "progress": j.Progress}
	if j.Error != "" {
		data["error"] = j.Error
	}
	q.events.Publish(events.TaskFinished, j.SiteID, data)
}
