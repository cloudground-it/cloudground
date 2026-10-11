package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/version"
)

// Binaries are what the installer puts in BinDir.
var Binaries = []string{"panel-api", "panel-agent", "cgctl"}

// OptionalBinaries are installed when the release has them (a release made
// before one existed has not): cg-site-shell, the gate client (R-10.5),
// which panel-agent stands in for, by argv[0], until it is there.
var OptionalBinaries = []string{"cg-site-shell"}

// BinDir is where the binaries and the pinned tools go.
const BinDir = "/usr/local/bin"

// HighestReleaseFile is the newest release the agent has self-updated to
// (internal/agent/ops/panel): a floor a downloaded install must not go below.
const HighestReleaseFile = "/var/lib/cloudground-agent/highest-release"

// ReleaseRequest is what the operator asked for (SPEC §5).
type ReleaseRequest struct {
	Dir string // the downloaded release
	// PublicKey is CLOUDGROUND_RELEASE_PUBKEY (base64 ed25519), the key
	// the operator got out of band; "" = the key compiled into this cgctl.
	PublicKey      string
	Requested      string // CLOUDGROUND_VERSION ("" or "latest" = any)
	AllowDowngrade bool   // CLOUDGROUND_ALLOW_DOWNGRADE=1
	Installed      []string
	// StageIn is where the verified copy is made ("" = the system's
	// temporary directory).
	StageIn string
}

// verifyManifest is the release's one signature check,
// version.VerifyManifestPinned: with CLOUDGROUND_RELEASE_PUBKEY (given) only
// that key may have signed, and it must be one of the keys compiled into this
// cgctl when it has any (the self-updates that follow check with those);
// without it, any compiled key.
func verifyManifest(body, sig []byte, given string) (version.Manifest, error) {
	m, err := version.VerifyManifestPinned(body, sig, given)
	switch {
	case errors.Is(err, version.ErrKeyNotTrusted):
		return m, fmt.Errorf("CLOUDGROUND_RELEASE_PUBKEY is not a release key this cgctl was built with: refusing to install")
	case errors.Is(err, version.ErrNoTrustedKey):
		return m, fmt.Errorf("no release signing key: this cgctl was built without one; set CLOUDGROUND_RELEASE_PUBKEY, or install a local build with CLOUDGROUND_LOCAL_BUILD")
	}
	return m, err
}

// VerifyRelease checks a downloaded release before anything of it is
// installed: the signature (version.VerifyManifest), one version line, the
// release check of SPEC §5, and each binary against the manifest. Every file
// is read once, and the bytes verified are the bytes written to a new
// root-only directory, which is returned with the manifest's version: what is
// installed is what was checked, whatever happens to Dir meanwhile. The
// caller removes the staged directory.
func VerifyRelease(r ReleaseRequest) (ver, staged string, err error) {
	read := func(name string) ([]byte, error) {
		f, err := os.OpenFile(filepath.Join(r.Dir, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, 512<<20))
	}
	body, err := read(version.ManifestName)
	if err != nil {
		return "", "", err
	}
	sig, err := read(version.SignatureName)
	if err != nil {
		return "", "", err
	}
	if n := bytes.Count(append([]byte("\n"), body...), []byte("\nversion ")); n != 1 {
		return "", "", fmt.Errorf("the signed manifest must carry exactly one version line")
	}
	m, err := verifyManifest(body, sig, r.PublicKey)
	if err != nil {
		return "", "", err
	}
	if err := CheckRelease(m.Version, r.Requested, r.AllowDowngrade, r.Installed...); err != nil {
		return "", "", err
	}
	bins := map[string][]byte{}
	for _, name := range Binaries {
		want, ok := m.Sums[name]
		if !ok {
			return "", "", fmt.Errorf("the signed manifest does not list %s", name)
		}
		b, err := read(name)
		if err != nil {
			return "", "", err
		}
		if sha256Hex(b) != want {
			return "", "", fmt.Errorf("%s does not match the signed manifest", name)
		}
		bins[name] = b
	}
	for _, name := range OptionalBinaries {
		want, ok := m.Sums[name]
		if !ok {
			continue
		}
		b, err := read(name)
		if err != nil {
			return "", "", err
		}
		if sha256Hex(b) != want {
			return "", "", fmt.Errorf("%s does not match the signed manifest", name)
		}
		bins[name] = b
	}
	staged, err = os.MkdirTemp(r.StageIn, "cloudground-release-")
	if err != nil {
		return "", "", err
	}
	for name, b := range bins {
		if err := os.WriteFile(filepath.Join(staged, name), b, 0o700); err != nil {
			_ = os.RemoveAll(staged)
			return "", "", err
		}
	}
	return m.Version, staged, nil
}

// CheckRelease decides whether release manifest may be installed: it must
// name a release; when requested is a version, be that one; and not be older
// than any installed version that parses, unless allowDowngrade. The same
// release may be reinstalled. (The self-update's version.CheckUpdate is
// stricter: it wants a strictly newer release.)
func CheckRelease(manifest, requested string, allowDowngrade bool, installed ...string) error {
	if _, ok := version.Parse(manifest); !ok {
		return fmt.Errorf("the signed manifest names no release version")
	}
	if requested = strings.TrimSpace(requested); requested != "" && requested != "latest" {
		if _, ok := version.Parse(requested); !ok {
			return fmt.Errorf("CLOUDGROUND_VERSION=%s is not a vX.Y.Z version", requested)
		}
		if c, _ := version.Compare(manifest, requested); c != 0 {
			return fmt.Errorf("the signed manifest is for %s, not the requested %s: refusing to install", manifest, requested)
		}
	}
	for _, have := range installed {
		if _, ok := version.Parse(have); !ok {
			continue
		}
		if version.Newer(have, manifest) && !allowDowngrade {
			return fmt.Errorf("release %s is older than %s, already installed: refusing to downgrade (set CLOUDGROUND_ALLOW_DOWNGRADE=1 to force it)", manifest, have)
		}
	}
	return nil
}

// CheckSameRelease refuses a downloaded release that would change the
// version of an installed panel (SPEC §5 step 3a, R-6.5): the installer has
// no snapshot and no rollback, so moving between releases is `cgctl
// update`'s. The same release is a repair and goes ahead; so does an install
// over a build whose version is not a release tag (a development build,
// whether "dev", a commit or `git describe`'s v0.1.0-29-gbe3432f).
func CheckSameRelease(upgrade bool, installed, manifest string) error {
	if !upgrade {
		return nil
	}
	if _, ok := version.ReleaseTag(installed); !ok {
		return nil
	}
	if manifest == installed {
		return nil
	}
	return fmt.Errorf("CloudGround %s is installed here, and this is the installer of %s: the installer only installs or repairs the "+
		"release that is installed, without a snapshot or a rollback. To move to %s, run `cgctl update --to %s` "+
		"(it snapshots the panel's state first and rolls back a release that does not come up); "+
		"to repair this server, run the installer of %s (CLOUDGROUND_VERSION=%s)", installed, manifest, manifest, manifest, installed, installed)
}

// binariesIn is what the installer puts in BinDir from the build in dir:
// every one of Binaries, and those of OptionalBinaries that dir has.
func binariesIn(dir string) []string {
	out := append([]string{}, Binaries...)
	for _, b := range OptionalBinaries {
		if fi, err := os.Lstat(filepath.Join(dir, b)); err == nil && fi.Mode().IsRegular() {
			out = append(out, b)
		}
	}
	return out
}
