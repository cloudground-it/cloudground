package throttle

import (
	"fmt"
	"testing"
	"time"
)

var (
	long  = Schedule{After: 5, First: time.Second, Max: time.Minute}
	short = Schedule{After: 5, First: time.Second, Max: 2 * time.Second}
)

func TestSchedule(t *testing.T) {
	var got []time.Duration
	for n := range 14 {
		got = append(got, long.Delay(n))
	}
	want := "[0s 0s 0s 0s 0s 1s 2s 4s 8s 16s 32s 1m0s 1m0s 1m0s]"
	if fmt.Sprint(got) != want {
		t.Fatalf("delays %v, want %s", got, want)
	}
	if short.Delay(5) != time.Second || short.Delay(9) != 2*time.Second || short.Delay(1000) != 2*time.Second {
		t.Fatal("the short schedule is not capped at 2s")
	}
}

// One network's failures grow its own delay; another network sees only
// the name's short one; parallel attempts each wait their own; the owner's
// success clears its network's failures and the name's, not the
// attacker's network's.
func TestDelays(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	d := NewDelays(15*time.Minute, long, short).WithClock(func() time.Time { return at })

	for range 9 {
		a := mustBegin(t, d, "site_1", "198.51.100.66")
		a.Fail()
	}
	if w := mustBegin(t, d, "site_1", "198.51.100.66"); w.Wait() != 16*time.Second {
		t.Fatalf("the attacker's tenth attempt waits %v, want 16s", w.Wait())
	} else {
		w.End()
	}
	owner := mustBegin(t, d, "site_1", "203.0.113.7")
	if owner.Wait() != 2*time.Second {
		t.Fatalf("the owner, from another network, waits %v, want the name's 2s cap", owner.Wait())
	}
	owner.Succeed()
	if d.Failures("site_1") != 0 || d.ScopedFailures("site_1", "203.0.113.7") != 0 {
		t.Fatal("the owner's success left the name's failures")
	}
	if n := d.ScopedFailures("site_1", "198.51.100.66"); n != 9 {
		t.Fatalf("the owner's success cleared the attacker's network: %d left, want 9", n)
	}
	if w := mustBegin(t, d, "site_1", "198.51.100.66"); w.Wait() != 16*time.Second {
		t.Fatalf("after the owner's sign-in the attacker waits %v, want still 16s", w.Wait())
	} else {
		w.End()
	}

	// Parallel attempts: each counts those in flight ahead of it.
	var waits []time.Duration
	var open []*Attempt
	for range 4 {
		a := mustBegin(t, d, "site_2", "192.0.2.1")
		waits = append(waits, a.Wait())
		open = append(open, a)
	}
	for range 3 {
		d.Fail("site_2", "192.0.2.1")
	}
	// 3 failures, 4 in flight: the next waits for seven.
	if w := mustBegin(t, d, "site_2", "192.0.2.1"); w.Wait() != 4*time.Second {
		t.Fatalf("with 3 failures and 4 in flight the next waits %v, want 4s", w.Wait())
	} else {
		w.End()
	}
	for _, a := range open {
		a.Fail()
		a.Fail() // one verdict per attempt
		a.Succeed()
	}
	if fmt.Sprint(waits) != "[0s 0s 0s 0s]" || d.ScopedFailures("site_2", "192.0.2.1") != 7 {
		t.Fatalf("waits %v, %d failures", waits, d.ScopedFailures("site_2", "192.0.2.1"))
	}

	// An IPv6 /48 is a scope of its own: a fresh /64 inside it waits the
	// /48's delay.
	for i := range 7 {
		d.Fail("site_3", fmt.Sprintf("2001:db8:1:%x::/64", i), "2001:db8:1::/48")
	}
	if w := mustBegin(t, d, "site_3", "2001:db8:1:ff::/64", "2001:db8:1::/48"); w.Wait() != 4*time.Second {
		t.Fatalf("a fresh /64 of the /48 waits %v, want 4s", w.Wait())
	} else {
		w.End()
	}

	// What a flood leaves is bounded: a key keeps at most keptFailures, and
	// nothing outlives the span.
	for range 10 * keptFailures {
		d.Fail("site_4", "192.0.2.9")
	}
	if n := d.ScopedFailures("site_4", "192.0.2.9"); n != keptFailures {
		t.Fatalf("%d failures kept for one key", n)
	}
	if _, inFlight := d.Size(); inFlight != 0 {
		t.Fatalf("%d keys in flight after every attempt ended", inFlight)
	}
	at = at.Add(16 * time.Minute)
	if failures, _ := d.Size(); failures != 0 {
		t.Fatalf("%d keys kept past the span", failures)
	}
}

// mustBegin begins an attempt that must not be refused.
func mustBegin(t *testing.T, d *Delays, name string, scopes ...string) *Attempt {
	t.Helper()
	a, err := d.Begin(name, scopes...)
	if err != nil {
		t.Fatalf("an attempt for %s from %v was refused: %v", name, scopes, err)
	}
	return a
}

// S-H3 review: parallel attempts from one network are no faster than
// sequential ones. Each one's delay runs from when the one before it on
// the network starts, and one that would wait past the schedule's bound
// is refused, counting nothing; an IPv6 /48 is spaced as a whole, whatever
// /64 each attempt comes from; the name's own delay is not spaced, so the
// owner from another network still waits only its cap.
func TestDelaysAreSpacedPerScope(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	d := NewDelays(15*time.Minute, long, short).WithClock(func() time.Time { return at })
	for range 5 {
		d.Fail("victim", "192.0.2.1")
	}
	// Five failures: 1, 2, 4, 8, 16 s one after the other; the sixth, 32 s
	// more, would start past a minute.
	var waits []time.Duration
	var open []*Attempt
	for range 5 {
		a := mustBegin(t, d, "victim", "192.0.2.1")
		waits = append(waits, a.Wait())
		open = append(open, a)
	}
	if fmt.Sprint(waits) != "[1s 3s 7s 15s 31s]" {
		t.Fatalf("parallel attempts from one network wait %v, want them spaced [1s 3s 7s 15s 31s]", waits)
	}
	if _, err := d.Begin("victim", "192.0.2.1"); err != ErrBacklog {
		t.Fatalf("a sixth parallel attempt = %v, want ErrBacklog", err)
	}
	if _, inFlight := d.Size(); inFlight != 2+1 {
		t.Fatalf("the refused attempt was counted: %d keys in flight", inFlight)
	}
	owner := mustBegin(t, d, "victim", "203.0.113.7")
	if owner.Wait() != 2*time.Second {
		t.Fatalf("the owner from another network waits %v, want the name's 2s cap", owner.Wait())
	}
	owner.End()

	// Ten failures: the next waits 32 s, and one more while it waits would
	// start past a minute.
	at = at.Add(31 * time.Second)
	for _, a := range open {
		a.Fail()
	}
	a := mustBegin(t, d, "victim", "192.0.2.1")
	if a.Wait() != 32*time.Second {
		t.Fatalf("after ten failures the next waits %v, want 32s", a.Wait())
	}
	if _, err := d.Begin("victim", "192.0.2.1"); err != ErrBacklog {
		t.Fatalf("a second attempt while one waits 32s = %v, want ErrBacklog", err)
	}
	a.End()
	if _, inFlight := d.Size(); inFlight != 0 {
		t.Fatalf("%d keys in flight after every attempt ended", inFlight)
	}

	// A /48 at its bound: one attempt from one of its /64s waits a minute;
	// one more, from any other /64, is refused.
	for range 11 {
		d.Fail("site_1", "2001:db8:1:1::/64", "2001:db8:1::/48")
	}
	a = mustBegin(t, d, "site_1", "2001:db8:1:2::/64", "2001:db8:1::/48")
	if a.Wait() != time.Minute {
		t.Fatalf("a fresh /64 of the /48 waits %v, want 1m", a.Wait())
	}
	for i := range 16 {
		if _, err := d.Begin("site_1", fmt.Sprintf("2001:db8:1:%x::/64", 0x100+i), "2001:db8:1::/48"); err != ErrBacklog {
			t.Fatalf("attempt %d from another /64 of the /48 = %v, want ErrBacklog", i, err)
		}
	}
	// Another /48 waits only the name's cap.
	b := mustBegin(t, d, "site_1", "2001:db8:2::/64", "2001:db8:2::/48")
	if b.Wait() != 2*time.Second {
		t.Fatalf("another /48 waits %v, want the name's 2s", b.Wait())
	}
	a.End()
	b.End()
}
