package install

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/version"
)

// The release check of SPEC §5 (the cases install.sh's release_check was
// held to by internal/version/installer_test.go).
func TestCheckRelease(t *testing.T) {
	for _, tc := range []struct {
		name                string
		manifest, requested string
		allow               bool
		installed           []string
		ok                  bool
		mention             string
	}{
		{"fresh install, latest", "v0.4.0", "latest", false, []string{"", ""}, true, ""},
		{"fresh install, as requested", "v0.4.0", "v0.4.0", false, []string{"", ""}, true, ""},
		{"upgrade", "v0.4.0", "latest", false, []string{"v0.3.0", "v0.3.0"}, true, ""},
		{"reinstall the same release", "v0.4.0", "v0.4.0", false, []string{"v0.4.0", ""}, true, ""},
		{"development build installed", "v0.4.0", "latest", false, []string{"dev", ""}, true, ""},
		{"older than installed", "v0.3.0", "latest", false, []string{"v0.4.0", ""}, false, "downgrade"},
		{"older than installed, even as requested", "v0.3.0", "v0.3.0", false, []string{"v0.4.0", ""}, false, "downgrade"},
		{"older than the newest self-update", "v0.3.0", "latest", false, []string{"dev", "v0.5.0"}, false, "downgrade"},
		{"other than requested", "v0.4.0", "v0.5.0", false, []string{"", ""}, false, "requested"},
		{"unparseable request", "v0.4.0", "tomorrow", false, []string{"", ""}, false, "CLOUDGROUND_VERSION"},
		{"manifest without a version", "", "latest", false, []string{"", ""}, false, "no release version"},
		{"forced rollback", "v0.3.0", "v0.3.0", true, []string{"v0.4.0", "v0.4.0"}, true, ""},
		{"forced rollback still checks the request", "v0.3.0", "v0.2.0", true, []string{"v0.4.0", ""}, false, "requested"},
		{"v0.3 is v0.3.0", "v0.3.0", "v0.3", false, nil, true, ""},
		{"a -N-gHASH suffix is the same release", "v0.4.0-2-gabc123", "v0.4.0", false, []string{"v0.4.0"}, true, ""},
		{"a beta is not the release it leads to", "v0.5.0-beta.1", "v0.5.0", false, nil, false, "requested"},
		{"a beta, as requested", "v0.5.0-beta.2", "v0.5.0-beta.2", false, []string{"v0.4.0"}, true, ""},
		{"a beta is older than its release", "v0.5.0-beta.2", "latest", false, []string{"v0.5.0"}, false, "downgrade"},
	} {
		err := CheckRelease(tc.manifest, tc.requested, tc.allow, tc.installed...)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), tc.mention) {
			t.Errorf("%s: %q does not mention %q", tc.name, err, tc.mention)
		}
	}
}

// release writes a signed release into a directory.
func release(t *testing.T, priv ed25519.PrivateKey, manifestHead string, tamper string) string {
	t.Helper()
	dir := t.TempDir()
	var sums strings.Builder
	sums.WriteString(manifestHead)
	for _, b := range Binaries {
		content := []byte("binary " + b)
		if err := os.WriteFile(filepath.Join(dir, b), content, 0o755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(content), b)
	}
	body := []byte(sums.String())
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte(sig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tamper != "" {
		if err := os.WriteFile(filepath.Join(dir, tamper), []byte("tampered"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestVerifyRelease(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	good := release(t, priv, "version v0.5.0\n", "")
	v, staged, err := VerifyRelease(ReleaseRequest{Dir: good, PublicKey: key, Requested: "latest", StageIn: t.TempDir()})
	if err != nil || v != "v0.5.0" {
		t.Fatalf("a good release: %q, %v", v, err)
	}
	// The verified bytes are staged in a root-only directory; what is
	// installed comes from there, not from the operator's directory.
	if fi, err := os.Stat(staged); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("staged dir: %v %v", fi, err)
	}
	if err := os.WriteFile(filepath.Join(good, "cgctl"), []byte("swapped after the check"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(staged, "cgctl")); string(b) != "binary cgctl" {
		t.Errorf("staged cgctl = %q: a file swapped after verification would be installed", b)
	}
	for _, tc := range []struct {
		name string
		req  ReleaseRequest
		want string
	}{
		{"another key", ReleaseRequest{Dir: release(t, priv, "version v0.5.0\n", ""), PublicKey: base64.StdEncoding.EncodeToString(otherPub)}, "does not verify"},
		{"a tampered binary", ReleaseRequest{Dir: release(t, priv, "version v0.5.0\n", "panel-agent"), PublicKey: key}, "panel-agent does not match"},
		{"two version lines", ReleaseRequest{Dir: release(t, priv, "version v0.5.0\nversion v0.6.0\n", ""), PublicKey: key}, "exactly one version line"},
		{"a downgrade", ReleaseRequest{Dir: release(t, priv, "version v0.5.0\n", ""), PublicKey: key, Installed: []string{"v0.6.0"}}, "refusing to downgrade"},
		{"not the requested one", ReleaseRequest{Dir: release(t, priv, "version v0.5.0\n", ""), PublicKey: key, Requested: "v0.4.0"}, "not the requested"},
	} {
		tc.req.StageIn = t.TempDir()
		if _, _, err := VerifyRelease(tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// A build without a key and no CLOUDGROUND_RELEASE_PUBKEY: refused.
	if _, _, err := VerifyRelease(ReleaseRequest{Dir: release(t, priv, "version v0.5.0\n", ""), StageIn: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no release signing key") {
		t.Errorf("no key: %v", err)
	}
	// A binary the manifest does not list is refused too.
	dir := release(t, priv, "version v0.5.0\n", "")
	body, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	var kept []string
	for _, l := range strings.Split(string(body), "\n") {
		if !strings.HasSuffix(l, " cgctl") {
			kept = append(kept, l)
		}
	}
	nb := []byte(strings.Join(kept, "\n"))
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), nb, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, nb))), 0o644)
	if _, _, err := VerifyRelease(ReleaseRequest{Dir: dir, PublicKey: key, StageIn: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "does not list cgctl") {
		t.Errorf("unlisted binary: %v", err)
	}
}

// The operator's key (CLOUDGROUND_RELEASE_PUBKEY, from out of band) must be
// one of the keys compiled into cgctl when the build has any, since the
// self-updates after this install check with those (SPEC §5); and when it is
// given, only that key may have signed the release.
func TestReleasePubkey(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	a, b := base64.StdEncoding.EncodeToString(pubA), base64.StdEncoding.EncodeToString(pubB)
	old := version.UpdatePublicKey
	t.Cleanup(func() { version.UpdatePublicKey = old })
	for _, tc := range []struct {
		name, given, compiled string
		signer                ed25519.PrivateKey
		err                   string
	}{
		{"given, none compiled", a, "", privA, ""},
		{"compiled only", "", b, privB, ""},
		{"given and compiled", b, b, privB, ""},
		{"given among compiled (rotation)", a, b + "," + a, privA, ""},
		{"given is not compiled", a, b, privA, "not a release key this cgctl was built with"},
		{"given pins the signer", a, b + "," + a, privB, "does not verify"},
		{"no key at all", "", "", privA, "no release signing key"},
		{"a malformed given key", "not-a-key", "", privA, "not a base64 ed25519"},
	} {
		version.UpdatePublicKey = tc.compiled
		_, _, err := VerifyRelease(ReleaseRequest{Dir: release(t, tc.signer, "version v0.5.0\n", ""), PublicKey: tc.given, StageIn: t.TempDir()})
		if (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.err)
		}
	}
}

// Over an installed panel the installer takes only the installed release
// (a repair); another one is `cgctl update`'s, refused here with the way
// to do it. A fresh install, and an install over a development build, take
// any release (R-6.5).
func TestCheckSameRelease(t *testing.T) {
	for _, c := range []struct {
		upgrade             bool
		installed, manifest string
		ok                  bool
	}{
		{true, "v0.2.0", "v0.2.0", true},
		{true, "v0.2.0", "v0.3.0", false},
		{true, "v0.3.0", "v0.2.0", false},
		{true, "v0.2.0", "v0.2.1-beta.1", false},
		{false, "v0.2.0", "v0.3.0", true},
		{true, "", "v0.3.0", true},
		{true, "dev", "v0.3.0", true},
		{true, "e9f4780", "v0.3.0", true},
		// A development build stamped by git describe after a tag is not
		// that release: the R-6.5 upgrade installs v0.1.0 over it.
		{true, "v0.1.0-29-gbe3432f", "v0.1.0", true},
		{true, "v0.1.0-29-gbe3432f-dirty", "v0.2.0", true},
		{true, "v0.2.0-beta.1", "v0.2.0", false},
	} {
		err := CheckSameRelease(c.upgrade, c.installed, c.manifest)
		if (err == nil) != c.ok {
			t.Errorf("CheckSameRelease(%v, %q, %q) = %v, want ok=%v", c.upgrade, c.installed, c.manifest, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "cgctl update --to "+c.manifest) {
			t.Errorf("the refusal does not point to cgctl update: %v", err)
		}
	}
}
