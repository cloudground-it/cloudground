package throttle

import (
	"context"
	"errors"
	"sync"
)

// ErrQueueFull is an attempt refused because Lanes already holds its
// maximum for the key and group.
var ErrQueueFull = errors.New("too many attempts in flight for this key")

// Lanes evaluates attempts on one key one at a time, in the order they
// arrive: the credential checks of one name at sign-in (internal/api
// SPEC §2.3 step 3, §6.4 step 3). The bound on what is in flight (the
// attempt being evaluated and those waiting for it) is per key and group,
// the group being the client's network: one network that fills its share
// gets ErrQueueFull, every other network still queues, and waits at most
// for the attempts already ahead of it. The zero value is not ready: use
// NewLanes.
type Lanes struct {
	depth int

	mu    sync.Mutex
	lanes map[string]*lane
}

type lane struct {
	turn   chan struct{} // holds a token while an attempt is evaluated
	total  int           // attempts holding or waiting for the turn
	groups map[string]int
}

// NewLanes returns lanes that hold at most depth attempts per key and
// group.
func NewLanes(depth int) *Lanes {
	return &Lanes{depth: depth, lanes: map[string]*lane{}}
}

// Enter waits for key's turn and returns the function that ends it (a
// second call does nothing). It fails with ErrQueueFull when depth
// attempts of group are already in flight for key, and with ctx's error
// when the request goes away first. Waiters take the turn in arrival order
// (a channel's senders queue first in, first out).
func (q *Lanes) Enter(ctx context.Context, key, group string) (func(), error) {
	q.mu.Lock()
	l := q.lanes[key]
	if l == nil {
		l = &lane{turn: make(chan struct{}, 1), groups: map[string]int{}}
		q.lanes[key] = l
	}
	if l.groups[group] >= q.depth {
		q.mu.Unlock()
		return nil, ErrQueueFull
	}
	l.groups[group]++
	l.total++
	q.mu.Unlock()
	leave := func() {
		q.mu.Lock()
		if l.groups[group]--; l.groups[group] == 0 {
			delete(l.groups, group)
		}
		if l.total--; l.total == 0 {
			delete(q.lanes, key)
		}
		q.mu.Unlock()
	}
	select {
	case l.turn <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.turn
			leave()
		})
	}, nil
}

// InFlight is how many attempts for key are evaluated or waiting, of every
// group.
func (q *Lanes) InFlight(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if l := q.lanes[key]; l != nil {
		return l.total
	}
	return 0
}

// Keys is how many keys have an attempt in flight (tests: nothing is kept
// for a key once its attempts are over).
func (q *Lanes) Keys() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.lanes)
}
