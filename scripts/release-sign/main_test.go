package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/version"
)

// What sign writes is what VerifyRelease accepts with the key keygen
// printed, covering every file of the directory.
func TestSignIsVerifiedByVersion(t *testing.T) {
	tmp := t.TempDir()
	keyFile := filepath.Join(tmp, "release.key")
	var pub bytes.Buffer
	if err := run([]string{"keygen", keyFile}, &pub); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(keyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	if err := run([]string{"keygen", keyFile}, &bytes.Buffer{}); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	var again bytes.Buffer
	if err := run([]string{"pubkey", keyFile}, &again); err != nil || again.String() != pub.String() {
		t.Fatalf("pubkey = %q, keygen printed %q (%v)", again.String(), pub.String(), err)
	}

	dir := filepath.Join(tmp, "rel")
	os.Mkdir(dir, 0o755)
	for _, n := range []string{"panel-api", "cgctl", "cloudground-v1.2.0-linux-amd64.tar.gz"} {
		os.WriteFile(filepath.Join(dir, n), []byte(n+" bytes"), 0o644)
	}
	if err := run([]string{"sign", keyFile, "v1.2", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("signed a tag that is not a release tag")
	}
	if err := run([]string{"sign", keyFile, "v1.2.0", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, version.ManifestName))
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 4 || lines[0] != "version v1.2.0" || !strings.HasSuffix(lines[1], "  cgctl") {
		t.Fatalf("manifest:\n%s", body)
	}

	old := version.UpdatePublicKey
	version.UpdatePublicKey = strings.TrimSpace(pub.String())
	defer func() { version.UpdatePublicKey = old }()
	if m, err := version.VerifyRelease(dir); err != nil || m.Version != "v1.2.0" || len(m.Sums) != 3 {
		t.Fatalf("VerifyRelease: %+v %v", m, err)
	}
	// Signing again (a second run of the job) gives the same manifest.
	if err := run([]string{"sign", keyFile, "v1.2.0", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, version.ManifestName)); !bytes.Equal(b, body) {
		t.Fatal("re-signing listed the old manifest or signature")
	}

	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	if err := run([]string{"sign", keyFile, "v1.2.0", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("signed a directory with a subdirectory in it")
	}
	if err := run([]string{"sign", keyFile, "v1.2.0", t.TempDir()}, &bytes.Buffer{}); err == nil {
		t.Fatal("signed an empty directory")
	}
	os.WriteFile(filepath.Join(tmp, "bad.key"), []byte("nope"), 0o600)
	if err := run([]string{"sign", filepath.Join(tmp, "bad.key"), "v1.2.0", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("signed with something that is not a key")
	}
}

func TestChannel(t *testing.T) {
	for tag, want := range map[string]string{"v1.0.0": "stable\n", "v1.0.0-beta.3": "beta\n", "v1.0": "", "main": ""} {
		var out bytes.Buffer
		err := run([]string{"channel", tag}, &out)
		if out.String() != want || (err == nil) != (want != "") {
			t.Errorf("channel %q = %q %v", tag, out.String(), err)
		}
	}
	if err := run(nil, &bytes.Buffer{}); err != errUsage {
		t.Errorf("no arguments: %v", err)
	}
}
