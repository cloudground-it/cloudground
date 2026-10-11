package version

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

// releaseKeys are the public halves of the ed25519 keys a release manifest
// may be signed with, base64 as `release-sign keygen` prints them. They are
// source, not configuration: a key enters or leaves the list only in a
// reviewed commit, and a binary trusts exactly the keys it was built with.
//
// At most two keys at a time: the current one, plus the next one during a
// rotation window (SPEC.md §3.2). TestReleaseKeys enforces both rules.
var releaseKeys = []string{
	"KDquuxOG12M2ejsCCSLPKnH70SvmHtiX9MnMB/SbJs4=", // 2026-10-10, the first release key
}

// UpdatePublicKey adds keys at link time, comma separated
// (-ldflags -X .../internal/version.UpdatePublicKey=<key>[,<key>]). It is
// for test builds signed with a throwaway key; a release build leaves it
// empty and trusts releaseKeys alone.
var UpdatePublicKey = ""

// TrustedKeys returns every key this build accepts a release signature from.
// A malformed key is an error rather than skipped: a build that silently
// trusted fewer keys than it lists could not be updated after a rotation.
func TrustedKeys() ([]ed25519.PublicKey, error) {
	encoded := append([]string{}, releaseKeys...)
	encoded = append(encoded, strings.FieldsFunc(UpdatePublicKey, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})...)
	var keys []ed25519.PublicKey
	for _, k := range encoded {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("this build carries a malformed release key %q", k)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// DropCompiledKeysForTest makes this build trust only UpdatePublicKey, for
// the tests of other packages that sign with throwaway keys and would
// otherwise also meet the real release key. It panics outside a test binary.
func DropCompiledKeysForTest() (restore func()) {
	if !testing.Testing() {
		panic("version.DropCompiledKeysForTest outside a test")
	}
	old := releaseKeys
	releaseKeys = nil
	return func() { releaseKeys = old }
}

var errNoOrigin = errors.New("this build has no update origin compiled in")

// CanUpdate reports why this build cannot take a release from its origin,
// or nil when it can: it needs an UpdateURL that CheckOrigin accepts and at
// least one well-formed key.
func CanUpdate() error {
	if UpdateURL == "" {
		return errNoOrigin
	}
	if err := CheckOrigin(UpdateURL); err != nil {
		return err
	}
	keys, err := TrustedKeys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("this build has no release signing key compiled in")
	}
	return nil
}

// CheckOrigin accepts an update origin (SPEC §4): an https URL, or plain
// http to a loopback address (127.0.0.0/8, ::1, localhost), which only a
// process on this machine can answer — the upgrade test serves its feed so.
// Either way the bytes are trusted only once the signed manifest verifies;
// https is defence in depth, not the check. A release build refuses
// anything but https (scripts/build-release.sh).
func CheckOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("the update origin %q is not a plain URL", origin)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("the update origin %q is plain http to another machine: it must be https", origin)
	}
	return fmt.Errorf("the update origin %q must be https", origin)
}
