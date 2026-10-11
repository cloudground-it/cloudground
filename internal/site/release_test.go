package site

import (
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

// A new release ID is a ULID: 26 Crockford base32 digits, the first ten
// its time in milliseconds.
func TestNewReleaseIDIsAULID(t *testing.T) {
	resetULIDs()
	at := time.UnixMilli(1_791_000_000_123) // 2026-10-03
	id := NewReleaseID(at)
	if len(id) != 26 || !ValidReleaseID(id) {
		t.Fatalf("%q is not a canonical ULID", id)
	}
	if ms, form := releaseKey(id); ms != at.UnixMilli() || form != 2 {
		t.Fatalf("%q carries %d (form %d), want %d", id, ms, form, at.UnixMilli())
	}
	// The epoch and the largest 48-bit time encode at the edges.
	if got := encodeULID(0, [10]byte{}); got != "00000000000000000000000000" {
		t.Fatalf("zero ULID %q", got)
	}
	var ones [10]byte
	for i := range ones {
		ones[i] = 0xff
	}
	if got := encodeULID(1<<48-1, ones); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" || !ValidReleaseID(got) {
		t.Fatalf("largest ULID %q", got)
	}
}

// resetULIDs forgets the last ID made, so a test can make IDs of a time
// before it.
func resetULIDs() {
	ulids.Lock()
	ulids.ms = 0
	ulids.Unlock()
}

// IDs made in the same millisecond, from many goroutines, or after the
// clock went back, are distinct and increase in the order they were made.
func TestReleaseIDsAreMonotonic(t *testing.T) {
	resetULIDs()
	at := time.UnixMilli(1_791_000_100_000)
	var prev string
	for i := range 1000 {
		when := at
		if i%3 == 2 {
			when = at.Add(-time.Second) // the clock stepped back
		}
		id := NewReleaseID(when)
		if prev != "" && !(prev < id) || prev != "" && !ReleaseBefore(prev, id) {
			t.Fatalf("id %d %q does not follow %q", i, id, prev)
		}
		prev = id
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				id := NewReleaseID(at)
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate id %q", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

// The random part's carry runs across every byte, and a full random part
// moves to the next millisecond rather than wrapping.
func TestReleaseIDCarry(t *testing.T) {
	b := []byte{0x00, 0xff, 0xff}
	if !increment(b) || !slices.Equal(b, []byte{0x01, 0x00, 0x00}) {
		t.Fatalf("carry: %x", b)
	}
	b = []byte{0xff, 0xff}
	if increment(b) {
		t.Fatal("a full number did not report the wrap")
	}

	ulids.Lock()
	ulids.ms = 1_791_000_200_000
	for i := range ulids.rand {
		ulids.rand[i] = 0xff
	}
	ulids.Unlock()
	id := NewReleaseID(time.UnixMilli(1_791_000_200_000))
	if ms, _ := releaseKey(id); ms != 1_791_000_200_001 {
		t.Fatalf("after a full millisecond the id carries %d", ms)
	}
}

func TestValidReleaseID(t *testing.T) {
	for _, id := range []string{
		NewReleaseID(time.Now()), "01JA2B3C4D5E6F7G8H9JKMNPQR", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ",
		"20260711-120000", "20261009-161248.123-9f2c1a",
	} {
		if !ValidReleaseID(id) {
			t.Errorf("%q is refused", id)
		}
	}
	for _, id := range []string{
		"", InitialRelease, "../x", "20260711-120000/x", "20260711-1200", "20260711-120000.12-abcdef",
		"20260711-120000.123-ABCDEF", "20260711-120000.123-abcdef\n", ".build-20260711-120000",
		"01ja2b3c4d5e6f7g8h9jkmnpqr", // lower case: not the canonical name
		"81JA2B3C4D5E6F7G8H9JKMNPQR", // above 48 bits of time
		"01JA2B3C4D5E6F7G8H9JKMNPQ",  // 25 digits
		"01JA2B3C4D5E6F7G8H9JKMNPQRS",
		"01JA2B3C4D5E6F7G8H9JKMNPQI", "01JA2B3C4D5E6F7G8H9JKMNPQL", "01JA2B3C4D5E6F7G8H9JKMNPQO", "01JA2B3C4D5E6F7G8H9JKMNPQU",
		".build-01JA2B3C4D5E6F7G8H9JKMNPQR",
	} {
		if ValidReleaseID(id) {
			t.Errorf("%q is accepted", id)
		}
	}
}

// Releases order by age whatever the form of their names: initial first,
// a second-resolution name before the millisecond ones of its second, an
// older-form name before a ULID of the same millisecond, ULIDs by time.
func TestReleaseOrderAcrossForms(t *testing.T) {
	sec := time.Date(2026, 10, 9, 16, 12, 48, 0, time.UTC)
	u := func(d time.Duration) string { return encodeULID(uint64(sec.Add(d).UnixMilli()), [10]byte{9}) }
	want := []string{
		InitialRelease,
		u(-time.Hour),
		"20261009-161248",
		"20261009-161248.000-00aa00",
		u(0),
		"20261009-161248.123-9f2c1a",
		u(124 * time.Millisecond),
		"20261009-161249",
		u(time.Second + time.Millisecond),
	}
	got := slices.Clone(want)
	slices.Reverse(got)
	sort.Slice(got, func(i, j int) bool { return ReleaseBefore(got[i], got[j]) })
	if !slices.Equal(got, want) {
		t.Fatalf("order\n got %q\nwant %q", got, want)
	}
	for i := range want {
		if ReleaseBefore(want[i], want[i]) {
			t.Fatalf("%q is before itself", want[i])
		}
	}
}
