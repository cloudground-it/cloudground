package throttle

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	w := New(3, time.Minute).WithClock(func() time.Time { return at })

	for i := range 3 {
		if !w.Allow("k") {
			t.Fatalf("event %d refused below the limit", i+1)
		}
	}
	if w.Allow("k") || !w.Blocked("k") {
		t.Fatal("a fourth event in the window was admitted")
	}
	if w.Count("k") != 3 || w.Count("none") != 0 {
		t.Fatalf("count = %d", w.Count("k"))
	}
	if !w.Allow("other") || w.Blocked("other") {
		t.Fatal("one key's count spilled onto another")
	}
	// Blocked records nothing.
	for range 10 {
		w.Blocked("quiet")
	}
	if !w.Allow("quiet") {
		t.Fatal("Blocked counted as events")
	}

	// The window slides: an event leaves it a full span after it happened.
	at = at.Add(59 * time.Second)
	if w.Allow("k") {
		t.Fatal("admitted before the span ended")
	}
	at = at.Add(2 * time.Second)
	if !w.Allow("k") {
		t.Fatal("still refused after the span")
	}

	w.Reset("k")
	for range 3 {
		if !w.Allow("k") {
			t.Fatal("refused after a reset")
		}
	}

	// Keys with nothing left in the window are forgotten.
	at = at.Add(time.Hour)
	w.Allow("fresh")
	if len(w.seen) != 1 {
		t.Fatalf("%d keys kept, want only the fresh one", len(w.seen))
	}
}
