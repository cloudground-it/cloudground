package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/state"
)

// DefaultLimit is how many jobs a person started (and the scheduler's
// visible ones) run at once; DefaultQuietLimit is the maintenance jobs'
// own limit, outside it (SPEC.md §3.4).
const (
	DefaultLimit      = 4
	DefaultQuietLimit = 2
)

// Messages a job ends or is resumed with (SPEC.md §6).
const (
	msgCanceled        = "Canceled"
	msgCanceledRestart = "Canceled (the panel restarted before the job stopped)"
	msgShutdown        = "interrupted by panel shutdown"
	msgRestart         = "interrupted by panel restart"
	msgTooManyRestarts = "interrupted by panel restarts 3 times; not run again"
	msgNotStarted      = "not started: the panel restarted before it could run"
	msgUnknownKind     = "unknown job kind after a panel restart"
	lineResumeShutdown = "Interrupted by a panel shutdown; it runs again after the restart"
	lineResumeRestart  = "Interrupted by a panel restart; it runs again"
	lineRequeue        = "The locks this job needs changed; queued again"
)

// Causes of a job's context ending.
var (
	errCanceledByUser = errors.New("canceled")
	errShutdown       = errors.New("panel shutdown")
)

// Options configure a Queue.
type Options struct {
	// Limit is the concurrency limit of non-quiet jobs (DefaultLimit when
	// <= 0); QuietLimit that of maintenance jobs (DefaultQuietLimit).
	Limit, QuietLimit int
	// Events receives the task.* events (nil publishes nothing).
	Events *events.Bus
	Log    *slog.Logger
	// Recovered runs once after recovery, before the first dispatch.
	Recovered func()
	// ShutdownGrace is how long Drain waits after cancelling the jobs
	// still running at its deadline (5 s when 0).
	ShutdownGrace time.Duration
	// Poll is how often the dispatcher looks at the queue without being
	// woken (2 s when 0).
	Poll time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// Queue runs jobs. Register every kind, then Start.
type Queue struct {
	store  *state.Store
	events *events.Bus
	log    *slog.Logger
	opt    Options

	mu       sync.Mutex
	kinds    map[string]Kind
	funcs    map[int64]Func   // work of queued jobs whose kind has no Run
	onCancel map[int64]func() // Spec.Canceled of queued jobs
	running  map[int64]*handle
	held     map[string]int64 // lock key -> job holding it
	waiters  map[int64][]chan struct{}
	started  bool
	draining bool
	wg       sync.WaitGroup // running jobs

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

type handle struct {
	job    state.Job
	kind   Kind
	ctx    context.Context
	cancel context.CancelCauseFunc
	run    *Run
	// userCanceled is set by Cancel (under Queue.mu), whatever else ended
	// the context first.
	userCanceled bool
}

// New returns a queue over store.
func New(store *state.Store, opt Options) *Queue {
	if opt.Limit <= 0 {
		opt.Limit = DefaultLimit
	}
	if opt.QuietLimit <= 0 {
		opt.QuietLimit = DefaultQuietLimit
	}
	if opt.ShutdownGrace <= 0 {
		opt.ShutdownGrace = 5 * time.Second
	}
	if opt.Poll <= 0 {
		opt.Poll = 2 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Queue{
		store: store, events: opt.Events, log: opt.Log, opt: opt,
		kinds: map[string]Kind{}, funcs: map[int64]Func{}, onCancel: map[int64]func(){},
		running: map[int64]*handle{}, held: map[string]int64{}, waiters: map[int64][]chan struct{}{},
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

func (q *Queue) logIfErr(err error, what string) {
	if err != nil {
		q.log.Error("job queue: state write failed", "op", what, "err", err)
	}
}

// Register adds a kind. It panics on a duplicate or after Start: kinds are
// a fixed table set up at start-up.
func (q *Queue) Register(k Kind) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.started {
		panic("jobs: Register after Start")
	}
	if _, dup := q.kinds[k.Name]; dup || k.Name == "" {
		panic("jobs: kind registered twice or unnamed: " + k.Name)
	}
	q.kinds[k.Name] = k
}

// Kind returns a registered kind.
func (q *Queue) Kind(name string) (Kind, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	k, ok := q.kinds[name]
	return k, ok
}

// Start settles the jobs a previous process left (SPEC.md §6, recovery),
// runs the Recovered callback and starts dispatching.
func (q *Queue) Start() error {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return nil
	}
	q.started = true
	hooks, err := q.recoverLocked()
	q.mu.Unlock()
	for _, h := range hooks {
		q.safely("recovery hook", h)
	}
	if q.opt.Recovered != nil {
		q.safely("recovered callback", q.opt.Recovered)
	}
	go q.dispatcher()
	q.wake()
	return err
}

// safely runs a hook, turning a panic into a log line.
func (q *Queue) safely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("job queue: "+what+" panicked", "panic", r)
		}
	}()
	fn()
}

func logOf(j state.Job) boundedLog {
	return boundedLog{head: j.LogHead, tail: j.LogTail, dropped: j.LogDropped}
}

func (q *Queue) recoverLocked() (hooks []func(), err error) {
	active, err := q.store.ActiveJobs()
	if err != nil {
		return nil, fmt.Errorf("job recovery: %w", err)
	}
	for _, j := range active {
		kind, known := q.kinds[j.Kind]
		l := logOf(j)
		finish := func(status state.JobStatus, msg string) (state.Job, error) {
			return q.store.FinishJob(j.ID, status, msg, "", nil, j.Progress, l.head, l.tail, l.dropped)
		}
		var ended state.Job
		var ferr error
		var hook func(state.Job)
		switch {
		case !known:
			ended, ferr = finish(state.JobFailed, msgUnknownKind)
		case j.Status == state.JobRunning && j.CancelRequested:
			// A cancel was asked for: it is never lost, the job never
			// rerun. It had started, so what it left is the interrupted
			// kind's to settle.
			ended, ferr = finish(state.JobCanceled, msgCanceledRestart)
			hook = kind.Interrupted
		case j.Status == state.JobRunning && kind.resumable() && j.Attempts < MaxAttempts:
			l.add(lineResumeRestart)
			var notBefore *time.Time
			if kind.RerunDelay > 0 {
				t := q.opt.Now().Add(kind.RerunDelay)
				notBefore = &t
			}
			q.logIfErr(q.store.RequeueJob(j.ID, l.head, l.tail, l.dropped, notBefore), "RequeueJob")
			continue
		case j.Status == state.JobRunning && kind.resumable():
			ended, ferr = finish(state.JobFailed, msgTooManyRestarts)
			hook = kind.Interrupted
		case j.Status == state.JobRunning:
			ended, ferr = finish(state.JobFailed, msgRestart)
			hook = kind.Interrupted
		case kind.Run == nil && q.funcs[j.ID] != nil:
			continue // enqueued by this process before Start
		case kind.Run == nil:
			// Queued, and its work died with the process: it never ran, so
			// it is undone as a cancel before its start would be.
			ended, ferr = q.store.FinishQueuedJob(j.ID, state.JobFailed, msgNotStarted)
			hook = kind.Canceled
			if hook == nil {
				hook = kind.Interrupted
			}
		default:
			continue // queued and resumable: it runs
		}
		if ferr != nil {
			q.logIfErr(ferr, "FinishJob (recovery)")
			continue
		}
		if hook != nil {
			h, job := hook, ended
			hooks = append(hooks, func() { h(job) })
		}
	}
	return hooks, nil
}

// Enqueue stores a job and wakes the dispatcher. A job with a dedup key
// that a queued or running job already has is not created: that job is
// returned instead.
func (q *Queue) Enqueue(spec Spec) (state.Job, error) {
	payload := "{}"
	if spec.Payload != nil {
		b, err := json.Marshal(spec.Payload)
		if err != nil {
			return state.Job{}, fmt.Errorf("job payload: %w", err)
		}
		payload = string(b)
	}
	q.mu.Lock()
	if q.draining {
		q.mu.Unlock()
		return state.Job{}, ErrDraining
	}
	kind, ok := q.kinds[spec.Kind]
	if !ok {
		q.mu.Unlock()
		return state.Job{}, fmt.Errorf("%w: %s", ErrUnknownKind, spec.Kind)
	}
	if kind.Run == nil && spec.Func == nil {
		q.mu.Unlock()
		return state.Job{}, fmt.Errorf("jobs: kind %s needs its work at enqueue", spec.Kind)
	}
	job, existing, err := q.store.CreateJob(state.Job{
		Kind: spec.Kind, SiteID: spec.SiteID, Payload: payload, Locks: dedupLocks(spec.Locks),
		DedupKey: spec.DedupKey, Quiet: kind.Quiet, Background: spec.Background,
	})
	if err != nil {
		q.mu.Unlock()
		return state.Job{}, err
	}
	if !existing {
		if kind.Run == nil {
			q.funcs[job.ID] = spec.Func
		}
		if spec.Canceled != nil {
			q.onCancel[job.ID] = spec.Canceled
		}
	}
	q.mu.Unlock()
	if !existing {
		q.publishCreated(job)
		q.wake()
	}
	return job, nil
}

func dedupLocks(keys []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (q *Queue) wake() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

func (q *Queue) dispatcher() {
	defer close(q.done)
	t := time.NewTicker(q.opt.Poll)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-q.kick:
		case <-t.C:
		}
		q.dispatch()
	}
}

// dispatch starts every queued job that may start now (SPEC.md §3).
func (q *Queue) dispatch() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.draining {
		return
	}
	queued, err := q.store.QueuedJobs()
	if err != nil {
		q.log.Error("job queue: list queued jobs", "err", err)
		return
	}
	// A person's jobs first, then the scheduler's; oldest first in each.
	sort.SliceStable(queued, func(a, b int) bool { return !queued[a].Background && queued[b].Background })
	running, quiet := 0, 0
	for _, h := range q.running {
		if h.job.Quiet {
			quiet++
		} else {
			running++
		}
	}
	now := q.opt.Now()
	// reserved: keys an earlier job waits for, closed to every later job.
	// fromBackground: free keys of a waiting job of a person's, closed to
	// the scheduler's jobs only.
	reserved, fromBackground := map[string]bool{}, map[string]bool{}
	for _, j := range queued {
		if j.NotBefore != nil && now.Before(*j.NotBefore) {
			continue
		}
		kind := q.kinds[j.Kind]
		if kind.ResolveLocks != nil {
			if want := dedupLocks(kind.ResolveLocks(j)); !slices.Equal(want, j.Locks) {
				q.logIfErr(q.store.SetJobLocks(j.ID, want), "SetJobLocks")
				j.Locks = want
			}
		}
		blocked := false
		for _, k := range j.Locks {
			if _, held := q.held[k]; held || reserved[k] || (j.Background && fromBackground[k]) {
				blocked = true
			}
		}
		slot := running < q.opt.Limit
		if j.Quiet {
			slot = quiet < q.opt.QuietLimit
		}
		if blocked || !slot {
			for _, k := range j.Locks {
				if _, held := q.held[k]; held || j.Background {
					reserved[k] = true
				} else {
					fromBackground[k] = true
				}
			}
			continue
		}
		if q.startLocked(j) {
			if j.Quiet {
				quiet++
			} else {
				running++
			}
		}
	}
}

func (q *Queue) startLocked(j state.Job) bool {
	kind := q.kinds[j.Kind]
	fn := kind.Run
	if fn == nil {
		fn = q.funcs[j.ID]
	}
	if fn == nil {
		// Cannot happen after recovery; never leave it queued forever.
		if ended, err := q.store.FinishQueuedJob(j.ID, state.JobFailed, msgNotStarted); err == nil {
			q.notifyLocked(ended)
			go q.publishFinished(ended)
		}
		return false
	}
	started, err := q.store.StartJob(j.ID)
	if err != nil {
		q.logIfErr(err, "StartJob")
		return false
	}
	delete(q.funcs, j.ID)
	delete(q.onCancel, j.ID)
	ctx, cancel := context.WithCancelCause(context.Background())
	h := &handle{job: started, kind: kind, ctx: ctx, cancel: cancel, run: newRun(q, started)}
	q.running[j.ID] = h
	for _, k := range started.Locks {
		q.held[k] = j.ID
	}
	q.wg.Add(1)
	if !started.Quiet {
		q.publishProgress(started, started.Progress, "")
	}
	go q.execute(h, fn)
	return true
}

func (q *Queue) execute(h *handle, fn Func) {
	defer q.wg.Done()
	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("internal panic: %v", r)
				q.log.Error("job panicked", "job", h.job.ID, "kind", h.job.Kind, "panic", r)
			}
		}()
		runErr = fn(h.ctx, h.run)
	}()
	q.finish(h, runErr)
}

// finish records how a job ended and releases its locks.
func (q *Queue) finish(h *handle, runErr error) {
	q.mu.Lock()
	userCanceled := h.userCanceled
	q.mu.Unlock()
	cause := context.Cause(h.ctx)
	h.cancel(nil)
	progress, log := h.run.snapshot()
	var status state.JobStatus
	var msg, errKey string
	var errArgs []string
	requeue, interrupted := false, false
	var notBefore *time.Time
	switch {
	case runErr == nil:
		status, progress = state.JobSucceeded, 100
	case userCanceled:
		status, msg = state.JobCanceled, msgCanceled
		if !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, errCanceledByUser) {
			msg += ": " + runErr.Error()
		}
	case errors.Is(runErr, ErrRequeue):
		requeue = true
		log.add(lineRequeue)
	case errors.Is(cause, errShutdown) && h.kind.resumable() && h.job.Attempts < MaxAttempts:
		requeue = true
		log.add(lineResumeShutdown)
	case errors.Is(cause, errShutdown):
		status, msg, interrupted = state.JobFailed, msgShutdown, true
	default:
		status, msg = state.JobFailed, runErr.Error()
		errKey, errArgs = state.ErrorKeyOf(runErr)
	}

	q.mu.Lock()
	var ended state.Job
	var err error
	if requeue {
		err = q.store.RequeueJob(h.job.ID, log.head, log.tail, log.dropped, notBefore)
		q.logIfErr(err, "RequeueJob")
	} else {
		ended, err = q.store.FinishJob(h.job.ID, status, msg, errKey, errArgs, progress, log.head, log.tail, log.dropped)
		q.logIfErr(err, "FinishJob")
	}
	delete(q.running, h.job.ID)
	for _, k := range h.job.Locks {
		if q.held[k] == h.job.ID {
			delete(q.held, k)
		}
	}
	if err == nil && !requeue {
		q.notifyLocked(ended)
	}
	q.mu.Unlock()
	if err == nil && !requeue {
		if interrupted && h.kind.Interrupted != nil {
			q.safely("interrupted hook", func() { h.kind.Interrupted(ended) })
		}
		q.publishFinished(ended)
	}
	q.wake()
}

// notifyLocked releases the waiters of a job that has ended.
func (q *Queue) notifyLocked(j state.Job) {
	for _, ch := range q.waiters[j.ID] {
		close(ch)
	}
	delete(q.waiters, j.ID)
}

// Cancel cancels a queued job at once, or asks a running one to stop
// (SPEC.md §6). It answers with the job as it now is. A queued job's
// cancel hooks run before any other job can start, so nothing queued
// behind it renders what the hook is about to undo; a hook must therefore
// not call the queue.
func (q *Queue) Cancel(id int64) (state.Job, error) {
	q.mu.Lock()
	if h, ok := q.running[id]; ok {
		if h.kind.NoCancel != "" {
			q.mu.Unlock()
			j, _ := q.store.GetJob(id)
			return j, notCancelable{h.kind.NoCancel}
		}
		h.userCanceled = true
		q.logIfErr(q.store.SetJobCancelRequested(id), "SetJobCancelRequested")
		h.cancel(errCanceledByUser)
		q.mu.Unlock()
		return q.store.GetJob(id)
	}
	cur, err := q.store.GetJob(id)
	if err != nil {
		q.mu.Unlock()
		return state.Job{}, err
	}
	kind := q.kinds[cur.Kind]
	if cur.Status.Terminal() {
		q.mu.Unlock()
		return cur, ErrFinished
	}
	if kind.NoCancel != "" {
		q.mu.Unlock()
		return cur, notCancelable{kind.NoCancel}
	}
	ended, err := q.store.FinishQueuedJob(id, state.JobCanceled, msgCanceled)
	if err != nil {
		q.mu.Unlock()
		if errors.Is(err, state.ErrJobState) {
			j, _ := q.store.GetJob(id)
			if j.Status.Terminal() {
				return j, ErrFinished
			}
			return j, fmt.Errorf("job %d is %s and not run by this process", id, j.Status)
		}
		return state.Job{}, err
	}
	hook := q.onCancel[id]
	delete(q.funcs, id)
	delete(q.onCancel, id)
	if hook != nil {
		q.safely("cancel hook", hook)
	}
	if kind.Canceled != nil {
		q.safely("cancel hook", func() { kind.Canceled(ended) })
	}
	q.notifyLocked(ended)
	q.mu.Unlock()
	q.publishFinished(ended)
	q.wake()
	return ended, nil
}

// Wait returns the job once it has ended, or when ctx ends.
func (q *Queue) Wait(ctx context.Context, id int64) (state.Job, error) {
	q.mu.Lock()
	j, err := q.store.GetJob(id)
	if err != nil || j.Status.Terminal() {
		q.mu.Unlock()
		return j, err
	}
	ch := make(chan struct{})
	q.waiters[id] = append(q.waiters[id], ch)
	q.mu.Unlock()
	select {
	case <-ch:
		return q.store.GetJob(id)
	case <-ctx.Done():
		return j, ctx.Err()
	}
}

// Running is the number of jobs running now.
func (q *Queue) Running() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.running)
}

// Drain stops the queue (SPEC.md §6): nothing new starts or is accepted,
// running jobs finish until ctx's deadline, then the rest are cancelled and
// given ShutdownGrace. It reports how many were still running when it gave
// up (they stay "running" for the next start's recovery).
func (q *Queue) Drain(ctx context.Context) int {
	q.mu.Lock()
	if q.draining {
		q.mu.Unlock()
		return 0
	}
	q.draining = true
	started := q.started
	n := len(q.running)
	q.mu.Unlock()
	if started {
		close(q.stop)
		<-q.done
	}
	if n > 0 {
		q.log.Info("draining jobs", "running", n)
	}
	idle := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(idle)
	}()
	select {
	case <-idle:
		return 0
	case <-ctx.Done():
	}
	q.mu.Lock()
	var kinds []string
	for _, h := range q.running {
		h.cancel(errShutdown)
		kinds = append(kinds, h.job.Kind)
	}
	q.mu.Unlock()
	q.log.Warn("drain deadline reached; cancelling the jobs still running", "kinds", strings.Join(kinds, ","))
	select {
	case <-idle:
		return 0
	case <-time.After(q.opt.ShutdownGrace):
	}
	left := q.Running()
	q.log.Warn("jobs still running at shutdown; the next start settles them", "running", left)
	return left
}
