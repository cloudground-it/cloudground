package sitefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The tests run on Linux (make docker-test runs them in the golang
// container, as root, so ownership is real).

var self = Owner{UID: os.Getuid(), GID: os.Getgid()}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(content), 0o644))
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (make docker-test runs as root)")
	}
}

func openRoot(t *testing.T, dir string, o Owner) *Root {
	t.Helper()
	r, err := Open(dir, o)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func stat(t *testing.T, p string) *unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	must(t, unix.Lstat(p, &st))
	return &st
}

// site builds the relative layout a site has: current -> releases/r1, the
// release's wp-config and uploads linked into shared/.
func site(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "shared/wp-config.php"), "<?php // config")
	must(t, os.MkdirAll(filepath.Join(root, "shared/uploads/2026"), 0o755))
	write(t, filepath.Join(root, "shared/uploads/2026/a.jpg"), "jpeg")
	write(t, filepath.Join(root, "releases/r1/index.php"), "<?php // index")
	must(t, os.MkdirAll(filepath.Join(root, "releases/r1/wp-content"), 0o755))
	must(t, os.Symlink("releases/r1", filepath.Join(root, "current")))
	must(t, os.Symlink("../../shared/wp-config.php", filepath.Join(root, "releases/r1/wp-config.php")))
	must(t, os.Symlink("../../../shared/uploads", filepath.Join(root, "releases/r1/wp-content/uploads")))
	return root
}

func TestOpenRefusesARelativeOrSymlinkedRoot(t *testing.T) {
	if _, err := Open("relative/dir", self); err == nil {
		t.Error("Open accepted a relative path")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(real, link))
	if _, err := Open(link, self); err == nil {
		t.Error("Open accepted a root reached through a symlink")
	}
	if _, err := Open(filepath.Join(link, "."), self); err == nil {
		t.Error("Open accepted a root with a symlink component")
	}
}

// Every way out of the root is refused by every kind of operation.
func TestEscapesAreRefused(t *testing.T) {
	root := site(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "root-only")
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "abs-file")))
	must(t, os.Symlink(outside, filepath.Join(root, "abs-dir")))
	rel, err := filepath.Rel(root, outside)
	must(t, err)
	must(t, os.Symlink(rel, filepath.Join(root, "rel-dir")))
	must(t, os.Symlink(filepath.Join(rel, "secret"), filepath.Join(root, "shared/rel-file")))
	// A chain: each link alone looks harmless.
	must(t, os.MkdirAll(filepath.Join(root, "d/x"), 0o755))
	must(t, os.Symlink("../..", filepath.Join(root, "d/x/up")))
	must(t, os.Symlink("d/x/up/..", filepath.Join(root, "chain")))

	r := openRoot(t, root, self)
	paths := []string{
		"/etc/passwd", "../" + filepath.Base(outside) + "/secret", "../../../../etc/passwd",
		"abs-file", "abs-dir/secret", "rel-dir/secret", "shared/rel-file",
		"chain/" + filepath.Base(root) + "/shared/wp-config.php", "current/../../x",
	}
	for _, p := range paths {
		if f, err := r.Open(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			t.Errorf("Open(%q) succeeded (%q)", p, b)
		}
		if _, err := r.ReadFile(p, 1<<20); err == nil {
			t.Errorf("ReadFile(%q) succeeded", p)
		}
		if _, err := r.Stat(p); err == nil {
			t.Errorf("Stat(%q) succeeded", p)
		}
		if err := r.WriteFile(p, []byte("pwned"), 0o644); err == nil {
			t.Errorf("WriteFile(%q) succeeded", p)
		}
	}
	for _, d := range []string{"abs-dir", "rel-dir"} {
		if _, err := r.ReadDir(d); err == nil {
			t.Errorf("ReadDir(%q) succeeded", d)
		}
		if err := r.Mkdir(d+"/new", 0o755); err == nil {
			t.Errorf("Mkdir(%q/new) succeeded", d)
		}
		if err := r.MkdirAll(d+"/a/b", 0o755); err == nil {
			t.Errorf("MkdirAll(%q/a/b) succeeded", d)
		}
		if f, err := r.Create(d+"/new.txt", 0o644); err == nil {
			f.Close()
			t.Errorf("Create(%q/new.txt) succeeded", d)
		}
		if err := r.Replace(d+"/new.txt", 0o644, func(*os.File) error { return nil }); err == nil {
			t.Errorf("Replace(%q/new.txt) succeeded", d)
		}
		if err := r.Remove(d + "/secret"); err == nil {
			t.Errorf("Remove(%q/secret) succeeded", d)
		}
		if err := r.RemoveAll(d + "/secret"); err == nil {
			t.Errorf("RemoveAll(%q/secret) succeeded", d)
		}
		if _, err := r.Sub(d); err == nil {
			t.Errorf("Sub(%q) succeeded", d)
		}
	}
	if err := r.Symlink("/etc", "shared/etc"); !errors.Is(err, ErrEscape) {
		t.Errorf("Symlink to an absolute target: %v", err)
	}
	if err := r.Symlink("../../..", "shared/up"); !errors.Is(err, ErrEscape) {
		t.Errorf("Symlink climbing out: %v", err)
	}
	if err := r.Rename("shared/wp-config.php", "../stolen", 0); err == nil {
		t.Error("Rename out of the root succeeded")
	}
	if _, err := r.Open("abs-dir/secret"); !errors.Is(err, ErrEscape) {
		t.Errorf("an escape is reported as ErrEscape: %v", err)
	}
	if _, err := r.Resolve("abs-file"); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve of an absolute link: %v", err)
	}
	// Nothing was created or changed outside.
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "root-only" {
		t.Errorf("outside file changed: %q", b)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Errorf("something was created outside the root: %v", ents)
	}
	// Errors name the site-relative path, never the host's.
	_, err = r.Open("abs-dir/secret")
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) {
		t.Errorf("error names a host path: %v", err)
	}
}

// The relative layout is followed, and stays inside.
func TestRelativeLayoutIsFollowed(t *testing.T) {
	root := site(t)
	r := openRoot(t, root, self)
	f, err := r.Open("current/wp-config.php")
	must(t, err)
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "<?php // config" {
		t.Errorf("read through the layout: %q", b)
	}
	if fi, err := r.Stat("current/wp-content/uploads/2026/a.jpg"); err != nil || fi.Size() != 4 {
		t.Errorf("stat through uploads: %v %v", fi, err)
	}
	ents, err := r.ReadDir("current")
	must(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "index.php,wp-config.php,wp-content" {
		t.Errorf("ReadDir(current) = %v", names)
	}
	// The editor's write goes through the link to shared/, and the link stays.
	must(t, r.WriteFile("current/wp-config.php", []byte("<?php // edited"), 0o600))
	if b, _ := os.ReadFile(filepath.Join(root, "shared/wp-config.php")); string(b) != "<?php // edited" {
		t.Errorf("shared config: %q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "releases/r1/wp-config.php")); fi.Mode()&fs.ModeSymlink == 0 {
		t.Error("WriteFile replaced the layout link with a file")
	}
	if got, err := r.Resolve("current/wp-config.php"); err != nil || got != "shared/wp-config.php" {
		t.Errorf("Resolve = %q, %v", got, err)
	}
	sub, err := r.Sub("current")
	must(t, err)
	defer sub.Close()
	if _, err := sub.Open("index.php"); err != nil {
		t.Errorf("Sub(current).Open: %v", err)
	}
	// From inside one release, shared/ is out of reach.
	if _, err := sub.Open("wp-config.php"); !errors.Is(err, ErrEscape) {
		t.Errorf("a Sub root followed a link out of itself: %v", err)
	}
}

// /proc magic links jump anywhere whatever the path says.
func TestMagicLinksAreRefused(t *testing.T) {
	r := openRoot(t, fmt.Sprintf("/proc/%d", os.Getpid()), self)
	for _, p := range []string{"root/etc/passwd", "cwd", "exe", "fd/0"} {
		if f, err := r.Open(p); err == nil {
			f.Close()
			t.Errorf("Open(%q) followed a magic link", p)
		}
	}
	if _, err := r.Open("root/etc/passwd"); !errors.Is(err, ErrSymlink) {
		t.Errorf("magic link: %v, want ErrSymlink", err)
	}
}

// A tenant swapping a directory for a symlink while root works in it never
// makes root read or write outside.
func TestSwapDuringOperationsNeverEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "f"), "SECRET")
	write(t, filepath.Join(root, "real/f"), "inside")
	rel, err := filepath.Rel(root, outside)
	must(t, err)
	must(t, os.Symlink(rel, filepath.Join(root, "relout")))
	must(t, os.Symlink(outside, filepath.Join(root, "absout")))
	r := openRoot(t, root, self)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d := filepath.Join(root, "d")
		for i := 0; !stop.Load(); i++ {
			src := "real"
			switch i % 3 {
			case 1:
				src = "relout"
			case 2:
				src = "absout"
			}
			os.Remove(d)
			os.Rename(filepath.Join(root, "d.tmp"), d)
			os.Symlink(src, filepath.Join(root, "d.tmp"))
			unix.Renameat2(unix.AT_FDCWD, filepath.Join(root, "d.tmp"), unix.AT_FDCWD, d, unix.RENAME_EXCHANGE)
		}
	}()
	deadline := time.Now().Add(1500 * time.Millisecond)
	reads := 0
	for time.Now().Before(deadline) {
		if b, err := r.ReadFile("d/f", 64); err == nil {
			reads++
			if string(b) != "inside" {
				t.Fatalf("read %q through a swapped link", b)
			}
		}
		_ = r.WriteFile("d/new", []byte("x"), 0o644)
		_ = r.Mkdir("d/dir", 0o755)
		_ = r.RemoveAll("d/dir")
	}
	stop.Store(true)
	wg.Wait()
	ents, _ := os.ReadDir(outside)
	if len(ents) != 1 {
		t.Fatalf("something was written outside: %v", ents)
	}
	t.Logf("%d confined reads succeeded during the swaps", reads)
}

func TestOwnershipThroughDescriptors(t *testing.T) {
	requireRoot(t)
	root := t.TempDir()
	o := Owner{UID: 4242, GID: 4343}
	r := openRoot(t, root, o)
	must(t, r.Mkdir("dir", 0o750))
	must(t, r.MkdirAll("a/b/c", 0o751))
	f, err := r.Create("dir/new", 0o640)
	must(t, err)
	f.Close()
	must(t, r.Replace("dir/repl", 0o600, func(f *os.File) error { _, err := f.WriteString("x"); return err }))
	must(t, r.Symlink("../dir/new", "a/link"))
	for p, mode := range map[string]uint32{"dir": 0o750, "a": 0o751, "a/b/c": 0o751, "dir/new": 0o640, "dir/repl": 0o600} {
		st := stat(t, filepath.Join(root, p))
		if int(st.Uid) != o.UID || int(st.Gid) != o.GID || st.Mode&0o7777 != mode {
			t.Errorf("%s: %d:%d %o, want %d:%d %o", p, st.Uid, st.Gid, st.Mode&0o7777, o.UID, o.GID, mode)
		}
	}
	if st := stat(t, filepath.Join(root, "a/link")); int(st.Uid) != o.UID {
		t.Errorf("symlink owned by %d", st.Uid)
	}

	// Chown on a symlink re-owns the link, never its target.
	victim := filepath.Join(t.TempDir(), "victim")
	write(t, victim, "x")
	must(t, os.Symlink(victim, filepath.Join(root, "planted")))
	must(t, r.Chown("planted", 7777, 7777))
	if st := stat(t, victim); st.Uid != 0 {
		t.Errorf("Chown followed the link: victim owned by %d", st.Uid)
	}
	if st := stat(t, filepath.Join(root, "planted")); st.Uid != 7777 {
		t.Errorf("link owned by %d", st.Uid)
	}
	// Chmod refuses a symlink outright.
	must(t, os.Chmod(victim, 0o600))
	if err := r.Chmod("planted", 0o777); !errors.Is(err, ErrSymlink) {
		t.Errorf("Chmod on a symlink: %v", err)
	}
	if st := stat(t, victim); st.Mode&0o777 != 0o600 {
		t.Errorf("Chmod followed the link: victim mode %o", st.Mode&0o777)
	}
	// The root itself.
	must(t, r.Chown(".", 0, 4343))
	must(t, r.Chmod(".", 0o750))
	if st := stat(t, root); st.Uid != 0 || st.Gid != 4343 || st.Mode&0o777 != 0o750 {
		t.Errorf("root: %d:%d %o", st.Uid, st.Gid, st.Mode&0o777)
	}
}

// Replace never writes through what is at the name: a hard link keeps its
// content, a symlink is replaced as a link.
func TestReplaceIsAtomicAndNewInode(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(root, "f"), "original")
	must(t, os.Link(filepath.Join(root, "f"), filepath.Join(root, "hard")))
	write(t, filepath.Join(outside, "target"), "outside")
	must(t, os.Symlink(filepath.Join(outside, "target"), filepath.Join(root, "sym")))
	r := openRoot(t, root, self)
	before := stat(t, filepath.Join(root, "hard")).Ino
	must(t, r.Replace("hard", 0o644, func(f *os.File) error { _, err := f.WriteString("new"); return err }))
	must(t, r.Replace("sym", 0o644, func(f *os.File) error { _, err := f.WriteString("new"); return err }))
	if b, _ := os.ReadFile(filepath.Join(root, "f")); string(b) != "original" {
		t.Errorf("the other link to the inode was truncated: %q", b)
	}
	if stat(t, filepath.Join(root, "hard")).Ino == before {
		t.Error("Replace wrote into the existing inode")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "target")); string(b) != "outside" {
		t.Errorf("Replace wrote through a symlink: %q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "sym")); !fi.Mode().IsRegular() {
		t.Error("the symlink was not replaced")
	}
	// A failing fill leaves the old file and no temporary behind.
	err := r.Replace("f", 0o644, func(*os.File) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("a failed fill was reported as success")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "f")); string(b) != "original" {
		t.Errorf("failed Replace changed the file: %q", b)
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".cg-tmp-") {
			t.Errorf("temporary left behind: %s", e.Name())
		}
	}
}

func TestWriteFileKeepsAnExistingFilesOwnerAndMode(t *testing.T) {
	requireRoot(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "f"), "old")
	must(t, os.Chown(filepath.Join(root, "f"), 1111, 2222))
	must(t, os.Chmod(filepath.Join(root, "f"), 0o604))
	r := openRoot(t, root, Owner{UID: 4242, GID: 4242})
	must(t, r.WriteFile("f", []byte("new"), 0o640))
	must(t, r.WriteFile("g", []byte("new"), 0o640))
	if st := stat(t, filepath.Join(root, "f")); st.Uid != 1111 || st.Gid != 2222 || st.Mode&0o777 != 0o604 {
		t.Errorf("existing file: %d:%d %o", st.Uid, st.Gid, st.Mode&0o777)
	}
	if st := stat(t, filepath.Join(root, "g")); st.Uid != 4242 || st.Mode&0o777 != 0o640 {
		t.Errorf("new file: %d %o", st.Uid, st.Mode&0o777)
	}
}

func TestOpenRefusesFIFOs(t *testing.T) {
	root := t.TempDir()
	must(t, unix.Mkfifo(filepath.Join(root, "fifo"), 0o644))
	r := openRoot(t, root, self)
	done := make(chan error, 1)
	go func() { _, err := r.Open("fifo"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("Open(fifo) = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Open blocked on a FIFO")
	}
}

func TestRemoveAllNeverFollowsLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "keep"), "keep")
	write(t, filepath.Join(root, "tree/sub/f"), "x")
	must(t, os.Symlink(outside, filepath.Join(root, "tree/sub/abs")))
	rel, _ := filepath.Rel(filepath.Join(root, "tree"), outside)
	must(t, os.Symlink(rel, filepath.Join(root, "tree/rel")))
	must(t, os.Symlink(outside, filepath.Join(root, "toplink")))
	r := openRoot(t, root, self)
	must(t, r.RemoveAll("tree"))
	must(t, r.RemoveAll("toplink"))
	must(t, r.RemoveAll("missing"))
	if _, err := os.Lstat(filepath.Join(root, "tree")); !os.IsNotExist(err) {
		t.Error("tree still there")
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "keep" {
		t.Errorf("RemoveAll reached outside: %v", err)
	}
	if err := r.RemoveAll("."); err == nil {
		t.Error("RemoveAll removed the root")
	}
}

func TestWalkNeverFollowsLinks(t *testing.T) {
	root := site(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "x")
	must(t, os.Symlink(outside, filepath.Join(root, "releases/r1/out")))
	r := openRoot(t, root, self)
	var seen []string
	must(t, r.Walk("releases/r1", func(rel string, fi fs.FileInfo) error {
		seen = append(seen, rel)
		return nil
	}))
	want := []string{"releases/r1", "releases/r1/index.php", "releases/r1/out", "releases/r1/wp-config.php", "releases/r1/wp-content", "releases/r1/wp-content/uploads"}
	if strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Errorf("Walk = %v\nwant %v", seen, want)
	}
	if err := r.Walk("current", func(string, fs.FileInfo) error { return nil }); !errors.Is(err, ErrSymlink) {
		t.Errorf("Walk started on a symlink: %v", err)
	}
	var n int
	must(t, r.Walk(".", func(rel string, fi fs.FileInfo) error {
		n++
		if rel == "shared" {
			return fs.SkipDir
		}
		return nil
	}))
	if n == 0 {
		t.Error("Walk(.) saw nothing")
	}
}

func TestChownAndChmodTreeNeverFollowLinks(t *testing.T) {
	requireRoot(t)
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "f"), "x")
	write(t, filepath.Join(root, "tree/sub/f"), "x")
	must(t, os.Chmod(filepath.Join(root, "tree/sub/f"), 0o666))
	must(t, os.Symlink(outside, filepath.Join(root, "tree/sub/dirlink")))
	must(t, os.Symlink(filepath.Join(outside, "f"), filepath.Join(root, "tree/filelink")))
	// A hard link to a file someone else owns.
	foreign := filepath.Join(root, "foreign")
	write(t, foreign, "theirs")
	must(t, os.Chown(foreign, 5555, 5555))
	must(t, os.Link(foreign, filepath.Join(root, "tree/hard")))
	must(t, os.Chmod(outside, 0o777))

	r := openRoot(t, root, Owner{UID: 4242, GID: 4242})
	must(t, r.ChmodTree("tree", 0o022))
	must(t, r.ChownTree("tree"))
	if st := stat(t, outside); st.Uid != 0 || st.Mode&0o777 != 0o777 {
		t.Errorf("outside dir changed: %d %o", st.Uid, st.Mode&0o777)
	}
	if st := stat(t, filepath.Join(outside, "f")); st.Uid != 0 {
		t.Errorf("outside file re-owned: %d", st.Uid)
	}
	if st := stat(t, filepath.Join(root, "tree/sub/f")); st.Uid != 4242 || st.Mode&0o777 != 0o644 {
		t.Errorf("tree file: %d %o", st.Uid, st.Mode&0o777)
	}
	if st := stat(t, filepath.Join(root, "tree/sub/dirlink")); st.Uid != 4242 {
		t.Errorf("link not re-owned as a link: %d", st.Uid)
	}
	if st := stat(t, foreign); st.Uid != 5555 {
		t.Errorf("ChownTree re-owned a hard link to someone else's file: %d", st.Uid)
	}
	must(t, os.Symlink(outside, filepath.Join(root, "toplink")))
	if err := r.ChownTree("toplink"); !errors.Is(err, ErrSymlink) {
		t.Errorf("ChownTree on a symlink: %v", err)
	}
}

func TestCopyTree(t *testing.T) {
	requireRoot(t)
	srcDir := site(t)
	outside := t.TempDir()
	// A hard link the tenant made to a file of its own outside its tree.
	write(t, filepath.Join(outside, "mine"), "outside content")
	must(t, os.Chown(filepath.Join(outside, "mine"), 3333, 3333))
	must(t, os.Link(filepath.Join(outside, "mine"), filepath.Join(srcDir, "releases/r1/hardlink")))
	must(t, os.Symlink("/etc/shadow", filepath.Join(srcDir, "releases/r1/abs")))
	must(t, unix.Mkfifo(filepath.Join(srcDir, "releases/r1/fifo"), 0o644))
	must(t, os.Chmod(filepath.Join(srcDir, "releases/r1/index.php"), 0o4755))
	ro := filepath.Join(srcDir, "releases/r1/ro")
	write(t, filepath.Join(ro, "f"), "ro")
	must(t, os.Chmod(ro, 0o555))
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(filepath.Join(srcDir, "releases/r1/index.php"), old, old))

	dstDir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dstDir, "releases"), 0o755))
	write(t, filepath.Join(dstDir, "shared/wp-config.php"), "<?php // staging config")
	src := openRoot(t, srcDir, self)
	o := Owner{UID: 4242, GID: 4343}
	dst := openRoot(t, dstDir, o)
	must(t, CopyTree(src, "releases/r1", dst, "releases/initial"))

	rel := filepath.Join(dstDir, "releases/initial")
	// The copy's layout links point at its own shared/.
	if b, err := os.ReadFile(filepath.Join(rel, "wp-config.php")); err != nil || string(b) != "<?php // staging config" {
		t.Errorf("staging wp-config resolves to %q, %v", b, err)
	}
	if got, _ := os.Readlink(filepath.Join(rel, "wp-content/uploads")); got != "../../../shared/uploads" {
		t.Errorf("uploads link = %q", got)
	}
	if got, _ := os.Readlink(filepath.Join(rel, "abs")); got != "/etc/shadow" {
		t.Errorf("a symlink was not copied verbatim: %q", got)
	}
	// The hard link became an independent copy, owned by the destination.
	hst := stat(t, filepath.Join(rel, "hardlink"))
	ost := stat(t, filepath.Join(outside, "mine"))
	if hst.Ino == ost.Ino || hst.Nlink != 1 {
		t.Errorf("the copy shares the outside inode (ino %d nlink %d)", hst.Ino, hst.Nlink)
	}
	if int(hst.Uid) != o.UID || ost.Uid != 3333 || ost.Nlink != 2 {
		t.Errorf("ownership/links: copy %d, outside %d nlink %d", hst.Uid, ost.Uid, ost.Nlink)
	}
	if b, _ := os.ReadFile(filepath.Join(rel, "hardlink")); string(b) != "outside content" {
		t.Errorf("hard link content: %q", b)
	}
	// Modes and times kept, set-uid dropped, FIFO skipped.
	ist := stat(t, filepath.Join(rel, "index.php"))
	if ist.Mode&0o7777 != 0o755 || int(ist.Uid) != o.UID || int(ist.Gid) != o.GID {
		t.Errorf("index.php: %o %d:%d", ist.Mode&0o7777, ist.Uid, ist.Gid)
	}
	if fi, _ := os.Stat(filepath.Join(rel, "index.php")); !fi.ModTime().Equal(old) {
		t.Errorf("mtime not kept: %v", fi.ModTime())
	}
	if st := stat(t, filepath.Join(rel, "ro")); st.Mode&0o777 != 0o555 {
		t.Errorf("read-only dir mode %o", st.Mode&0o777)
	}
	if _, err := os.Lstat(filepath.Join(rel, "fifo")); !os.IsNotExist(err) {
		t.Error("a FIFO was copied")
	}
	var walked []string
	must(t, filepath.Walk(rel, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			st := fi.Sys().(*syscall.Stat_t)
			if int(st.Uid) != o.UID {
				walked = append(walked, p)
			}
		}
		return err
	}))
	if len(walked) > 0 {
		t.Errorf("not owned by the destination: %v", walked)
	}

	// The start is never followed, and the destination must be new.
	if err := CopyTree(src, "current", dst, "releases/x"); !errors.Is(err, ErrSymlink) {
		t.Errorf("CopyTree from a symlink: %v", err)
	}
	if err := CopyTree(src, "releases/r1", dst, "releases/initial"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("CopyTree onto an existing directory: %v", err)
	}
	if err := CopyTree(src, "releases/r1", dst, "../escape"); !errors.Is(err, ErrEscape) {
		t.Errorf("CopyTree out of the destination: %v", err)
	}
}

func TestReadFileIsBoundedAndNeverFollowsTheFinalLink(t *testing.T) {
	root := site(t)
	r := openRoot(t, root, self)
	if _, err := r.ReadFile("releases/r1/wp-config.php", 1<<20); !errors.Is(err, ErrSymlink) {
		t.Errorf("ReadFile followed the final link: %v", err)
	}
	if b, err := r.ReadFile("current/index.php", 1<<20); err != nil || string(b) != "<?php // index" {
		t.Errorf("ReadFile through an intermediate link: %q %v", b, err)
	}
	if _, err := r.ReadFile("current/index.php", 3); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadFile past its limit: %v", err)
	}
}

func TestRenameFlags(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a"), "a")
	write(t, filepath.Join(root, "b"), "b")
	r := openRoot(t, root, self)
	if err := r.Rename("a", "b", NoReplace); !errors.Is(err, fs.ErrExist) {
		t.Errorf("NoReplace: %v", err)
	}
	must(t, r.Rename("a", "b", Exchange))
	if b, _ := os.ReadFile(filepath.Join(root, "a")); string(b) != "b" {
		t.Errorf("Exchange: a = %q", b)
	}
	ents, _ := r.ReadDir(".")
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("ReadDir = %v", names)
	}
}
