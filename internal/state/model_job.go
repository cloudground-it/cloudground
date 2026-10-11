package state

import "time"

// JobStatus is a queue job's place in its life (internal/jobs/SPEC.md §1).
type JobStatus string

// The job statuses. The last three are terminal.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

// Terminal reports whether a job in this status is finished for good.
func (st JobStatus) Terminal() bool {
	switch st {
	case JobSucceeded, JobFailed, JobCanceled:
		return true
	}
	return false
}

// Job is a row of the job queue. internal/jobs decides how it moves; the
// store only keeps it.
type Job struct {
	ID       int64
	Kind     string
	SiteID   int64
	Status   JobStatus
	Progress int
	Message  string
	// The log, bounded (internal/jobs/SPEC.md §5): the first lines, the
	// last lines and the count of bytes dropped in between.
	LogHead    string
	LogTail    string
	LogDropped int64
	Error      string
	// ErrorKey and ErrorArgs, when the failure carried a key, name Error
	// in the API's catalogue (internal/i18n), so each reader gets it in
	// their own language.
	ErrorKey  string
	ErrorArgs []string
	// Payload is the JSON the kind's handler works from, "{}" for none.
	Payload string
	Locks   []string
	// DedupKey, when not empty, lets one queued or running job hold it.
	DedupKey string
	Quiet    bool
	// Background marks the scheduler's jobs, which let a person's go first
	// in a lock's queue (internal/jobs/SPEC.md §3).
	Background      bool
	Attempts        int
	CancelRequested bool
	// NotBefore keeps a queued job waiting until then.
	NotBefore  *time.Time
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}
