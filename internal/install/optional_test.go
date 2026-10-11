package install

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// cg-site-shell is installed when the release has it, and a release made
// before it existed still installs (R-10.5).
func TestOptionalBinaries(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	without := release(t, priv, "version v0.5.0\n", "")
	if got := binariesIn(without); !slices.Equal(got, Binaries) {
		t.Errorf("without the gate: %v", got)
	}
	if _, staged, err := VerifyRelease(ReleaseRequest{Dir: without, PublicKey: key, StageIn: t.TempDir()}); err != nil {
		t.Fatalf("a release without cg-site-shell: %v", err)
	} else if _, err := os.Stat(filepath.Join(staged, "cg-site-shell")); !os.IsNotExist(err) {
		t.Errorf("staged a gate the release has not: %v", err)
	}

	// With it: listed, verified, staged.
	dir := t.TempDir()
	head := "version v0.6.0\n"
	sums := head
	for _, b := range append(append([]string{}, Binaries...), OptionalBinaries...) {
		content := []byte("binary " + b)
		if err := os.WriteFile(filepath.Join(dir, b), content, 0o755); err != nil {
			t.Fatal(err)
		}
		sums += fmt.Sprintf("%s  %s\n", sha256Hex(content), b)
	}
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(sums)))), 0o644)
	if got := binariesIn(dir); !slices.Contains(got, "cg-site-shell") {
		t.Errorf("with the gate: %v", got)
	}
	_, staged, err := VerifyRelease(ReleaseRequest{Dir: dir, PublicKey: key, StageIn: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(staged, "cg-site-shell")); string(b) != "binary cg-site-shell" {
		t.Errorf("staged gate %q", b)
	}
	// Tampered: refused.
	_ = os.WriteFile(filepath.Join(dir, "cg-site-shell"), []byte("tampered"), 0o755)
	if _, _, err := VerifyRelease(ReleaseRequest{Dir: dir, PublicKey: key, StageIn: t.TempDir()}); err == nil {
		t.Error("a tampered cg-site-shell was accepted")
	}
}
