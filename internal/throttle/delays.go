package throttle

import (
	"errors"
	"sync"
	"time"
)

// Schedule is a delay that grows with failures: none below After, then
// First, doubled with each further failure, at most Max.
type Schedule struct {
	After      int
	First, Max time.Duration
}

// Delay is the wait for an attempt with n failures before it.
func (s Schedule) Delay(n int) time.Duration {
	if n < s.After || s.First <= 0 {
		return 0
	}
	d := s.First
	for range n - s.After {
		if d >= s.Max {
			break
		}
		d *= 2
	}
	return min(d, s.Max)
}

// keptFailures is the most failures a key keeps in the span. The schedules
// stop growing well below it, so a flood of failures for one key costs a
// fixed amount of memory and changes no delay.
const keptFailures = 32

// Delays is the sign-in delay of a name (internal/api SPEC §2.3 step 3,
// §6.4 step 3). Failures are counted twice over a span: per name and
// scope (the client's network, its IPv6 /48), on the scoped schedule,
// which grows to a long bound; and per name from anywhere, on the name
// schedule, capped low. An attempt waits the longer of the two, read from
// the failures counted so far plus the attempts in flight on the same key.
//
// On a scope the delays are spaced, as if the attempts came one after the
// other: an attempt's delay runs from when the one before it on the key
// starts its check, not from now, so parallel guesses from one network
// (or /48) are no faster than sequential ones. An attempt that would wait
// longer than the scoped schedule's bound is refused (ErrBacklog) and
// counts nothing: one network cannot pile up held requests. The name's
// delay is not spaced, so someone else's attempts, from any number of
// networks, add at most the name schedule's cap for the owner signing in
// from another network, and never refuse it. The wait is meant to be
// slept by the caller before it takes any per-name queue slot.
type Delays struct {
	scoped, name Schedule

	scopedFails, nameFails *Window

	clock func() time.Time

	mu      sync.Mutex
	pending map[string]int // attempts in flight per key
	// next is, per scoped key with attempts in flight, when the latest of
	// them starts its check: the next one's delay runs from there.
	next map[string]time.Time
}

// ErrBacklog is an attempt refused because its network already has
// attempts for the name waiting as long as the scoped schedule allows.
var ErrBacklog = errors.New("too many attempts waiting for this name from this network")

// NewDelays returns delays over failures of the last span.
func NewDelays(span time.Duration, scoped, name Schedule) *Delays {
	return &Delays{
		scoped: scoped, name: name,
		scopedFails: New(keptFailures, span),
		nameFails:   New(keptFailures, span),
		clock:       time.Now,
		pending:     map[string]int{},
		next:        map[string]time.Time{},
	}
}

// WithClock makes d read the time from now (tests); it returns d.
func (d *Delays) WithClock(now func() time.Time) *Delays {
	d.scopedFails.WithClock(now)
	d.nameFails.WithClock(now)
	d.clock = now
	return d
}

func scopedKey(name, scope string) string { return "s\x00" + scope + "\x00" + name }
func nameKey(name string) string          { return "n\x00" + name }

// Attempt is one check of a name's credentials. It ends with Fail,
// Succeed or End; the first of them counts, the rest do nothing.
type Attempt struct {
	d      *Delays
	name   string
	scopes []string
	wait   time.Duration

	once sync.Once
}

// Begin starts an attempt for name from scopes (empty ones are skipped)
// and reads its wait. It fails with ErrBacklog, counting nothing, when a
// scope's spaced delay would run past the scoped schedule's bound.
func (d *Delays) Begin(name string, scopes ...string) (*Attempt, error) {
	a := &Attempt{d: d, name: name}
	for _, s := range scopes {
		if s != "" {
			a.scopes = append(a.scopes, s)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	nk := nameKey(name)
	a.wait = d.name.Delay(d.nameFails.Count(name) + d.pending[nk])
	for _, s := range a.scopes {
		k := scopedKey(name, s)
		from := now
		if t, ok := d.next[k]; ok && t.After(from) {
			from = t
		}
		w := from.Add(d.scoped.Delay(d.scopedFails.Count(k) + d.pending[k])).Sub(now)
		if w > d.scoped.Max {
			return nil, ErrBacklog
		}
		a.wait = max(a.wait, w)
	}
	d.pending[nk]++
	start := now.Add(a.wait)
	for _, s := range a.scopes {
		k := scopedKey(name, s)
		d.pending[k]++
		if t, ok := d.next[k]; !ok || start.After(t) {
			d.next[k] = start
		}
	}
	return a, nil
}

// Wait is how long the attempt waits before its credentials are checked.
func (a *Attempt) Wait() time.Duration { return a.wait }

// end takes the attempt out of the ones in flight and runs verdict, under
// the lock, the first time only.
func (a *Attempt) end(verdict func()) {
	a.once.Do(func() {
		d := a.d
		d.mu.Lock()
		defer d.mu.Unlock()
		drop := func(k string) {
			if d.pending[k]--; d.pending[k] <= 0 {
				delete(d.pending, k)
				delete(d.next, k)
			}
		}
		drop(nameKey(a.name))
		for _, s := range a.scopes {
			drop(scopedKey(a.name, s))
		}
		verdict()
	})
}

// Fail counts a failure for the name and for each of its scopes.
func (a *Attempt) Fail() {
	a.end(func() {
		a.d.nameFails.Allow(a.name)
		for _, s := range a.scopes {
			a.d.scopedFails.Allow(scopedKey(a.name, s))
		}
	})
}

// Succeed forgets the name's failures and those of its scopes; other
// scopes keep theirs (an attacker's network keeps its delay when the owner
// signs in from elsewhere).
func (a *Attempt) Succeed() {
	a.end(func() {
		a.d.nameFails.Reset(a.name)
		for _, s := range a.scopes {
			a.d.scopedFails.Reset(scopedKey(a.name, s))
		}
	})
}

// End ends the attempt without a verdict: it was neither a failure nor a
// success (the request went away, a second factor was asked for).
func (a *Attempt) End() { a.end(func() {}) }

// Fail counts a failure for name from scopes outside an attempt (tests).
func (d *Delays) Fail(name string, scopes ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nameFails.Allow(name)
	for _, s := range scopes {
		if s != "" {
			d.scopedFails.Allow(scopedKey(name, s))
		}
	}
}

// Failures is how many failures name has in the span, from anywhere
// (tests).
func (d *Delays) Failures(name string) int { return d.nameFails.Count(name) }

// ScopedFailures is how many failures name has in the span from scope
// (tests).
func (d *Delays) ScopedFailures(name, scope string) int {
	return d.scopedFails.Count(scopedKey(name, scope))
}

// Size is how many keys d holds: failures and attempts in flight (tests:
// what an attack leaves behind is bounded).
func (d *Delays) Size() (failures, inFlight int) {
	d.mu.Lock()
	inFlight = len(d.pending) + len(d.next)
	d.mu.Unlock()
	return d.scopedFails.Keys() + d.nameFails.Keys(), inFlight
}
