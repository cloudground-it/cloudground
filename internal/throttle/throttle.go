// Package throttle counts events per key in a sliding time window, in
// memory: the panel's sign-in and connector-token limits. Volumes are small
// and a restart forgetting the counts is acceptable, so there is no store.
package throttle

import (
	"sync"
	"time"
)

// Window admits at most a fixed number of events per key in any span of
// a fixed length.
type Window struct {
	max   int
	per   time.Duration
	clock func() time.Time

	mu    sync.Mutex
	seen  map[string][]time.Time // per key, oldest first
	swept time.Time
}

// New returns a window of max events per span.
func New(max int, per time.Duration) *Window {
	return &Window{max: max, per: per, clock: time.Now, seen: map[string][]time.Time{}}
}

// WithClock makes w read the time from now (tests); it returns w.
func (w *Window) WithClock(now func() time.Time) *Window {
	w.clock = now
	return w
}

// recent drops the key's events older than the window and returns the rest.
// Callers hold mu.
func (w *Window) recent(key string, at time.Time) []time.Time {
	list := w.seen[key]
	start := 0
	for start < len(list) && !list[start].After(at.Add(-w.per)) {
		start++
	}
	list = list[start:]
	if len(list) == 0 {
		delete(w.seen, key)
		return nil
	}
	w.seen[key] = list
	return list
}

// sweep forgets keys with nothing in the window, once per window, so the
// map does not grow with every address ever seen. Callers hold mu.
func (w *Window) sweep(at time.Time) {
	if at.Sub(w.swept) < w.per {
		return
	}
	w.swept = at
	for key := range w.seen {
		w.recent(key, at)
	}
}

// Allow records an event for key when the window has room for it, and
// reports whether it had.
func (w *Window) Allow(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.clock()
	w.sweep(at)
	if len(w.recent(key, at)) >= w.max {
		return false
	}
	w.seen[key] = append(w.seen[key], at)
	return true
}

// Blocked reports whether key's window is full, recording nothing.
func (w *Window) Blocked(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.recent(key, w.clock())) >= w.max
}

// Count is how many events key has in the window, recording nothing.
func (w *Window) Count(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.recent(key, w.clock()))
}

// Keys is how many keys have events in the window, once the idle ones are
// forgotten.
func (w *Window) Keys() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	at := w.clock()
	for key := range w.seen {
		w.recent(key, at)
	}
	return len(w.seen)
}

// Reset forgets key's events.
func (w *Window) Reset(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, key)
}
