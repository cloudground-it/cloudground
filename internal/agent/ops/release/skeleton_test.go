package release

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// LockSkeleton re-roots every key file of .ssh, not only authorized_keys,
// and removes what sshd must not read there (SPEC §1.1): a restore copies a
// snapshot's .ssh as the site user's.
func TestLockSkeletonReRootsSSH(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to own files as another user")
	}
	f := newSite(t, "wordpress")
	ssh := f.path(".ssh")
	mkTree(t, f.site.RootPath, map[string]string{
		".ssh/authorized_keys": "k1\n",
		".ssh/shell_keys":      "k2\n",
		".ssh/other":           "x\n",
		".ssh/sub/f":           "y\n",
		"outside":              "z\n",
	})
	if err := os.Symlink("../outside", filepath.Join(ssh, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f.path("outside"), filepath.Join(ssh, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(f.site.RootPath, func(p string, _ os.FileInfo, _ error) error {
		return os.Lchown(p, siteUID, siteUID)
	}); err != nil {
		t.Fatal(err)
	}
	r := mustOpen(t, f)
	defer r.Close()
	if err := LockSkeleton(r); err != nil {
		t.Fatal(err)
	}
	check := func(p string, mode os.FileMode) {
		t.Helper()
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != 0 || st.Gid != siteUID || fi.Mode().Perm() != mode {
			t.Errorf("%s: %d:%d %v, want 0:%d %v", p, st.Uid, st.Gid, fi.Mode().Perm(), siteUID, mode)
		}
	}
	check(ssh, 0o750)
	for _, n := range []string{"authorized_keys", "shell_keys", "other"} {
		check(filepath.Join(ssh, n), 0o640)
	}
	for _, n := range []string{"link", "hard", "sub"} {
		if _, err := os.Lstat(filepath.Join(ssh, n)); !os.IsNotExist(err) {
			t.Errorf(".ssh/%s survived: %v", n, err)
		}
	}
	// The other name of the hard link is untouched.
	fi, err := os.Lstat(f.path("outside"))
	if err != nil || fi.Sys().(*syscall.Stat_t).Uid != siteUID {
		t.Errorf("the hard link's other name changed: %v", err)
	}
}

// A .ssh that is not a folder is left alone.
func TestLockSkeletonLeavesANonFolderSSH(t *testing.T) {
	f := newSite(t, "wordpress")
	if err := os.Symlink("releases", f.path(".ssh")); err != nil {
		t.Fatal(err)
	}
	r := mustOpen(t, f)
	defer r.Close()
	if err := LockSSH(r); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(f.path(".ssh")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf(".ssh changed: %v", err)
	}
}
