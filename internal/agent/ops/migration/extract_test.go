package migration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// member is one archive entry; a regular file's content is body.
type member struct {
	h    tar.Header
	body string
}

func reg(name, body string) member {
	return member{h: tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}, body: body}
}
func dir(name string) member {
	return member{h: tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}}
}
func symlink(name, target string) member {
	return member{h: tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink}}
}
func hardlink(name, target string) member {
	return member{h: tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeLink}}
}

// tgz is the .tar.gz of members.
func tgz(t testing.TB, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := m.h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("header %s: %v", h.Name, err)
		}
		if m.body != "" {
			if _, err := io.WriteString(tw, m.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testOwner owns what the tests' Roots create: the current user, so the
// tests also run unprivileged.
func testOwner() sitefs.Owner { return sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()} }

// unpackTo unpacks archive into dest (created) with lim and a budget of
// lim.MaxBytes.
func unpackTo(t *testing.T, dest string, archive []byte, lim Limits) (Stats, string, error) {
	t.Helper()
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := sitefs.Open(dest, testOwner())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return Unpack(bytes.NewReader(archive), r, lim, lim.MaxBytes)
}

func unpack(t *testing.T, members ...member) (string, Stats, string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "tree")
	st, src, err := unpackTo(t, dest, tgz(t, members...), DefaultLimits)
	return dest, st, src, err
}

func wantInvalid(t *testing.T, what string, err error, msg string) {
	t.Helper()
	if err == nil || agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("%s: err = %v, want invalid", what, err)
	}
	if msg != "" && !strings.Contains(err.Error(), msg) {
		t.Fatalf("%s: err = %v, want it to mention %q", what, err, msg)
	}
}

func absent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err == nil {
		t.Fatalf("%s exists", p)
	}
}

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// A path-traversal name aborts the whole extraction.
func TestArchiveRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../../etc/shadow", "a/../../x", ".."} {
		outside := t.TempDir()
		dest := filepath.Join(outside, "tree")
		_, _, err := unpackTo(t, dest, tgz(t, reg("index.php", "x"), reg(name, "x")), DefaultLimits)
		wantInvalid(t, name, err, "outside the archive")
		absent(t, filepath.Join(outside, "x"))
	}
}

// An absolute member name is a separate check from a ../ one, and is what a
// tar written on the source host with a full path looks like.
func TestArchiveRejectsAbsoluteNames(t *testing.T) {
	for _, name := range []string{"/etc/shadow", "//etc/shadow", "/root/.ssh/authorized_keys"} {
		_, _, _, err := unpack(t, reg(name, "x"))
		wantInvalid(t, name, err, "absolute")
	}
}

// An escaping symlink, and a write attempted through it, never places a file
// outside the tree, and does not fail the migration: the real files extract
// and the unsafe links are skipped.
func TestArchiveSkipsEscapingLinksSafely(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dest, st, _, err := unpack(t,
		reg("index.php", "x"),
		symlink("escape", outside),                             // absolute target
		reg("escape/pwned", "x"),                               // a write through it
		hardlink("hl", "/etc/hosts"),                           // a hard link to a system file
		symlink("wp-content/object-cache.php", "../index.php"), // in-tree: kept
	)
	if err != nil {
		t.Fatalf("safe entries should extract: %v", err)
	}
	absent(t, filepath.Join(outside, "pwned"))
	absent(t, filepath.Join(dest, "hl"))
	if _, err := os.Lstat(filepath.Join(dest, "index.php")); err != nil {
		t.Fatalf("benign file not extracted: %v", err)
	}
	if !isLink(filepath.Join(dest, "wp-content/object-cache.php")) {
		t.Fatal("safe in-tree symlink not kept")
	}
	if isLink(filepath.Join(dest, "escape")) {
		t.Fatal("escaping link kept")
	}
	if st.Skipped < 2 {
		t.Fatalf("skipped = %d, want the two escaping links", st.Skipped)
	}
}

// Relative escaping links are resolved against the entry's own directory,
// which is where an off-by-one puts a write outside the site.
func TestArchiveSkipsRelativeEscapingLinks(t *testing.T) {
	outside := t.TempDir()
	dest := filepath.Join(outside, "site")
	st, _, err := unpackTo(t, dest, tgz(t,
		reg("index.php", "x"),
		symlink("up", "../"),
		symlink("wp-content/plugins/deep", "../../../../"),
		reg("up/pwned", "x"),
		reg("wp-content/plugins/deep/pwned", "x"),
		hardlink("hl", "../../etc/hosts"),
	), DefaultLimits)
	if err != nil {
		t.Fatalf("safe entries should extract: %v", err)
	}
	if st.Skipped < 3 {
		t.Fatalf("skipped = %d, want the three escaping links", st.Skipped)
	}
	absent(t, filepath.Join(outside, "pwned"))
	absent(t, filepath.Join(filepath.Dir(outside), "pwned"))
	if isLink(filepath.Join(dest, "up")) || isLink(filepath.Join(dest, "wp-content/plugins/deep")) {
		t.Fatal("an escaping link was kept")
	}
}

// A target in the tree on paper can leave it once the kernel follows the
// links on it. A hard link becomes an independent copy, never a shared inode
// (the build hands the tree to the site user).
func TestArchiveLinkChainsAndHardlinks(t *testing.T) {
	dest, st, _, err := unpack(t,
		reg("index.php", "<?php // index"),
		dir("d1/d2/"),
		symlink("d1/d2/x", "../.."),         // the tree itself: allowed
		symlink("a", "d1/d2/x/../../.."),    // lexically the tree, really above it
		hardlink("copy.php", "index.php"),   // a copy
		hardlink("dangling.php", "missing"), // nothing to copy: skipped
		hardlink("dirlink", "d1"),           // not a regular file: skipped
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "a")); err == nil {
		real, _ := filepath.EvalSymlinks(filepath.Join(dest, "a"))
		t.Fatalf("a link chain leaving the tree was kept (resolves to %s)", real)
	}
	if !isLink(filepath.Join(dest, "d1/d2/x")) {
		t.Fatal("a link to the tree itself was dropped")
	}
	src, _ := os.Stat(filepath.Join(dest, "index.php"))
	cp, err := os.Stat(filepath.Join(dest, "copy.php"))
	if err != nil {
		t.Fatalf("hard-linked member not materialised: %v", err)
	}
	if os.SameFile(src, cp) {
		t.Fatal("the hard link shares an inode with its source")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "copy.php")); string(b) != "<?php // index" {
		t.Fatalf("copy holds %q", b)
	}
	absent(t, filepath.Join(dest, "dangling.php"))
	absent(t, filepath.Join(dest, "dirlink"))
	if st.Files != 2 || st.Skipped != 3 {
		t.Fatalf("stats = %+v, want 2 files and 3 skipped", st)
	}
}

// With one top-level directory, that directory is the site, and links are
// confined to it: "top/up -> .." stays in the tree but would leave the
// release (it became releases/<id>/up -> the site's releases/).
func TestArchiveLinksConfinedToTheSourceDirectory(t *testing.T) {
	dest, st, src, err := unpack(t,
		dir("public_html/"),
		reg("public_html/index.php", "x"),
		symlink("public_html/up", ".."),
		symlink("public_html/sibling", "../public_html/index.php"),
		symlink("public_html/ok", "index.php"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if src != "public_html" {
		t.Fatalf("source = %q, want the one top-level directory", src)
	}
	absent(t, filepath.Join(dest, "public_html/up"))
	absent(t, filepath.Join(dest, "public_html/sibling"))
	if !isLink(filepath.Join(dest, "public_html/ok")) {
		t.Fatal("an in-tree link was dropped")
	}
	if st.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", st.Skipped)
	}

	// Two top-level entries: the tree is the source.
	_, _, src, err = unpack(t, dir("a/"), reg("a/index.php", "x"), reg("b.php", "x"))
	if err != nil || src != "." {
		t.Fatalf("source = %q, %v; want the tree", src, err)
	}
	// One top-level link: not a directory, so the tree is the source.
	_, _, src, err = unpack(t, reg("index.php", "x"))
	if err != nil || src != "." {
		t.Fatalf("source = %q, %v; want the tree", src, err)
	}
}

// Devices, FIFOs and unknown types are never created, and do not abort.
func TestArchiveRefusesSpecialFiles(t *testing.T) {
	dest, st, _, err := unpack(t,
		reg("index.php", "x"),
		member{h: tar.Header{Name: "fifo", Mode: 0o644, Typeflag: tar.TypeFifo}},
		member{h: tar.Header{Name: "null", Mode: 0o666, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		member{h: tar.Header{Name: "sda", Mode: 0o660, Typeflag: tar.TypeBlock, Devmajor: 8}},
		member{h: tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "git"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"fifo", "null", "sda", "pax_global_header"} {
		absent(t, filepath.Join(dest, n))
	}
	if st.Skipped != 3 {
		t.Fatalf("skipped = %d, want the three special files (not the pax header)", st.Skipped)
	}
}

// Two entries for one name, or a file where a directory is needed, abort with
// an error naming the entry, not with the work directory's host path.
func TestArchiveConflictingEntries(t *testing.T) {
	for name, members := range map[string][]member{
		"file then dir":     {reg("x", "1"), dir("x/")},
		"dir then file":     {dir("x/"), reg("x", "1")},
		"file twice":        {reg("x", "1"), reg("x", "2")},
		"file under a file": {reg("x", "1"), reg("x/y", "2")},
		"dir under a file":  {reg("x", "2"), dir("x/z/")},
	} {
		_, _, _, err := unpack(t, members...)
		wantInvalid(t, name, err, "conflicting entries")
		if strings.Contains(err.Error(), os.TempDir()) {
			t.Fatalf("%s: the error names a host path: %v", name, err)
		}
	}
	// A directory listed twice, or after its files, is not a conflict.
	if _, _, _, err := unpack(t, dir("d/"), reg("d/a", "1"), dir("d/"), dir("d/e/")); err != nil {
		t.Fatal(err)
	}
}

// A link loses to a file or directory of the same name, wherever it comes in
// the archive, and is never made inside another link.
func TestArchiveLinkNameTaken(t *testing.T) {
	dest, st, _, err := unpack(t,
		symlink("a", "index.php"),
		reg("a", "file"),
		reg("index.php", "x"),
		dir("sub/"),
		symlink("d", "sub"),
		symlink("d/inner", "../index.php"),
		symlink("twice", "index.php"),
		symlink("twice", "a"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a")); isLink(filepath.Join(dest, "a")) || string(b) != "file" {
		t.Fatal("the link replaced the file")
	}
	absent(t, filepath.Join(dest, "sub/inner"))
	if l, _ := os.Readlink(filepath.Join(dest, "twice")); l != "index.php" {
		t.Fatalf("twice -> %q, want the first one", l)
	}
	if st.Skipped != 3 {
		t.Fatalf("skipped = %d, want 3", st.Skipped)
	}
}

func TestArchiveNeedsFiles(t *testing.T) {
	_, _, _, err := unpack(t, dir("a/"), symlink("b", "a"))
	wantInvalid(t, "no files", err, "no files")
	_, _, err = unpackTo(t, filepath.Join(t.TempDir(), "t"), []byte("PK\x03\x04 not gzip"), DefaultLimits)
	wantInvalid(t, "not gzip", err, "gzip")
	a := tgz(t, reg("index.php", strings.Repeat("x", 1<<16)))
	_, _, err = unpackTo(t, filepath.Join(t.TempDir(), "t"), a[:len(a)/2], DefaultLimits)
	if err == nil {
		t.Fatal("a truncated archive was accepted")
	}
}

func TestArchiveLimits(t *testing.T) {
	lim := DefaultLimits
	lim.MaxFiles = 3
	_, _, err := unpackTo(t, filepath.Join(t.TempDir(), "t"), tgz(t, reg("a", "1"), reg("b", "1"), reg("c", "1"), reg("d", "1")), lim)
	wantInvalid(t, "files", err, "more than 3 files")

	// Hard-link copies are files too.
	_, _, err = unpackTo(t, filepath.Join(t.TempDir(), "t"), tgz(t, reg("a", "1"), hardlink("b", "a"), hardlink("c", "a"), hardlink("d", "a")), lim)
	wantInvalid(t, "hard-link copies", err, "more than 3 files")

	// Every entry counts: the previous code counted files only, so an
	// archive of directories made as many inodes as it listed.
	lim = DefaultLimits
	lim.MaxEntries = 50
	var many []member
	for i := range 60 {
		many = append(many, dir(strings.Repeat("d", i+1)+"/"))
	}
	dest := filepath.Join(t.TempDir(), "t")
	_, _, err = unpackTo(t, dest, tgz(t, append(many, reg("index.php", "x"))...), lim)
	wantInvalid(t, "entries", err, "more than 50 entries")
	if ents, _ := os.ReadDir(dest); len(ents) > 50 {
		t.Fatalf("%d directories made past the limit", len(ents))
	}

	// Content past the budget is refused before it is written.
	lim = DefaultLimits
	lim.MaxBytes = 1000
	dest = filepath.Join(t.TempDir(), "t")
	_, _, err = unpackTo(t, dest, tgz(t, reg("a", strings.Repeat("x", 600)), reg("b", strings.Repeat("x", 600))), lim)
	wantInvalid(t, "bytes", err, "expands to more than")
	absent(t, filepath.Join(dest, "b"))
}

// zeros is a reader of n zero bytes.
type zeros struct{ n int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	k := min(int64(len(p)), z.n)
	clear(p[:k])
	z.n -= k
	return int(k), nil
}

// A gigabyte of zeros is a megabyte of gzip. The budget a migration gets is
// a multiple of the compressed size, so it is refused before anything big
// is written.
func TestArchiveGzipBomb(t *testing.T) {
	const size = 256 << 20
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "index.php", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.WriteHeader(&tar.Header{Name: "bomb.log", Mode: 0o644, Size: size, Typeflag: tar.TypeReg})
	if _, err := io.Copy(tw, &zeros{size}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	lim := DefaultLimits
	lim.MinBudget = 1 << 20
	budget := lim.Budget(int64(buf.Len()), 1<<40)
	if budget >= size {
		t.Fatalf("budget %d for a %d-byte archive lets the bomb through", budget, buf.Len())
	}
	dest := filepath.Join(t.TempDir(), "t")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := sitefs.Open(dest, testOwner())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _, err = Unpack(bytes.NewReader(buf.Bytes()), r, lim, budget)
	wantInvalid(t, "bomb", err, "expands to more than")
	absent(t, filepath.Join(dest, "bomb.log"))
}

// Data of entries that are skipped still costs decompression: the stream
// is capped too.
func TestArchiveStreamCap(t *testing.T) {
	c := &capped{r: &zeros{100}, left: 10}
	n, err := io.Copy(io.Discard, c)
	if n != 10 || err == nil || !c.over {
		t.Fatalf("copied %d, err %v, over %v; want 10 and the cap", n, err, c.over)
	}
}

func TestBudget(t *testing.T) {
	l := Limits{MaxBytes: 20 << 30, MaxRatio: 200, MinBudget: 64 << 20, Reserve: 1 << 30}
	for _, c := range []struct {
		compressed, avail, want int64
	}{
		{1 << 10, 100 << 30, 64 << 20},    // tiny archive: the floor
		{1 << 30, 100 << 30, 20 << 30},    // big archive: the maximum
		{10 << 20, 100 << 30, 2000 << 20}, // the ratio
		{1 << 30, 5 << 30, 4 << 30},       // the free space, less the reserve
		{1 << 30, 512 << 20, 0},           // less free than the reserve
		{1 << 62, 1 << 62, 20 << 30},      // no overflow
	} {
		if got := l.Budget(c.compressed, c.avail); got != c.want {
			t.Errorf("Budget(%d, %d) = %d, want %d", c.compressed, c.avail, got, c.want)
		}
	}
}
