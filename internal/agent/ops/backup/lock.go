package backup

import (
	"context"
	"sync"
)

// repoLock is a repository's lock in the agent (SPEC §3): shared or
// exclusive, waited for under a context, with a waiting exclusive holder
// served before new shared ones.
type repoLock struct {
	mu       sync.Mutex
	shared   int
	held     bool // exclusively
	waiting  int  // exclusive holders waiting
	released chan struct{}
}

// acquire takes the lock and returns its release.
func (l *repoLock) acquire(ctx context.Context, exclusive bool) (func(), error) {
	queued := false
	for {
		l.mu.Lock()
		free := !l.held && l.shared == 0
		if !exclusive {
			free = !l.held && l.waiting == 0
		}
		if free {
			if exclusive {
				if queued {
					l.waiting--
				}
				l.held = true
			} else {
				l.shared++
			}
			l.mu.Unlock()
			return func() { l.release(exclusive) }, nil
		}
		if exclusive && !queued {
			l.waiting++
			queued = true
		}
		if l.released == nil {
			l.released = make(chan struct{})
		}
		ch := l.released
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			if queued {
				l.mu.Lock()
				l.waiting--
				l.broadcast()
				l.mu.Unlock()
			}
			return nil, ctx.Err()
		}
	}
}

func (l *repoLock) release(exclusive bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if exclusive {
		l.held = false
	} else {
		l.shared--
	}
	l.broadcast()
}

// broadcast wakes every waiter; l.mu is held.
func (l *repoLock) broadcast() {
	if l.released != nil {
		close(l.released)
		l.released = nil
	}
}

// lock takes the lock of the repository at url.
func (o *Ops) lock(ctx context.Context, url string, exclusive bool) (func(), error) {
	v, _ := o.locks.LoadOrStore(url, &repoLock{})
	return v.(*repoLock).acquire(ctx, exclusive)
}
