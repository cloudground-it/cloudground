package sitefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// SubStrict refuses a symlink anywhere on the path, even one that stays in
// the tree; a link its Root makes may climb out of it as long as it stays
// in the tree it came from.
func TestSubStrict(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"a/b", "shared"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(dir, "a", "lb")); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, Owner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, p := range []string{"l", "l/b", "a/lb"} {
		if _, err := r.SubStrict(p); !errors.Is(err, ErrSymlink) {
			t.Errorf("SubStrict(%q) = %v, want ErrSymlink", p, err)
		}
	}
	if _, err := r.Sub("l/b"); err != nil {
		t.Errorf("control: Sub follows an in-tree link: %v", err)
	}
	s, err := r.SubStrict("a/b")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Symlink("../../shared", "up"); err != nil {
		t.Errorf("a link that stays in the original tree: %v", err)
	}
	if err := s.Symlink("../../../x", "out"); !errors.Is(err, ErrEscape) {
		t.Errorf("a link out of the original tree: %v", err)
	}
}
