package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Release channels (SPEC §4.1). A server follows one, chosen in the panel's
// settings: stable takes only vX.Y.Z tags, beta takes those and the
// pre-releases vX.Y.Z-beta.N.
const (
	Stable = "stable"
	Beta   = "beta"
)

// releaseTagRe is the exact shape of a tag the release workflow publishes.
var releaseTagRe = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-beta\.([1-9][0-9]*))?$`)

// ReleaseTag reports the channel a release tag belongs to, and false for a
// string that is not a release tag at all. It is strict where Parse is
// lenient: no missing "v", no leading zeros, no third-party suffix. The
// signing tool refuses to sign anything else.
func ReleaseTag(tag string) (channel string, ok bool) {
	m := releaseTagRe.FindStringSubmatch(tag)
	switch {
	case m == nil:
		return "", false
	case m[4] != "":
		return Beta, true
	default:
		return Stable, true
	}
}

// ParseChannel accepts a channel name, "" meaning stable.
func ParseChannel(s string) (string, bool) {
	switch strings.TrimSpace(s) {
	case "", Stable:
		return Stable, true
	case Beta:
		return Beta, true
	}
	return "", false
}

// CheckChannel refuses a release tag the channel does not take: anything but
// a release tag, and a beta on the stable channel. It runs on the version
// line of the verified manifest, so a feed that names a beta to a stable
// server is caught after the signature check as well as before it.
func CheckChannel(channel, tag string) error {
	ch, ok := ParseChannel(channel)
	if !ok {
		return fmt.Errorf("unknown release channel %q", channel)
	}
	got, ok := ReleaseTag(tag)
	if !ok {
		return fmt.Errorf("%q is not a release tag", tag)
	}
	if ch == Stable && got == Beta {
		return fmt.Errorf("%s is a beta release and this server follows the stable channel", tag)
	}
	return nil
}

// Compare orders two versions that Parse accepts: -1, 0 or 1, and false when
// either does not parse. Between equal X.Y.Z, a -beta.N comes before the
// release and betas order by N; any other suffix (the -N-gHASH of a build
// between tags) is ignored.
func Compare(a, b string) (int, bool) {
	ca, okA := Parse(a)
	cb, okB := Parse(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range ca {
		if ca[i] != cb[i] {
			return sign(ca[i] - cb[i]), true
		}
	}
	return sign(betaRank(a) - betaRank(b)), true
}

// betaRank is N for a -beta.N version, and above every N for anything else
// (a release, or a build described from one).
func betaRank(v string) int {
	_, suffix, _ := strings.Cut(strings.TrimSpace(v), "-")
	n, ok := strings.CutPrefix(suffix, "beta.")
	if !ok {
		return int(^uint(0) >> 1)
	}
	n, _, _ = strings.Cut(n, "-")
	r, err := strconv.Atoi(n)
	if err != nil || r < 0 {
		return int(^uint(0) >> 1)
	}
	return r
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
