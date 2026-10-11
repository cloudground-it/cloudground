package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/state"
)

func openStore(t *testing.T) (*state.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newQueue(t *testing.T, st *state.Store, opt Options) *Queue {
	t.Helper()
	opt.Log = quietLog()
	if opt.Poll == 0 {
		opt.Poll = 20 * time.Millisecond
	}
	if opt.ShutdownGrace == 0 {
		opt.ShutdownGrace = 200 * time.Millisecond
	}
	return New(st, opt)
}

// gate is a job function that runs until released, recording when it
// started.
type gate struct {
	mu      sync.Mutex
	started map[string]bool
	order   []string
	release map[string]chan struct{}
}

func newGate() *gate {
	return &gate{started: map[string]bool{}, release: map[string]chan struct{}{}}
}

func (g *gate) fn(name string) Func {
	g.mu.Lock()
	ch := make(chan struct{})
	g.release[name] = ch
	g.mu.Unlock()
	return func(ctx context.Context, r *Run) error {
		g.mu.Lock()
		g.started[name] = true
		g.order = append(g.order, name)
		g.mu.Unlock()
		r.Progress(50, name+" working")
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gate) open(name string) { close(g.release[name]) }

func (g *gate) isStarted(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started[name]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func status(t *testing.T, st *state.Store, id int64) state.JobStatus {
	t.Helper()
	j, err := st.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	return j.Status
}

func mustEnqueue(t *testing.T, q *Queue, spec Spec) state.Job {
	t.Helper()
	j, err := q.Enqueue(spec)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestSiteLockSerialisesAndQueuedJobsWait(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "site.x"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	g := newGate()
	a := mustEnqueue(t, q, Spec{Kind: "site.x", SiteID: 1, Locks: []string{SiteLock(1)}, Func: g.fn("a")})
	b := mustEnqueue(t, q, Spec{Kind: "site.x", SiteID: 1, Locks: []string{SiteLock(1)}, Func: g.fn("b")})
	other := mustEnqueue(t, q, Spec{Kind: "site.x", SiteID: 2, Locks: []string{SiteLock(2)}, Func: g.fn("other")})
	eventually(t, "a and the other site's job start", func() bool { return g.isStarted("a") && g.isStarted("other") })
	time.Sleep(100 * time.Millisecond)
	if g.isStarted("b") {
		t.Fatal("a second job on the same site started while the first ran")
	}
	if got := status(t, st, b.ID); got != state.JobQueued {
		t.Fatalf("the waiting job is %s, want queued (it waits, it does not fail)", got)
	}
	g.open("a")
	eventually(t, "b starts once a ends", func() bool { return g.isStarted("b") })
	if got := status(t, st, a.ID); got != state.JobSucceeded {
		t.Fatalf("a = %s", got)
	}
	g.open("b")
	g.open("other")
	for _, id := range []int64{b.ID, other.ID} {
		if j, err := q.Wait(context.Background(), id); err != nil || j.Status != state.JobSucceeded || j.Progress != 100 {
			t.Fatalf("job %d = %+v %v", id, j, err)
		}
	}
}

// A person's job blocked on a lock keeps the scheduler's jobs off every key
// it needs (a scheduled verify does not overtake a waiting restore), while
// a job with nothing in common runs.
func TestBlockedJobReservesItsLocks(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	g := newGate()
	repo := RepoLock("s3:https://user:pw@example.com/bucket")
	// Enqueued before Start so the order is fixed when dispatch first looks.
	mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{SiteLock(1)}, Func: g.fn("site1")})
	mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{SiteLock(1), repo}, Func: g.fn("restore")})
	mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{repo}, Background: true, Func: g.fn("verify")})
	mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{SiteLock(9)}, Func: g.fn("unrelated")})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	eventually(t, "site1 and unrelated start", func() bool { return g.isStarted("site1") && g.isStarted("unrelated") })
	time.Sleep(100 * time.Millisecond)
	if g.isStarted("verify") {
		t.Fatal("verify overtook the older restore waiting for the same repository")
	}
	g.open("site1")
	eventually(t, "restore starts", func() bool { return g.isStarted("restore") })
	time.Sleep(50 * time.Millisecond)
	if g.isStarted("verify") {
		t.Fatal("verify ran beside the restore holding the repository lock")
	}
	g.open("restore")
	eventually(t, "verify starts", func() bool { return g.isStarted("verify") })
	g.open("verify")
	g.open("unrelated")
	if strings.Contains(repo, "pw") {
		t.Fatalf("lock key %q carries the repository's credentials", repo)
	}
}

func TestGlobalLimit(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{Limit: 2})
	q.Register(Kind{Name: "k"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	var running, peak atomic.Int32
	release := make(chan struct{})
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, mustEnqueue(t, q, Spec{Kind: "k", Func: func(ctx context.Context, r *Run) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			return nil
		}}).ID)
	}
	eventually(t, "two jobs run", func() bool { return running.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	close(release)
	for _, id := range ids {
		if j, _ := q.Wait(context.Background(), id); j.Status != state.JobSucceeded {
			t.Fatalf("job %d = %s", id, j.Status)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want the limit 2", peak.Load())
	}
}

func TestCancelQueuedIsImmediateAndRunsItsHook(t *testing.T) {
	st, _ := openStore(t)
	bus := events.New(0)
	sub, _, _ := bus.Subscribe("", 64)
	defer sub.Close()
	q := newQueue(t, st, Options{Events: bus})
	var kindHook atomic.Int32
	q.Register(Kind{Name: "k", Run: func(ctx context.Context, r *Run) error { return nil }, Canceled: func(state.Job) { kindHook.Add(1) }})
	q.Register(Kind{Name: "c"})
	g := newGate()
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	blocker := mustEnqueue(t, q, Spec{Kind: "c", Locks: []string{SiteLock(1)}, Func: g.fn("blocker")})
	eventually(t, "blocker runs", func() bool { return g.isStarted("blocker") })
	var specHook atomic.Int32
	queued := mustEnqueue(t, q, Spec{Kind: "c", Locks: []string{SiteLock(1)}, Func: g.fn("never"), Canceled: func() { specHook.Add(1) }})
	payloadJob := mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{SiteLock(1)}})
	for _, id := range []int64{queued.ID, payloadJob.ID} {
		j, err := q.Cancel(id)
		if err != nil || j.Status != state.JobCanceled || j.Error != "Canceled" || j.FinishedAt == nil {
			t.Fatalf("cancel %d = %+v %v", id, j, err)
		}
	}
	if specHook.Load() != 1 || kindHook.Load() != 1 {
		t.Fatalf("cancel hooks ran spec=%d kind=%d, want 1 and 1", specHook.Load(), kindHook.Load())
	}
	if _, err := q.Cancel(queued.ID); !errors.Is(err, ErrFinished) {
		t.Fatalf("second cancel = %v, want ErrFinished", err)
	}
	g.open("blocker")
	q.Wait(context.Background(), blocker.ID)
	time.Sleep(50 * time.Millisecond)
	if g.isStarted("never") {
		t.Fatal("a canceled job started")
	}
	// The browser saw it end, in the task vocabulary.
	sawFinished := false
	for done := false; !done; {
		select {
		case ev := <-sub.C():
			if ev.Type == events.TaskFinished {
				d := ev.Data.(map[string]any)
				if d["id"] == queued.ID && d["status"] == "failed" && d["error"] == "Canceled" {
					sawFinished = true
				}
			}
		default:
			done = true
		}
	}
	if !sawFinished {
		t.Fatal("no task.finished for the canceled job")
	}
	if v := View(mustGet(t, st, queued.ID)); v.Status != "failed" || v.JobStatus != state.JobCanceled {
		t.Fatalf("view of a canceled job = %+v", v)
	}
}

func mustGet(t *testing.T, st *state.Store, id int64) state.Job {
	t.Helper()
	j, err := st.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestCancelRunningCancelsItsContext(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	g := newGate()
	stops := mustEnqueue(t, q, Spec{Kind: "k", Func: g.fn("stops")})
	finishes := mustEnqueue(t, q, Spec{Kind: "k", Func: func(ctx context.Context, r *Run) error {
		<-ctx.Done()
		return nil // a job may decide to complete anyway
	}})
	eventually(t, "both run", func() bool {
		return status(t, st, stops.ID) == state.JobRunning && status(t, st, finishes.ID) == state.JobRunning
	})
	for _, id := range []int64{stops.ID, finishes.ID} {
		j, err := q.Cancel(id)
		if err != nil || !j.CancelRequested {
			t.Fatalf("cancel running %d = %+v %v", id, j, err)
		}
	}
	if j, _ := q.Wait(context.Background(), stops.ID); j.Status != state.JobCanceled || j.Error != "Canceled" {
		t.Fatalf("stopped job = %s %q", j.Status, j.Error)
	}
	if j, _ := q.Wait(context.Background(), finishes.ID); j.Status != state.JobSucceeded {
		t.Fatalf("a job that completed despite the cancel = %s, want succeeded", j.Status)
	}
}

func TestDrainWaitsForRunningJobsAndRefusesNewOnes(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	g := newGate()
	j := mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{"x"}, Func: g.fn("j")})
	queued := mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{"x"}, Func: g.fn("q")})
	eventually(t, "j runs", func() bool { return g.isStarted("j") })
	done := make(chan int)
	go func() { done <- q.Drain(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := q.Enqueue(Spec{Kind: "k", Func: g.fn("late")}); !errors.Is(err, ErrDraining) {
		t.Fatalf("enqueue while draining = %v, want ErrDraining", err)
	}
	select {
	case <-done:
		t.Fatal("drain returned while a job ran")
	default:
	}
	g.open("j")
	if left := <-done; left != 0 {
		t.Fatalf("drain left %d running", left)
	}
	if got := status(t, st, j.ID); got != state.JobSucceeded {
		t.Fatalf("drained job = %s", got)
	}
	time.Sleep(50 * time.Millisecond)
	if g.isStarted("q") || status(t, st, queued.ID) != state.JobQueued {
		t.Fatal("a queued job started during the drain")
	}
}

// At the drain's deadline, a rerunnable job is put back in the queue and
// any other is failed; on the next start the rerunnable one runs.
func TestDrainDeadlineResumesOrFails(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	var reruns atomic.Int32
	blockRun := func(ctx context.Context, r *Run) error {
		if reruns.Add(1) > 1 {
			return nil // second process: completes
		}
		<-ctx.Done()
		return ctx.Err()
	}
	q.Register(Kind{Name: "rerun", Run: blockRun, Rerun: true})
	q.Register(Kind{Name: "once"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	g := newGate()
	re := mustEnqueue(t, q, Spec{Kind: "rerun"})
	once := mustEnqueue(t, q, Spec{Kind: "once", Func: g.fn("once")})
	eventually(t, "both run", func() bool { return g.isStarted("once") && reruns.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if left := q.Drain(ctx); left != 0 {
		t.Fatalf("drain left %d", left)
	}
	if j := mustGet(t, st, re.ID); j.Status != state.JobQueued || !strings.Contains(View(j).Log, "panel shutdown") {
		t.Fatalf("rerunnable job after the drain deadline = %s %q", j.Status, View(j).Log)
	}
	if j := mustGet(t, st, once.ID); j.Status != state.JobFailed || j.Error != "interrupted by panel shutdown" {
		t.Fatalf("other job after the drain deadline = %s %q", j.Status, j.Error)
	}

	q2 := newQueue(t, st, Options{})
	q2.Register(Kind{Name: "rerun", Run: blockRun, Rerun: true})
	q2.Register(Kind{Name: "once"})
	if err := q2.Start(); err != nil {
		t.Fatal(err)
	}
	defer q2.Drain(context.Background())
	if j, _ := q2.Wait(context.Background(), re.ID); j.Status != state.JobSucceeded || j.Attempts != 2 {
		t.Fatalf("resumed job = %s after %d attempts", j.Status, j.Attempts)
	}
}

// A process that dies leaves rows behind; the next start settles every one
// of them (SPEC.md §6, recovery table).
func TestRestartRecovery(t *testing.T) {
	st, path := openStore(t)
	q := newQueue(t, st, Options{Limit: 10})
	hold := make(chan struct{})
	defer close(hold)
	blocking := func(ctx context.Context, r *Run) error {
		r.Progress(30, "half way")
		<-hold
		return nil
	}
	q.Register(Kind{Name: "rerun", Run: blocking, Rerun: true})
	q.Register(Kind{Name: "payload", Run: blocking})
	q.Register(Kind{Name: "closure"})
	q.Register(Kind{Name: "gone"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	runRerun := mustEnqueue(t, q, Spec{Kind: "rerun", Locks: []string{"a"}})
	runPayload := mustEnqueue(t, q, Spec{Kind: "payload", SiteID: 7, Locks: []string{"b"}})
	runClosure := mustEnqueue(t, q, Spec{Kind: "closure", Locks: []string{"c"}, Func: blocking})
	eventually(t, "three running", func() bool { return q.Running() == 3 })
	queuedPayload := mustEnqueue(t, q, Spec{Kind: "payload", Locks: []string{"b"}})
	queuedClosure := mustEnqueue(t, q, Spec{Kind: "closure", Locks: []string{"c"}, Func: blocking})
	queuedGone := mustEnqueue(t, q, Spec{Kind: "gone", Locks: []string{"c"}, Func: blocking})
	eventually(t, "progress saved", func() bool { return mustGet(t, st, runRerun.ID).Progress == 30 })

	// The process dies here: no drain. A new one opens the same database.
	st2, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	q2 := newQueue(t, st2, Options{Limit: 10})
	var interrupted []int64
	var mu sync.Mutex
	hook := func(j state.Job) { mu.Lock(); interrupted = append(interrupted, j.ID); mu.Unlock() }
	ran := make(chan int64, 4)
	quick := func(ctx context.Context, r *Run) error { ran <- r.Job().ID; return nil }
	q2.Register(Kind{Name: "rerun", Run: quick, Rerun: true})
	q2.Register(Kind{Name: "payload", Run: quick, Interrupted: hook})
	q2.Register(Kind{Name: "closure", Interrupted: hook})
	recovered := false
	q2.opt.Recovered = func() { recovered = true }
	if err := q2.Start(); err != nil {
		t.Fatal(err)
	}
	defer q2.Drain(context.Background())
	if !recovered {
		t.Fatal("the Recovered callback did not run")
	}
	want := map[int64]state.JobStatus{
		runRerun.ID:      state.JobSucceeded, // resumed and run again
		runPayload.ID:    state.JobFailed,    // running, not rerunnable
		runClosure.ID:    state.JobFailed,    // running, its function died
		queuedPayload.ID: state.JobSucceeded, // queued, resumable
		queuedClosure.ID: state.JobFailed,    // queued, its function died
		queuedGone.ID:    state.JobFailed,    // its kind no longer exists
	}
	for id, w := range want {
		j, err := q2.Wait(context.Background(), id)
		if err != nil || j.Status != w {
			t.Errorf("job %d (%s) = %s %q, want %s", id, j.Kind, j.Status, j.Error, w)
		}
	}
	if j := mustGet(t, st2, runRerun.ID); !strings.Contains(View(j).Log, "half way") || !strings.Contains(View(j).Log, "panel restart") {
		t.Errorf("resumed job's log = %q, want its first run and the restart", View(j).Log)
	}
	if j := mustGet(t, st2, runPayload.ID); j.Error != "interrupted by panel restart" || j.Progress != 30 {
		t.Errorf("interrupted job = %q at %d%%", j.Error, j.Progress)
	}
	if j := mustGet(t, st2, queuedClosure.ID); j.Error != "not started: the panel restarted before it could run" {
		t.Errorf("lost queued job = %q", j.Error)
	}
	if j := mustGet(t, st2, queuedGone.ID); j.Error != "unknown job kind after a panel restart" {
		t.Errorf("unknown-kind job = %q", j.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(interrupted) != 3 {
		t.Errorf("interrupted hooks ran for %v, want the three failed jobs of known kinds", interrupted)
	}
	active, _ := st2.ActiveJobs()
	for _, j := range active {
		t.Errorf("job %d still %s after recovery", j.ID, j.Status)
	}
}

func TestDedupKeyAdmitsOneActiveJob(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	g := newGate()
	a := mustEnqueue(t, q, Spec{Kind: "k", DedupKey: "backup.verify:1", Func: g.fn("a")})
	b := mustEnqueue(t, q, Spec{Kind: "k", DedupKey: "backup.verify:1", Func: g.fn("b")})
	if a.ID != b.ID {
		t.Fatalf("a second job with the same key was created (%d, %d)", a.ID, b.ID)
	}
	g.open("a")
	q.Wait(context.Background(), a.ID)
	c := mustEnqueue(t, q, Spec{Kind: "k", DedupKey: "backup.verify:1", Func: func(context.Context, *Run) error { return nil }})
	if c.ID == a.ID {
		t.Fatal("a finished job still held its dedup key")
	}
}

func TestPanicFailsTheJob(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	j := mustEnqueue(t, q, Spec{Kind: "k", Func: func(ctx context.Context, r *Run) error {
		r.Progress(40, "about to fail")
		panic("boom")
	}})
	got, _ := q.Wait(context.Background(), j.ID)
	if got.Status != state.JobFailed || !strings.Contains(got.Error, "boom") || got.Progress != 40 {
		t.Fatalf("panicking job = %+v", got)
	}
}

func TestBoundedLog(t *testing.T) {
	var l boundedLog
	line := strings.Repeat("x", 99) // 100 bytes with its newline
	for i := 0; i < 2000; i++ {
		l.add(fmt.Sprintf("%04d%s", i, line[4:]))
	}
	if len(l.head) > LogHeadBytes || len(l.tail) > LogTailBytes {
		t.Fatalf("head %d, tail %d bytes: over the bounds", len(l.head), len(l.tail))
	}
	s := l.String()
	if !strings.HasPrefix(s, "0000") {
		t.Fatal("the log lost its first line")
	}
	if !strings.Contains(s, "1999") {
		t.Fatal("the log lost its last line")
	}
	if !strings.Contains(s, "bytes of log omitted") || l.dropped == 0 {
		t.Fatal("no marker for the dropped middle")
	}
	total := int64(2000 * 100)
	if int64(len(l.head)+len(l.tail))+l.dropped != total {
		t.Fatalf("head+tail+dropped = %d, want every byte accounted for (%d)", int64(len(l.head)+len(l.tail))+l.dropped, total)
	}
	for _, part := range []string{l.head, l.tail} {
		if !strings.HasSuffix(part, "\n") {
			t.Fatal("a part does not end on a whole line")
		}
	}
	if len(strings.SplitN(l.tail, "\n", 2)[0]) != 99 {
		t.Fatal("the tail does not start on a whole line")
	}
	long := capLine(strings.Repeat("é", LogLineBytes))
	if len(long) > LogLineBytes+len("…") || !strings.HasSuffix(long, "…") {
		t.Fatalf("a long line was not cut: %d bytes", len(long))
	}
	if short := (boundedLog{head: "a\n"}).String(); short != "a\n" {
		t.Fatalf("a short log reads %q", short)
	}
}

// The log a running job writes is bounded in the database, not only in
// memory.
func TestJobLogIsBoundedInStore(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	j := mustEnqueue(t, q, Spec{Kind: "k", Func: func(ctx context.Context, r *Run) error {
		for i := 0; i < 1500; i++ {
			r.Progress(i/15, fmt.Sprintf("step %d %s", i, strings.Repeat("y", 80)))
		}
		return nil
	}})
	got, _ := q.Wait(context.Background(), j.ID)
	log := View(got).Log
	if len(log) > LogHeadBytes+LogTailBytes+100 {
		t.Fatalf("stored log is %d bytes", len(log))
	}
	if !strings.Contains(log, "step 0 ") || !strings.Contains(log, "step 1499 ") {
		t.Fatal("stored log lost its head or its tail")
	}
}

func TestSchedulerRunsDueSchedulesAndTicks(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	var mu sync.Mutex
	counts := map[string]int{}
	var order []string
	mk := func(name string, d time.Duration) Func {
		return func(ctx context.Context, r *Run) error {
			time.Sleep(d)
			mu.Lock()
			counts[name]++
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	for _, n := range []string{"fast", "slow", "daily", "after", "never"} {
		q.Register(Kind{Name: n, Quiet: true, Rerun: true, Run: mk(n, 20*time.Millisecond)})
	}
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	spec := func(kind string) func() Spec { return func() Spec { return Spec{Kind: kind} } }
	sc := NewScheduler(q, []Schedule{
		{Name: "fast", Every: time.Hour, Tick: InTick, Spec: spec("fast")},
		{Name: "slow", Every: time.Hour, Tick: InTick, Spec: spec("slow")},
		{Name: "daily", Every: 24 * time.Hour, PersistKey: "cert.renew", Tick: InTickWhenDue, Spec: spec("daily")},
		{Name: "after", Every: time.Hour, Tick: AfterTick, Spec: spec("after")},
		{Name: "never", Every: time.Hour, Spec: spec("never")},
	})
	sc.FirstWake, sc.Every = 10*time.Millisecond, 10*time.Millisecond
	sc.Start()
	eventually(t, "every schedule ran once", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return counts["fast"] == 1 && counts["slow"] == 1 && counts["daily"] == 1 && counts["after"] == 1 && counts["never"] == 1
	})
	time.Sleep(100 * time.Millisecond)
	sc.Stop()
	mu.Lock()
	if counts["fast"] != 1 {
		t.Fatalf("a schedule ran %d times within its interval", counts["fast"])
	}
	order = nil
	mu.Unlock()
	if last, _ := st.LastScheduleRun("cert.renew"); last.IsZero() {
		t.Fatal("a persisted schedule did not record its run")
	}

	if err := sc.RunTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["fast"] != 2 || counts["slow"] != 2 || counts["after"] != 2 {
		t.Fatalf("run-now did not run the tick's schedules: %v", counts)
	}
	if counts["daily"] != 1 || counts["never"] != 1 {
		t.Fatalf("run-now ran a schedule that was not due or not in the tick: %v", counts)
	}
	if len(order) != 3 || order[2] != "after" {
		t.Fatalf("tick order %v, want the after-tick schedule last", order)
	}
	// Quiet jobs are not listed as tasks.
	if listed, _ := st.ListJobs(100, false); len(listed) != 0 {
		t.Fatalf("%d quiet jobs listed as tasks", len(listed))
	}
}

// Run-now while the same schedule's job is running waits for that job
// rather than starting a second.
func TestRunTickWaitsForARunningSweep(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	release := make(chan struct{})
	var runs atomic.Int32
	q.Register(Kind{Name: "sweep", Quiet: true, Run: func(ctx context.Context, r *Run) error {
		runs.Add(1)
		<-release
		return nil
	}})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	sc := NewScheduler(q, []Schedule{{Name: "sweep", Every: time.Hour, Tick: InTick, Spec: func() Spec { return Spec{Kind: "sweep"} }}})
	sc.wake()
	eventually(t, "sweep runs", func() bool { return runs.Load() == 1 })
	done := make(chan error)
	go func() { done <- sc.RunTick(context.Background()) }()
	select {
	case <-done:
		t.Fatal("run-now answered while the sweep ran")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 1 {
		t.Fatalf("the sweep ran %d times; run-now must join the running one", runs.Load())
	}
}

func TestPruneJobs(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "quiet", Quiet: true, Run: func(context.Context, *Run) error { return nil }})
	q.Register(Kind{Name: "loud", Run: func(context.Context, *Run) error { return nil }})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	a := mustEnqueue(t, q, Spec{Kind: "quiet"})
	b := mustEnqueue(t, q, Spec{Kind: "loud"})
	q.Wait(context.Background(), a.ID)
	q.Wait(context.Background(), b.ID)
	future := time.Now().Add(time.Minute)
	if n, err := st.PruneJobs(future, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d %v, want the quiet job only", n, err)
	}
	if _, err := st.GetJob(b.ID); err != nil {
		t.Fatal("the visible job was pruned before its time")
	}
}
