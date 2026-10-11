package version

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// UpdateURL is the repository releases come from
// (https://github.com/<owner>/<repo> in a release build; Feed, SPEC §4), and
// UpdatePublicKey (keys.go) a link-time addition to the keys a release must
// be signed with. Both are fixed when the binary is linked and deliberately
// not configurable at runtime: they decide which bytes become root binaries,
// so neither an API request nor a panel setting may change them.
var UpdateURL = ""

// Manifest is a signed release description: SHA256SUMS in the familiar
// `sha256sum` layout, preceded by a "version <tag>" line, with a detached
// ed25519 signature over the exact bytes in SHA256SUMS.sig.
type Manifest struct {
	Version string
	Sums    map[string]string // file name -> lowercase hex sha256
}

// ManifestName and SignatureName are the file names published beside the
// binaries.
const (
	ManifestName  = "SHA256SUMS"
	SignatureName = "SHA256SUMS.sig"
)

// VerifyManifest is the one signature check of a release (SPEC §3): sig
// (base64) must be an ed25519 signature over the exact bytes of body by one
// of the keys this build trusts (TrustedKeys). The verified body is then
// parsed. It refuses rather than guesses: no trusted key, a bad signature or
// a malformed line all fail. The installer, the self-update, the release
// check and `cgctl release verify` (VerifyRelease) all end here; nothing
// else in the tree checks a release signature.
func VerifyManifest(body, sig []byte) (Manifest, error) {
	keys, err := TrustedKeys()
	if err != nil {
		return Manifest{}, err
	}
	return verifyWith(body, sig, keys)
}

// VerifyManifestPinned is VerifyManifest narrowed to one key, pin (base64),
// that the operator got out of band (the installer's
// CLOUDGROUND_RELEASE_PUBKEY, internal/install SPEC §5): only pin may have
// signed body. A build that trusts keys of its own trusts pin only if it is
// one of them (ErrKeyNotTrusted), since the self-updates that follow check
// with those keys; a build with none takes pin alone. An empty pin is
// VerifyManifest.
func VerifyManifestPinned(body, sig []byte, pin string) (Manifest, error) {
	if pin = strings.TrimSpace(pin); pin == "" {
		return VerifyManifest(body, sig)
	}
	raw, err := base64.StdEncoding.DecodeString(pin)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return Manifest{}, fmt.Errorf("release key %q is not a base64 ed25519 public key", pin)
	}
	key := ed25519.PublicKey(raw)
	trusted, err := TrustedKeys()
	if err != nil {
		return Manifest{}, err
	}
	if len(trusted) > 0 && !slices.ContainsFunc(trusted, func(k ed25519.PublicKey) bool { return k.Equal(key) }) {
		return Manifest{}, ErrKeyNotTrusted
	}
	return verifyWith(body, sig, []ed25519.PublicKey{key})
}

// ErrNoTrustedKey: the build has no release key to check a signature with.
// ErrKeyNotTrusted: VerifyManifestPinned was given a key the build does not
// trust.
var (
	ErrNoTrustedKey  = errors.New("no valid update signing key in this build")
	ErrKeyNotTrusted = errors.New("the given release key is not one this build trusts")
)

func verifyWith(body, sig []byte, keys []ed25519.PublicKey) (Manifest, error) {
	if len(keys) == 0 {
		return Manifest{}, ErrNoTrustedKey
	}
	rawSig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(rawSig) != ed25519.SignatureSize {
		return Manifest{}, fmt.Errorf("malformed release signature")
	}
	for _, k := range keys {
		if ed25519.Verify(k, body, rawSig) {
			return ParseManifest(body)
		}
	}
	return Manifest{}, fmt.Errorf("release signature does not verify: refusing to install")
}

// ParseManifest reads the manifest layout without checking any signature.
func ParseManifest(body []byte) (Manifest, error) {
	m := Manifest{Sums: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if v, ok := strings.CutPrefix(line, "version "); ok {
			m.Version = strings.TrimSpace(v)
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return Manifest{}, fmt.Errorf("malformed manifest line %q", line)
		}
		sum, name := strings.ToLower(f[0]), strings.TrimPrefix(f[1], "*")
		if _, err := hex.DecodeString(sum); err != nil || len(sum) != 64 {
			return Manifest{}, fmt.Errorf("malformed checksum for %s", name)
		}
		if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return Manifest{}, fmt.Errorf("manifest names a path, not a file: %q", name)
		}
		m.Sums[name] = sum
	}
	if err := sc.Err(); err != nil {
		return Manifest{}, err
	}
	if _, ok := Parse(m.Version); !ok {
		return Manifest{}, fmt.Errorf("manifest has no release version")
	}
	return m, nil
}

// Parse reads a vX.Y.Z tag, ignoring any suffix after a "-" (a beta's
// -beta.N, the -N-gHASH of a build between tags). It reports false for a
// development build ("dev", a bare commit hash) or anything unparseable.
func Parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return out, false
	}
	core, _, _ := strings.Cut(v, "-")
	fields := strings.Split(core, ".")
	if len(fields) < 2 || len(fields) > 3 {
		return out, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether candidate is a strictly later release than current.
// Both must parse. A beta comes before the release it leads to
// (v1.2.0-beta.1 < v1.2.0-beta.2 < v1.2.0); a -N-gHASH suffix orders as the
// tag it follows (Compare).
func Newer(candidate, current string) bool {
	c, ok := Compare(candidate, current)
	return ok && c > 0
}

// CheckUpdate decides whether a verified manifest for release manifest may
// replace the running release installed, when the operator asked for
// requested. It refuses:
//
//   - a manifest that names no release;
//   - a manifest for another release than the one requested (requested may be
//     "" or "latest" to take whatever the origin publishes), so an operator
//     who approved one release is never handed another;
//   - a release that is not strictly newer than installed, so an old, validly
//     signed release with known bugs cannot be pushed back on. A development
//     build (installed does not parse) cannot order itself and is not checked
//     here; the caller keeps its own floor for that case.
//
// The installer's install.CheckRelease (internal/install) makes the same
// checks for a downloaded install, but allows reinstalling the same release.
func CheckUpdate(manifest, requested, installed string) error {
	if _, ok := Parse(manifest); !ok {
		return fmt.Errorf("signed manifest names no release version")
	}
	if requested = strings.TrimSpace(requested); requested != "" && requested != "latest" {
		if _, ok := Parse(requested); !ok {
			return fmt.Errorf("requested release %q is not a vX.Y.Z version", requested)
		}
		if c, _ := Compare(manifest, requested); c != 0 {
			return fmt.Errorf("signed manifest is for %s, not the requested %s: refusing to install", manifest, requested)
		}
	}
	if _, ok := Parse(installed); ok && !Newer(manifest, installed) {
		return fmt.Errorf("release %s is not newer than the running %s: refusing to downgrade", manifest, installed)
	}
	return nil
}
