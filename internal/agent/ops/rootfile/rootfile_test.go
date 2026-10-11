package rootfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReplaceSetsExactModeWhateverTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	p := filepath.Join(dir, "panel.pem")
	if err := Replace(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v (%v), want 0644", fi.Mode().Perm(), err)
	}
	if err := ReplaceOwned(p, []byte("two"), 0o640, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ = os.Stat(p)
	if string(b) != "two" || fi.Mode().Perm() != 0o640 {
		t.Fatalf("got %q %v", b, fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// A symlink at the target's name is replaced, not written through.
func TestReplaceDoesNotWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	p := filepath.Join(dir, "conf")
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	if err := Replace(p, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the symlink's target was written: %q", b)
	}
	if fi, _ := os.Lstat(p); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the symlink is still there")
	}
	if _, err := ReadNoFollow(p); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	os.Symlink(victim, p)
	if _, err := ReadNoFollow(p); err == nil {
		t.Fatal("ReadNoFollow followed a symlink")
	}
}

func TestMkdirModeIgnoresTheUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	d := filepath.Join(t.TempDir(), "a", ".well-known")
	if err := MkdirMode(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// Every directory MkdirAllMode creates has exactly the mode under the agent's
// umask; one that was there keeps its own, and a symlink at the leaf is
// refused by MkdirMode, never chmod-ed through.
func TestMkdirAllModeCreatesEachParentWithTheMode(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	base := t.TempDir()
	if err := os.Chmod(base, 0o711); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(base, "a", "b", "c")
	if err := MkdirAllMode(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(base, "a"), filepath.Join(base, "a", "b"), d} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: mode %v, want 0755", p, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(base); fi.Mode().Perm() != 0o711 {
		t.Errorf("an existing parent was changed: %v", fi.Mode().Perm())
	}
	os.Chmod(d, 0o700)
	if err := MkdirAllMode(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Errorf("MkdirAllMode changed an existing directory: %v", fi.Mode().Perm())
	}
	if err := MkdirMode(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o755 {
		t.Errorf("MkdirMode left an existing directory at %v", fi.Mode().Perm())
	}

	target := t.TempDir()
	os.Chmod(target, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := MkdirMode(link, 0o755); err == nil {
		t.Error("MkdirMode accepted a symlink")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o700 {
		t.Errorf("the symlink's target was chmod-ed: %v", fi.Mode().Perm())
	}
	f := filepath.Join(base, "file")
	os.WriteFile(f, nil, 0o600)
	if err := MkdirAllMode(filepath.Join(f, "x"), 0o755); err == nil {
		t.Error("a file was taken for a directory")
	}
}
