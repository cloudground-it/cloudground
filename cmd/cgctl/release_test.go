package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/version"
)

func TestReleaseVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := version.UpdatePublicKey
	version.UpdatePublicKey = base64.StdEncoding.EncodeToString(pub)
	defer func() { version.UpdatePublicKey = old }()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cgctl"), []byte("binary"), 0o755)
	sum := sha256.Sum256([]byte("binary"))
	body := "version v1.0.0\n" + hex.EncodeToString(sum[:]) + "  cgctl\n"
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		c := releaseCommand(args, &out, &errb)
		return c, out.String(), errb.String()
	}

	if c, _, e := run("verify", dir); c != exitFailed || !strings.Contains(e, "SHA256SUMS") {
		t.Fatalf("an unsigned release: exit %d, %q", c, e)
	}
	os.WriteFile(filepath.Join(dir, version.ManifestName), []byte(body), 0o644)
	os.WriteFile(filepath.Join(dir, version.SignatureName), []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))), 0o644)
	if c, o, e := run("verify", dir, "cgctl"); c != exitOK || o != "v1.0.0: signature and 1 file(s) verified\n" {
		t.Fatalf("a signed release: exit %d, %q %q", c, o, e)
	}
	os.WriteFile(filepath.Join(dir, "cgctl"), []byte("swapped"), 0o755)
	if c, _, _ := run("verify", dir); c != exitFailed {
		t.Fatalf("a swapped binary: exit %d", c)
	}
	for _, args := range [][]string{nil, {"verify"}, {"sign", dir}} {
		if c, _, _ := run(args...); c != exitUsage {
			t.Errorf("release %q: exit %d, want usage", args, c)
		}
	}
}
