package wp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
)

// A wp-content or mu-plugins the tenant made a symlink (here into the
// site's own .ssh) is refused: the connector and the drop-in link are
// written through the directory checked to be real, never through a link
// (SPEC §12).
func TestConnectorAndDropinRefuseLinkedDirectories(t *testing.T) {
	own := sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	for _, linked := range []string{"wp-content", "wp-content/mu-plugins"} {
		t.Run(linked, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range []string{"releases/r1/wp-content", ".ssh", "shared"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "shared/object-cache.php"), []byte("<?php"), 0o640); err != nil {
				t.Fatal(err)
			}
			at := filepath.Join(root, "releases/r1", linked)
			if err := os.RemoveAll(at); err != nil {
				t.Fatal(err)
			}
			target, _ := filepath.Rel(filepath.Dir(at), filepath.Join(root, ".ssh"))
			if err := os.Symlink(target, at); err != nil {
				t.Fatal(err)
			}
			r, err := sitefs.Open(root, own)
			if err != nil {
				t.Skip(err)
			}
			defer r.Close()
			if err := InstallConnector(r, "releases/r1"); err == nil {
				t.Error("the connector was installed through a linked " + linked)
			}
			if linked == "wp-content" {
				if err := LinkDropin(r, "releases/r1", true); err == nil {
					t.Error("the drop-in was linked through a linked wp-content")
				}
			}
			if ents, _ := os.ReadDir(filepath.Join(root, ".ssh")); len(ents) != 0 {
				t.Errorf(".ssh got %v", ents)
			}
		})
	}
}

// Through real directories both work, and the drop-in link is the relative
// one into shared/.
func TestDropinThroughRealDirectories(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"releases/r1/wp-content", "shared"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "shared/object-cache.php"), []byte("<?php"), 0o640); err != nil {
		t.Fatal(err)
	}
	r, err := sitefs.Open(root, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Skip(err)
	}
	defer r.Close()
	if err := LinkDropin(r, "releases/r1", true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(root, "releases/r1", DropinLink)); err != nil || got != DropinTarget {
		t.Fatalf("drop-in: %q %v", got, err)
	}
	if err := LinkDropin(r, "releases/r1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "releases/r1", DropinLink)); !os.IsNotExist(err) {
		t.Fatalf("drop-in still there: %v", err)
	}
}
