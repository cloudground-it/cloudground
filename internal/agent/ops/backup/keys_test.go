package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A restore keeps the live site's SSH keys, not the snapshot's (SPEC §5, step 3):
// a key revoked after the snapshot stays revoked, and every file of the
// restored .ssh is root's with the site's group.
func TestRestoreKeepsTheLiveSSHKeys(t *testing.T) {
	f := newFixture(t)
	mk(t, f.site.RootPath, map[string]string{
		".ssh/authorized_keys": "ssh-ed25519 OLD revoked\nssh-ed25519 KEPT kept\n",
		".ssh/shell_keys":      "ssh-ed25519 OLD revoked\n",
		".ssh/planted":         "x\n",
	})
	f.seed(false, "")
	// After the snapshot: the old key is revoked, the shell key with it.
	mk(t, f.site.RootPath, map[string]string{".ssh/authorized_keys": "ssh-ed25519 KEPT kept\n"})
	if err := os.Remove(filepath.Join(f.site.RootPath, ".ssh/shell_keys")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.site.RootPath, ".ssh/planted")); err != nil {
		t.Fatal(err)
	}
	if err := f.restore("files"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := f.live(".ssh/authorized_keys"); got != "ssh-ed25519 KEPT kept\n" {
		t.Errorf("authorized_keys after the restore = %q", got)
	}
	for _, n := range []string{".ssh/shell_keys", ".ssh/planted"} {
		if _, err := os.Lstat(filepath.Join(f.site.RootPath, n)); !os.IsNotExist(err) {
			t.Errorf("%s came back with the snapshot: %v", n, err)
		}
	}
	if os.Getuid() == 0 {
		for _, n := range []string{".ssh", ".ssh/authorized_keys"} {
			fi, err := os.Lstat(filepath.Join(f.site.RootPath, n))
			if err != nil {
				t.Fatal(err)
			}
			if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || int(st.Gid) != f.owner.GID {
				t.Errorf("%s is %d:%d, want root's with the site's group", n, st.Uid, st.Gid)
			}
		}
	}
}

// A live site with no .ssh gets none back from a snapshot that had one.
func TestRestoreDropsTheSnapshotsSSHWhenTheSiteHasNone(t *testing.T) {
	f := newFixture(t)
	mk(t, f.site.RootPath, map[string]string{".ssh/authorized_keys": "ssh-ed25519 OLD old\n"})
	f.seed(false, "")
	if err := os.RemoveAll(filepath.Join(f.site.RootPath, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if err := f.restore("files"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.site.RootPath, ".ssh")); !os.IsNotExist(err) {
		t.Errorf(".ssh came back with the snapshot: %v", err)
	}
}
