package throttle

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("timed out")
		}
	}
}

func TestLanes(t *testing.T) {
	q := NewLanes(2)
	ctx := context.Background()
	done, err := q.Enter(ctx, "k", "net-a")
	if err != nil {
		t.Fatal(err)
	}
	// The second from the same group waits for the first; a third from it
	// is refused.
	got := make(chan func(), 1)
	go func() {
		d, err := q.Enter(ctx, "k", "net-a")
		if err != nil {
			t.Error(err)
		}
		got <- d
	}()
	waitUntil(t, func() bool { return q.InFlight("k") == 2 })
	time.Sleep(10 * time.Millisecond) // blocked on the turn, not just counted
	if _, err := q.Enter(ctx, "k", "net-a"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("a third attempt from one group: %v", err)
	}
	// Another group still queues for the key: the first group's backlog
	// refuses nobody else.
	other := make(chan func(), 1)
	go func() {
		d, err := q.Enter(ctx, "k", "net-b")
		if err != nil {
			t.Error(err)
		}
		other <- d
	}()
	waitUntil(t, func() bool { return q.InFlight("k") == 3 })
	if d, err := q.Enter(ctx, "other", "net-a"); err != nil {
		t.Fatal("one key's queue held another")
	} else {
		d()
	}
	select {
	case <-got:
		t.Fatal("the second attempt ran during the first")
	case <-other:
		t.Fatal("another group's attempt ran during the first")
	case <-time.After(20 * time.Millisecond):
	}
	done()
	done() // a second call is harmless
	// In arrival order: net-a's second, then net-b's.
	second := <-got
	select {
	case <-other:
		t.Fatal("two attempts for one key ran at once")
	case <-time.After(20 * time.Millisecond):
	}
	second()
	(<-other)()
	if q.InFlight("k") != 0 || q.Keys() != 0 {
		t.Fatal("an empty lane was kept")
	}
	// A request that goes away while it waits leaves the queue.
	hold, _ := q.Enter(ctx, "k", "net-a")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := q.Enter(cctx, "k", "net-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait: %v", err)
	}
	if q.InFlight("k") != 1 {
		t.Fatal("a cancelled wait stayed in the queue")
	}
	hold()
	if q.Keys() != 0 {
		t.Fatal("a lane was kept")
	}
}
