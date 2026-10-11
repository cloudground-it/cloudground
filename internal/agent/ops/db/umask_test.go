package db

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// MariaDB runs as mysql and reads the tuning file: under the agent's
// UMask=0027 a directory the agent makes for it is still 0755 and the file
// 0644.
func TestWriteAtomicModesIgnoreTheUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), "mysql", "mariadb.conf.d")
	p := filepath.Join(dir, "60-cloudground.cnf")
	if err := writeAtomic(p, "[mysqld]\n", 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{filepath.Dir(dir): 0o755, dir: 0o755, p: 0o644} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %04o, want %04o", path, fi.Mode().Perm(), want)
		}
	}
}
