package site

import (
	"crypto/rand"
	"regexp"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// InitialRelease is the release provisioning creates.
const InitialRelease = state.InitialRelease

// A release made by a deploy or a migration is named by a ULID (R-4.3): 26
// characters of Crockford base32, the first ten the creation time in
// milliseconds since the Unix epoch, the other sixteen 80 random bits. The
// names releases had before stay valid, so a site deployed before keeps
// its releases: "20261009-181248.123-9f2c1a" (the UTC time to the
// millisecond and 24 random bits) and "20261009-181248" (to the second).
var (
	ulidRe        = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)
	legacyRe      = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)
	legacyMilliRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}\.[0-9]{3}-[0-9a-f]{6}$`)
)

// crockford is the ULID alphabet: no I, L, O or U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulids hands out IDs that increase strictly, also within a millisecond
// and when the clock steps back.
var ulids struct {
	sync.Mutex
	ms   uint64
	rand [10]byte
}

// NewReleaseID names a new release created at now: a ULID. IDs made by
// this process increase strictly: within one millisecond (or after the
// clock went back) the random part of the previous ID is incremented by
// one instead of drawn again.
func NewReleaseID(now time.Time) string {
	ms := uint64(max(now.UnixMilli(), 0)) & (1<<48 - 1)
	ulids.Lock()
	defer ulids.Unlock()
	if ms > ulids.ms {
		ulids.ms = ms
		_, _ = rand.Read(ulids.rand[:]) // crypto/rand.Read does not fail
	} else if !increment(ulids.rand[:]) {
		// 2^80 IDs in one millisecond: move to the next one.
		ulids.ms++
		_, _ = rand.Read(ulids.rand[:])
	}
	return encodeULID(ulids.ms, ulids.rand)
}

// increment adds one to a big-endian number; false when it wrapped.
func increment(b []byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return true
		}
	}
	return false
}

// encodeULID writes the 48-bit time and the 80 random bits as 10 and 16
// base32 digits, most significant first.
func encodeULID(ms uint64, r [10]byte) string {
	var out [26]byte
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&31]
		ms >>= 5
	}
	// 80 bits are exactly 16 digits: two 40-bit halves of 8 digits each.
	for half := range 2 {
		var v uint64
		for _, c := range r[half*5 : half*5+5] {
			v = v<<8 | uint64(c)
		}
		for i := 7; i >= 0; i-- {
			out[10+half*8+i] = crockford[v&31]
			v >>= 5
		}
	}
	return string(out[:])
}

// ValidReleaseID reports whether id names a release a deploy or migration
// made: a ULID in its canonical upper-case form, or one of the two older
// forms. InitialRelease is not one.
func ValidReleaseID(id string) bool {
	return ulidRe.MatchString(id) || legacyMilliRe.MatchString(id) || legacyRe.MatchString(id)
}

// releaseKey orders a release by when it was made: its time in
// milliseconds, then its form (a second-resolution name before the
// millisecond names of its second, the older forms before a ULID of the
// same millisecond), then the name itself.
func releaseKey(id string) (ms int64, form int) {
	switch {
	case ulidRe.MatchString(id):
		for i := range 10 {
			ms = ms<<5 | int64(indexCrockford(id[i]))
		}
		return ms, 2
	case legacyMilliRe.MatchString(id):
		if t, err := time.Parse("20060102-150405.000", id[:19]); err == nil {
			return t.UnixMilli(), 1
		}
	case legacyRe.MatchString(id):
		if t, err := time.Parse("20060102-150405", id); err == nil {
			return t.UnixMilli(), 0
		}
	}
	return -1, -1
}

func indexCrockford(c byte) int {
	for i := range len(crockford) {
		if crockford[i] == c {
			return i
		}
	}
	return 0
}

// ReleaseBefore orders release names by age: InitialRelease is the oldest,
// then the deployed ones by the time their name carries, whatever its form.
// (The older forms sorted by their text, which a ULID does not share: a
// ULID starts with a digit 0–7 and would sort before every older name.)
func ReleaseBefore(a, b string) bool {
	switch {
	case a == b:
		return false
	case a == InitialRelease:
		return true
	case b == InitialRelease:
		return false
	}
	am, af := releaseKey(a)
	bm, bf := releaseKey(b)
	if am != bm {
		return am < bm
	}
	if af != bf {
		return af < bf
	}
	return a < b
}
