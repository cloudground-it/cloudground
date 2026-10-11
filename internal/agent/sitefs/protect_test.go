package sitefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A protected view refuses the named entries of its own directory, also
// through a view made with WithOwner, and also when the path reaches that
// directory through an in-tree link; other entries change as before.
func TestProtectRefusesNamedEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(dir, "shared", "up")); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, Owner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := r.Protect("current", "shared")
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"mkdir":         p.Mkdir("current", 0o755),
		"mkdir via up":  p.Mkdir("shared/up/current", 0o755),
		"replace":       p.WithOwner(Owner{}).WriteFile("current", []byte("x"), 0o644),
		"remove":        p.RemoveAll("shared"),
		"rename from":   p.Rename("shared", "s2", NoReplace),
		"rename onto":   p.Rename("shared/up/shared", "shared/x", NoReplace),
		"chmod":         p.Chmod("shared", 0o700),
		"symlink":       p.Symlink("shared", "current"),
		"chown tree":    p.ChownTree("shared"),
		"create":        func() error { _, err := p.Create("current", 0o644); return err }(),
		"open to write": func() error { _, err := p.OpenFile("current", os.O_CREATE|os.O_WRONLY, 0o644); return err }(),
	} {
		if !errors.Is(err, ErrProtected) {
			t.Errorf("%s: %v, want ErrProtected", name, err)
		}
	}
	if err := p.WriteFile("shared/ok.txt", []byte("ok"), 0o644); err != nil {
		t.Errorf("an entry inside a protected folder: %v", err)
	}
	if err := p.Mkdir("other", 0o755); err != nil {
		t.Errorf("an unprotected name: %v", err)
	}
	// The unprotected Root is unchanged.
	if err := r.Mkdir("current", 0o755); err != nil {
		t.Errorf("the Root itself: %v", err)
	}
}
