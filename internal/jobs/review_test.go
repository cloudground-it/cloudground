package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A kind with NoCancel refuses every cancel, queued or running, with its
// message: a restore test is never skipped.
func TestNoCancelKindsRefuseCancel(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	g := newGate()
	q.Register(Kind{Name: "verify", NoCancel: "restore tests run to their end"})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	running := mustEnqueue(t, q, Spec{Kind: "verify", Locks: []string{"repo"}, Func: g.fn("a")})
	eventually(t, "a runs", func() bool { return g.isStarted("a") })
	queued := mustEnqueue(t, q, Spec{Kind: "verify", Locks: []string{"repo"}, Func: g.fn("b")})
	for _, id := range []int64{running.ID, queued.ID} {
		if _, err := q.Cancel(id); !errors.Is(err, ErrNotCancelable) || !strings.Contains(err.Error(), "restore tests") {
			t.Fatalf("cancel of job %d = %v, want ErrNotCancelable with the kind's message", id, err)
		}
	}
	g.open("a")
	g.open("b")
	for _, id := range []int64{running.ID, queued.ID} {
		if j := mustWait(t, q, id); j.Status != state.JobSucceeded {
			t.Fatalf("job %d = %s", id, j.Status)
		}
	}
}

func mustWait(t *testing.T, q *Queue, id int64) state.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, err := q.Wait(ctx, id)
	if err != nil {
		t.Fatalf("job %d: %v", id, err)
	}
	return j
}

// A queued job's cancel hook runs before anything queued behind it can
// start: the job behind sees what the hook put back.
func TestCancelHookRunsBeforeTheNextJobStarts(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	var value atomic.Int32
	value.Store(3)
	q.Register(Kind{Name: "k"})
	g := newGate()
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	first := mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{"site:1"}, Func: g.fn("first")})
	eventually(t, "first runs", func() bool { return g.isStarted("first") })
	canceled := mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{"site:1"}, Func: g.fn("never"),
		Canceled: func() {
			g.open("first") // the lock frees while the hook still runs
			time.Sleep(100 * time.Millisecond)
			value.Store(2)
		}})
	var seen atomic.Int32
	behind := mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{"site:1"}, Func: func(ctx context.Context, r *Run) error {
		seen.Store(value.Load())
		return nil
	}})
	if _, err := q.Cancel(canceled.ID); err != nil {
		t.Fatal(err)
	}
	mustWait(t, q, first.ID)
	mustWait(t, q, behind.ID)
	if seen.Load() != 2 {
		t.Fatalf("the job behind the canceled one saw %d, want the value its hook put back (2)", seen.Load())
	}
}

// A person's job goes first in the queue of a lock; a waiting job keeps
// the scheduler's jobs off the free keys it needs, not another person's.
func TestUserJobsGoBeforeBackgroundAndHoldOnlyWhatTheyWaitFor(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	q.Register(Kind{Name: "k"})
	g := newGate()
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	mustEnqueue(t, q, Spec{Kind: "k", Locks: []string{"repo"}, Background: true, Func: g.fn("backup1")})
	eventually(t, "backup1 runs", func() bool { return g.isStarted("backup1") })
	// Dispatch is held while these are queued, so it sees them together.
	q.mu.Lock()
	for _, sp := range []Spec{
		{Kind: "k", Locks: []string{"repo"}, Background: true, Func: g.fn("backup2")},
		{Kind: "k", Locks: []string{"site:1", "repo"}, Func: g.fn("restore")},
		{Kind: "k", Locks: []string{"site:1"}, Func: g.fn("edit")},
		{Kind: "k", Locks: []string{"site:1"}, Background: true, Func: g.fn("renew")},
	} {
		j, _, err := st.CreateJob(state.Job{Kind: sp.Kind, Locks: sp.Locks, Background: sp.Background})
		if err != nil {
			q.mu.Unlock()
			t.Fatal(err)
		}
		q.funcs[j.ID] = sp.Func
	}
	q.mu.Unlock()
	q.wake()
	eventually(t, "backup1 and the edit run", func() bool { return g.isStarted("backup1") && g.isStarted("edit") })
	time.Sleep(80 * time.Millisecond)
	if g.isStarted("renew") {
		t.Fatal("a background job took the site lock a waiting restore needs")
	}
	g.open("edit")
	g.open("backup1")
	eventually(t, "the restore runs next, ahead of the queued scheduled backup", func() bool { return g.isStarted("restore") })
	if g.isStarted("backup2") {
		t.Fatal("a scheduled backup ran before the person's restore")
	}
	g.open("restore")
	eventually(t, "the rest runs", func() bool { return g.isStarted("backup2") && g.isStarted("renew") })
	g.open("backup2")
	g.open("renew")
}

// Maintenance has its own slot: four long jobs do not hold it back.
func TestQuietJobsHaveTheirOwnSlots(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{Limit: 2})
	q.Register(Kind{Name: "long"})
	ran := make(chan struct{})
	q.Register(Kind{Name: "tick", Quiet: true, Run: func(context.Context, *Run) error { close(ran); return nil }})
	g := newGate()
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	mustEnqueue(t, q, Spec{Kind: "long", Func: g.fn("a")})
	mustEnqueue(t, q, Spec{Kind: "long", Func: g.fn("b")})
	mustEnqueue(t, q, Spec{Kind: "long", Func: g.fn("c")})
	eventually(t, "two long jobs run", func() bool { return g.isStarted("a") && g.isStarted("b") })
	mustEnqueue(t, q, Spec{Kind: "tick"})
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("a maintenance job waited behind the long jobs")
	}
	for _, n := range []string{"a", "b", "c"} {
		g.open(n)
	}
}

// Recovery never loses a cancel, never reruns a job past MaxAttempts, and
// holds a rerun back by its kind's delay.
func TestRecoveryCancelAttemptsAndDelay(t *testing.T) {
	st, path := openStore(t)
	q := newQueue(t, st, Options{Limit: 10})
	hold := make(chan struct{})
	defer close(hold)
	block := func(ctx context.Context, r *Run) error { <-hold; return nil }
	for _, n := range []string{"canceled", "tired", "delayed"} {
		q.Register(Kind{Name: n, Run: block, Rerun: true})
	}
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	canceled := mustEnqueue(t, q, Spec{Kind: "canceled", Locks: []string{"a"}})
	tired := mustEnqueue(t, q, Spec{Kind: "tired", Locks: []string{"b"}})
	delayed := mustEnqueue(t, q, Spec{Kind: "delayed", Locks: []string{"c"}})
	eventually(t, "three running", func() bool { return q.Running() == 3 })
	// The cancel lands in the store; the process dies before the job stops.
	if err := st.SetJobCancelRequested(canceled.ID); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	q.draining = true // freeze: nothing more starts here
	q.mu.Unlock()

	open := func() (*state.Store, *Queue, *sync.Map) {
		st2, err := state.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st2.Close() })
		now := time.Now()
		q2 := newQueue(t, st2, Options{Limit: 10, Now: func() time.Time { return now }})
		hooks := &sync.Map{}
		quick := func(ctx context.Context, r *Run) error { return nil }
		q2.Register(Kind{Name: "canceled", Run: quick, Rerun: true, Interrupted: func(j state.Job) { hooks.Store(j.ID, true) }})
		q2.Register(Kind{Name: "tired", Run: quick, Rerun: true, Interrupted: func(j state.Job) { hooks.Store(j.ID, true) }})
		q2.Register(Kind{Name: "delayed", Run: quick, Rerun: true, RerunDelay: time.Hour})
		return st2, q2, hooks
	}
	// Two earlier restarts used up attempts of "tired".
	for i := 0; i < MaxAttempts-1; i++ {
		if err := st.RequeueJob(tired.ID, "", "", 0, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.StartJob(tired.ID); err != nil {
			t.Fatal(err)
		}
	}
	st2, q2, hooks := open()
	if err := q2.Start(); err != nil {
		t.Fatal(err)
	}
	defer q2.Drain(context.Background())
	if j, _ := st2.GetJob(canceled.ID); j.Status != state.JobCanceled {
		t.Fatalf("a job canceled before the restart is %s, want canceled (never rerun)", j.Status)
	}
	if _, ok := hooks.Load(canceled.ID); !ok {
		t.Fatal("the canceled job's Interrupted hook did not run")
	}
	if j, _ := st2.GetJob(tired.ID); j.Status != state.JobFailed || !strings.Contains(j.Error, "3 times") {
		t.Fatalf("a job interrupted %d times = %s %q", MaxAttempts, j.Status, j.Error)
	}
	time.Sleep(100 * time.Millisecond)
	if j, _ := st2.GetJob(delayed.ID); j.Status != state.JobQueued || j.NotBefore == nil {
		t.Fatalf("a rerun with a delay = %s, not_before %v; want held back in the queue", j.Status, j.NotBefore)
	}
}

// A job whose locks changed is put back and started again under the locks
// its kind resolves now.
func TestResolveLocksAndRequeue(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	var want atomic.Value
	want.Store("repo:a")
	var starts atomic.Int32
	q.Register(Kind{Name: "backup", Rerun: true,
		ResolveLocks: func(state.Job) []string { return []string{want.Load().(string)} },
		Run: func(ctx context.Context, r *Run) error {
			starts.Add(1)
			if r.Job().Locks[0] != "repo:b" {
				want.Store("repo:b") // the repository changed under it
				return ErrRequeue
			}
			return nil
		}})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	defer q.Drain(context.Background())
	j := mustEnqueue(t, q, Spec{Kind: "backup", Locks: []string{"repo:old"}})
	got := mustWait(t, q, j.ID)
	if got.Status != state.JobSucceeded || starts.Load() != 2 || got.Locks[0] != "repo:b" {
		t.Fatalf("job = %s after %d starts under %v", got.Status, starts.Load(), got.Locks)
	}
}

// At the drain deadline a job that is not rerun fails with its Interrupted
// hook; one canceled by a person meanwhile ends canceled, not requeued.
func TestDrainDeadlineHooksAndCancels(t *testing.T) {
	st, _ := openStore(t)
	q := newQueue(t, st, Options{})
	var hooked atomic.Int64
	wait := func(ctx context.Context, r *Run) error { <-ctx.Done(); return ctx.Err() }
	q.Register(Kind{Name: "once", Interrupted: func(j state.Job) { hooked.Store(j.ID) }})
	q.Register(Kind{Name: "rerun", Run: wait, Rerun: true})
	if err := q.Start(); err != nil {
		t.Fatal(err)
	}
	once := mustEnqueue(t, q, Spec{Kind: "once", Func: wait})
	asked := mustEnqueue(t, q, Spec{Kind: "rerun"})
	eventually(t, "both run", func() bool { return q.Running() == 2 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		q.Cancel(asked.ID) //nolint:errcheck // the test reads the outcome below
	}()
	q.Drain(ctx)
	if j, _ := st.GetJob(once.ID); j.Status != state.JobFailed || hooked.Load() != once.ID {
		t.Fatalf("job cut by the drain = %s, hook ran for %d", j.Status, hooked.Load())
	}
	if j, _ := st.GetJob(asked.ID); j.Status != state.JobCanceled {
		t.Fatalf("a rerunnable job canceled during the drain = %s, want canceled", j.Status)
	}
}
