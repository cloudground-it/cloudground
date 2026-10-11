// Package jobs is the panel's persistent job queue and scheduler: jobs
// stored in the state database, run under per-site and per-repository
// locks within a global limit, cancellable, drained on shutdown and settled
// deterministically after a restart. See SPEC.md.
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

var (
	// ErrDraining is returned by Enqueue once the queue has begun to drain.
	ErrDraining = errors.New("the panel is shutting down and takes no new jobs")
	// ErrFinished is returned by Cancel for a job that has already ended.
	ErrFinished = errors.New("the job has already finished")
	// ErrUnknownKind is returned by Enqueue for a kind never registered.
	ErrUnknownKind = errors.New("unknown job kind")
	// ErrNotCancelable is returned by Cancel for a kind that must run to
	// its end (a restore test); errors.Is it, the message says why.
	ErrNotCancelable = errors.New("this job cannot be canceled")
	// ErrRequeue, returned by a job's work, puts the job back in the queue
	// to start again (its locks resolved again): the job found, at its
	// start, that the locks it holds are not the ones it needs.
	ErrRequeue = errors.New("the job's locks changed; queued again")
)

// MaxAttempts is how many times a job interrupted by restarts is started
// before recovery gives up on it (SPEC.md §6).
const MaxAttempts = 3

// notCancelable is the refusal for a kind with Kind.NoCancel.
type notCancelable struct{ why string }

func (e notCancelable) Error() string        { return e.why }
func (e notCancelable) Is(target error) bool { return target == ErrNotCancelable }

// Func is a job's work. It returns nil when the job succeeded.
type Func func(ctx context.Context, r *Run) error

// Kind describes one kind of job (SPEC.md §2).
type Kind struct {
	Name string
	// Quiet jobs are maintenance: not listed as tasks, no task events.
	Quiet bool
	// Run, when set, runs a job of this kind from its payload, which makes
	// the kind resumable across a restart.
	Run Func
	// Rerun: a job interrupted while running may run again from the start.
	Rerun bool
	// Canceled is called after a job of this kind was canceled before it
	// started.
	Canceled func(state.Job)
	// Interrupted is called after a job of this kind that had started was
	// failed by a shutdown or by recovery, or canceled before it could stop.
	Interrupted func(state.Job)
	// NoCancel, when set, refuses every cancel of this kind with this
	// message.
	NoCancel string
	// ResolveLocks, when set, gives a queued job's locks as they are now
	// (the configured backup repository may have changed since it was
	// queued); the dispatcher stores them before it decides.
	ResolveLocks func(state.Job) []string
	// RerunDelay holds back a job recovery puts back in the queue, so the
	// agent call the dead process left running can end first.
	RerunDelay time.Duration
}

func (k Kind) resumable() bool { return k.Run != nil && k.Rerun }

// Spec is a job to enqueue.
type Spec struct {
	Kind   string
	SiteID int64
	// Locks the job holds while it runs (SPEC.md §3).
	Locks    []string
	DedupKey string
	// Background marks the scheduler's work: a person's jobs go first in
	// the queue of every lock (SPEC.md §3).
	Background bool
	// Payload is stored as JSON; it must never carry a secret.
	Payload any
	// Func is the work for a kind without Run. It lives in this process
	// only.
	Func Func
	// Canceled is called if this job is canceled before it starts.
	Canceled func()
}

// Lock keys (SPEC.md §3.5).
const (
	PanelLock  = "panel"
	RenderLock = "maintenance:render"
)

// SiteLock is the key of anything that changes site id.
func SiteLock(id int64) string { return "site:" + strconv.FormatInt(id, 10) }

// RepoLock is the key of anything that runs restic against repository: a
// hash of the URL without its password (state.RepoKey), so a new password
// is the same repository.
func RepoLock(repository string) string {
	sum := sha256.Sum256([]byte(state.RepoKey(repository)))
	return "repo:" + hex.EncodeToString(sum[:8])
}

// Run is what a job's work sees of itself.
type Run struct {
	q   *Queue
	job state.Job

	mu       sync.Mutex
	log      boundedLog
	progress int
	message  string
}

func newRun(q *Queue, job state.Job) *Run {
	return &Run{q: q, job: job, progress: job.Progress, message: job.Message,
		log: boundedLog{head: job.LogHead, tail: job.LogTail, dropped: job.LogDropped}}
}

// Job is the job as it was when it started.
func (r *Run) Job() state.Job { return r.job }

// ShuttingDown reports whether a job's context ended because the panel is
// stopping rather than because a person canceled the job: the queue then
// reruns the job or settles it through its kind's Interrupted hook
// (SPEC.md §6), so the work must not record it as canceled.
func ShuttingDown(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errShutdown) }

// Payload decodes the job's payload into v.
func (r *Run) Payload(v any) error {
	p := r.job.Payload
	if p == "" {
		p = "{}"
	}
	return json.Unmarshal([]byte(p), v)
}

// Progress records progress (0–100) and a log line (SPEC.md §5). A
// negative pct keeps the progress as it is and only logs.
func (r *Run) Progress(pct int, msg string) {
	r.mu.Lock()
	if pct < 0 {
		pct = r.progress
	}
	pct = max(0, min(pct, 100))
	msg = capLine(msg)
	r.progress, r.message = pct, msg
	r.log.add(msg)
	head, tail, dropped := r.log.head, r.log.tail, r.log.dropped
	r.mu.Unlock()
	r.q.logIfErr(r.q.store.SaveJobProgress(r.job.ID, pct, msg, head, tail, dropped), "SaveJobProgress")
	if !r.job.Quiet {
		r.q.publishProgress(r.job, pct, msg)
	}
}

// state returns the progress and the log as they stand.
func (r *Run) snapshot() (progress int, log boundedLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.progress, r.log
}
