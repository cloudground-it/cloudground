package blocklist

import (
	"net/netip"
	"sync"
	"time"
)

// maxUnits bounds the units a tracker counts at once (SPEC §3).
const maxUnits = 50000

// maxCount bounds the failures one report may add.
const maxCount = 100

// Tracker counts failures per unit in a sliding window and says when a
// unit has reached the threshold (SPEC §3). In memory: a restart forgetting
// the counts costs a guesser's next few attempts, nothing more.
type Tracker struct {
	mu   sync.Mutex
	seen map[netip.Prefix][]time.Time // per unit, oldest first
	max  int                          // maxUnits; tests lower it
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{seen: map[netip.Prefix][]time.Time{}, max: maxUnits}
}

// Fail adds count failures from a at now and reports whether its unit is
// now to be blocked; the unit then starts again from zero. An exempt
// address, or settings with automatic blocking off, count nothing.
func (t *Tracker) Fail(a netip.Addr, count int, now time.Time, s Settings, ex Exempt) (netip.Prefix, bool) {
	if !s.Enabled || s.MaxFailures < 1 || ex.Has(a) {
		return netip.Prefix{}, false
	}
	count = min(max(count, 1), maxCount)
	unit := Unit(a)
	window := time.Duration(s.WindowMinutes) * time.Minute
	t.mu.Lock()
	defer t.mu.Unlock()
	list, known := t.seen[unit]
	if !known && len(t.seen) >= t.max {
		t.sweep(now, window)
		if len(t.seen) >= t.max {
			return netip.Prefix{}, false
		}
	}
	list = recent(list, now, window)
	for range count {
		list = append(list, now)
	}
	if len(list) >= s.MaxFailures {
		delete(t.seen, unit)
		return unit, true
	}
	t.seen[unit] = list
	return netip.Prefix{}, false
}

// Forget drops what a unit has counted (it was blocked or allowed by hand).
func (t *Tracker) Forget(unit netip.Prefix) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, unit)
}

// Len is the number of units counting (tests).
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}

// sweep drops the units with nothing in the window. Callers hold mu.
func (t *Tracker) sweep(now time.Time, window time.Duration) {
	for unit, list := range t.seen {
		if len(recent(list, now, window)) == 0 {
			delete(t.seen, unit)
		}
	}
}

// recent is list without the failures older than the window.
func recent(list []time.Time, now time.Time, window time.Duration) []time.Time {
	start := 0
	for start < len(list) && !list[start].After(now.Add(-window)) {
		start++
	}
	return list[start:]
}
