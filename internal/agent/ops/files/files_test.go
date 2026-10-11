package files

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

var ctx = context.Background()

// newSite returns handlers on a fresh site tree owned by the test's user,
// with a release layout: releases/r1, current -> releases/r1, shared/.
func newSite(t *testing.T) (*Ops, state.Site) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "example.com")
	for _, d := range []string{"releases/r1", "shared"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/r1", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	owner := sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	if os.Getuid() == 0 {
		// As root (the container suite) the tree is a site user's, as on a
		// server: the file manager refuses entries of folders root owns.
		owner = sitefs.Owner{UID: siteUID, GID: siteUID}
		if err := filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
			if p == root {
				return nil
			}
			return os.Lchown(p, siteUID, siteUID)
		}); err != nil {
			t.Fatal(err)
		}
	}
	o := &Ops{
		Open: func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, owner) },
	}
	return o, state.Site{Domain: "example.com", SystemUser: "cg-site-1", RootPath: root}
}

func put(t *testing.T, s state.Site, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(s.RootPath, rel)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantCode(t *testing.T, what string, err error, code agentrpc.Code) {
	t.Helper()
	if got := agentrpc.CodeOf(err); err == nil || got != code {
		t.Fatalf("%s: err = %v (%s), want %s", what, err, got, code)
	}
}

func TestClassify(t *testing.T) {
	long := bytes.Repeat([]byte("a"), sniffLen+10)
	long[sniffLen+5] = 0 // past the NUL window
	cases := []struct {
		name  string
		b     []byte
		whole bool
		want  kind
	}{
		{"empty", nil, true, text},
		{"ascii", []byte("<?php echo 1;\n"), true, text},
		{"utf-8", []byte("caffè — ☕\n"), true, text},
		{"bom", []byte("\xEF\xBB\xBFname=x\n"), true, textBOM},
		{"bom only", []byte("\xEF\xBB\xBF"), true, textBOM},
		{"latin-1", []byte("caff\xe8\n"), true, binary},
		{"nul", []byte("GIF89a\x00\x01"), true, binary},
		{"png", []byte("\x89PNG\r\n\x1a\n"), true, binary},
		{"nul after the window", long, true, text},
		{"character cut by the limit", []byte("ab\xe2\x98"), false, text},
		{"file ends inside a character", []byte("ab\xe2\x98"), true, binary},
		{"invalid before the cut", []byte("a\xff\xe2\x98"), false, binary},
	}
	for _, c := range cases {
		if got := classify(c.b, c.whole); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRelCleansToTheRoot(t *testing.T) {
	for in, want := range map[string]string{
		"": ".", "/": ".", ".": ".", "//": ".", "/.": ".", "..": ".", "../..": ".",
		"a/../b": "b", "/a/./b/": "a/b", "../etc/passwd": "etc/passwd",
	} {
		if got := rel(in); got != want {
			t.Errorf("rel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListFoldersFirstAndLinks(t *testing.T) {
	o, s := newSite(t)
	put(t, s, "current/b.txt", []byte("b"))
	put(t, s, "current/a.txt", []byte("a"))
	os.Mkdir(filepath.Join(s.RootPath, "current/z"), 0o755)
	os.Symlink("/etc", filepath.Join(s.RootPath, "current/out"))

	res, err := o.List(ctx, rpc.ListFilesParams{Site: s, RelPath: "current"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Name)
		if e.Name == "out" && e.IsDir {
			t.Error("a link leading outside the site is shown as a folder")
		}
		if e.ModTime == "" || e.Mode == "" {
			t.Errorf("%s: missing mode or time: %+v", e.Name, e)
		}
	}
	if got := strings.Join(names, ","); got != "z,a.txt,b.txt,out" {
		t.Fatalf("entries = %s, want z,a.txt,b.txt,out (folders first)", got)
	}

	top, err := o.List(ctx, rpc.ListFilesParams{Site: s, RelPath: "/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range top.Entries {
		if e.Name == "current" && !e.IsDir {
			t.Error("current (a link to the release) is not shown as a folder")
		}
	}

	os.Mkdir(filepath.Join(s.RootPath, "shared/empty"), 0o755)
	empty, err := o.List(ctx, rpc.ListFilesParams{Site: s, RelPath: "shared/empty"})
	if err != nil || empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("empty folder: %#v, %v; want [] (not null)", empty.Entries, err)
	}
}

// A missing folder is not_found, with the cause kept for the log only.
func TestListMissingIsNotFound(t *testing.T) {
	o, s := newSite(t)
	_, err := o.List(ctx, rpc.ListFilesParams{Site: s, RelPath: "nope1/nope2"})
	wantCode(t, "missing folder", err, agentrpc.CodeNotFound)
	e := agentrpc.AsError(err)
	if strings.Contains(e.Message, s.RootPath) || !strings.Contains(e.Detail, "nope1") {
		t.Fatalf("message %q, detail %q", e.Message, e.Detail)
	}
	os.Symlink("/etc", filepath.Join(s.RootPath, "shared/etc"))
	_, err = o.List(ctx, rpc.ListFilesParams{Site: s, RelPath: "shared/etc"})
	wantCode(t, "listing through a link out of the site", err, agentrpc.CodeInvalid)
}

func TestReadReportsEditability(t *testing.T) {
	o, s := newSite(t)
	put(t, s, "shared/index.php", []byte("<?php echo 'caffè';\n"))
	put(t, s, "shared/bom.txt", []byte("\xEF\xBB\xBFhello\n"))
	put(t, s, "shared/latin1.txt", []byte("caff\xe8\n"))
	put(t, s, "shared/logo.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	put(t, s, "shared/big.sql", bytes.Repeat([]byte("INSERT INTO t VALUES (1);\n"), MaxEditable/20))
	put(t, s, "shared/exact.txt", bytes.Repeat([]byte("x"), MaxEditable))

	read := func(name string) rpc.ReadFileResult {
		t.Helper()
		r, err := o.Read(ctx, rpc.ReadFileParams{Site: s, RelPath: name})
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		return r
	}
	if r := read("shared/index.php"); r.Binary || r.TooLarge || r.Content != "<?php echo 'caffè';\n" || r.Encoding != "utf-8" || r.Size != int64(len(r.Content)) {
		t.Errorf("text: %+v", r)
	}
	if r := read("shared/bom.txt"); r.Binary || r.Content != "hello\n" || r.Encoding != "utf-8-bom" || r.Size != 9 {
		t.Errorf("BOM: %+v", r)
	}
	for _, n := range []string{"shared/latin1.txt", "shared/logo.png"} {
		if r := read(n); !r.Binary || r.Content != "" || r.Encoding != "" || r.Size == 0 {
			t.Errorf("%s: %+v, want binary without content", n, r)
		}
	}
	if r := read("shared/big.sql"); !r.TooLarge || r.Binary || r.Content != "" || r.Encoding != "" || r.Size <= MaxEditable {
		t.Errorf("large: too_large=%v binary=%v size=%d content=%d bytes", r.TooLarge, r.Binary, r.Size, len(r.Content))
	}
	if r := read("shared/exact.txt"); r.TooLarge || len(r.Content) != MaxEditable {
		t.Errorf("a file of exactly the limit: too_large=%v, %d bytes", r.TooLarge, len(r.Content))
	}
	// Through the release link.
	put(t, s, "current/x.txt", []byte("x"))
	if r := read("current/x.txt"); r.Content != "x" {
		t.Errorf("read through current: %+v", r)
	}

	_, err := o.Read(ctx, rpc.ReadFileParams{Site: s, RelPath: "."})
	wantCode(t, "read of a folder", err, agentrpc.CodeInvalid)
	_, err = o.Read(ctx, rpc.ReadFileParams{Site: s, RelPath: "shared/none.txt"})
	wantCode(t, "read of a missing file", err, agentrpc.CodeNotFound)
	os.Symlink("/etc/passwd", filepath.Join(s.RootPath, "shared/passwd"))
	_, err = o.Read(ctx, rpc.ReadFileParams{Site: s, RelPath: "shared/passwd"})
	wantCode(t, "read through a link out of the site", err, agentrpc.CodeInvalid)
	syscall.Mkfifo(filepath.Join(s.RootPath, "shared/fifo"), 0o644)
	_, err = o.Read(ctx, rpc.ReadFileParams{Site: s, RelPath: "shared/fifo"})
	wantCode(t, "read of a FIFO", err, agentrpc.CodeInvalid)
}

func TestWriteRefusesBinaryKeepsBOMAndMode(t *testing.T) {
	o, s := newSite(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	pngPath := put(t, s, "shared/logo.png", png)
	_, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared/logo.png", Content: "text"})
	wantCode(t, "text over a binary file", err, agentrpc.CodeConflict)
	if got, _ := os.ReadFile(pngPath); !bytes.Equal(got, png) {
		t.Fatal("the refused write changed the binary file")
	}

	bomPath := put(t, s, "shared/bom.txt", []byte("\xEF\xBB\xBFold\n"))
	os.Chmod(bomPath, 0o604)
	if _, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared/bom.txt", Content: "new\n"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(bomPath)
	fi, _ := os.Stat(bomPath)
	if string(got) != "\xEF\xBB\xBFnew\n" || fi.Mode().Perm() != 0o604 {
		t.Fatalf("BOM file after write: %q mode %v, want the mark and the mode kept", got, fi.Mode().Perm())
	}
	// Content that already carries the mark gets it once.
	o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared/bom.txt", Content: "\xEF\xBB\xBFagain\n"})
	if got, _ := os.ReadFile(bomPath); string(got) != "\xEF\xBB\xBFagain\n" {
		t.Fatalf("BOM doubled: %q", got)
	}

	if _, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "current/new.txt", Content: "hi\n"}); err != nil {
		t.Fatal(err)
	}
	np := filepath.Join(s.RootPath, "releases/r1/new.txt")
	got, _ = os.ReadFile(np)
	fi, _ = os.Stat(np)
	if string(got) != "hi\n" || fi.Mode().Perm() != 0o640 {
		t.Fatalf("new file: %q mode %v, want 0640", got, fi.Mode().Perm())
	}

	_, err = o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared", Content: "x"})
	wantCode(t, "write over a folder", err, agentrpc.CodeInvalid)
	_, err = o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "/", Content: "x"})
	wantCode(t, "write over the root", err, agentrpc.CodeInvalid)
	_, err = o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "nope/x.txt", Content: "x"})
	wantCode(t, "write into a missing folder", err, agentrpc.CodeNotFound)
	_, err = o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "x", Content: strings.Repeat("x", MaxEditable+1)})
	wantCode(t, "write over the limit", err, agentrpc.CodeInvalid)

	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(s.RootPath, "shared/out"))
	_, err = o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared/out/x.txt", Content: "x"})
	wantCode(t, "write through a link out of the site", err, agentrpc.CodeInvalid)
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("a file was written outside the site")
	}
}

// The editor saves a new inode: a hard link to a file outside the site is
// never written through.
func TestWriteNeverThroughAHardLink(t *testing.T) {
	o, s := newSite(t)
	outside := filepath.Join(t.TempDir(), "theirs")
	os.WriteFile(outside, []byte("original\n"), 0o644)
	if err := os.Link(outside, filepath.Join(s.RootPath, "shared/linked.txt")); err != nil {
		t.Skip("hard link across temp dirs:", err)
	}
	if _, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared/linked.txt", Content: "edited\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "original\n" {
		t.Fatalf("written through the hard link: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(s.RootPath, "shared/linked.txt")); string(b) != "edited\n" {
		t.Fatalf("the edit was not saved: %q", b)
	}
}

func TestTransferRoundTripAndLimits(t *testing.T) {
	o, s := newSite(t)
	data := []byte{0, 1, 2, 3, 255}
	up, err := o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared/blob.bin", Data: data, Upload: true})
	if err != nil || up.Name != "blob.bin" || up.Size != 5 {
		t.Fatalf("upload = %+v, %v", up, err)
	}
	down, err := o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared/blob.bin"})
	if err != nil || !bytes.Equal(down.Data, data) || down.Size != 5 || down.Name != "blob.bin" {
		t.Fatalf("download = %+v, %v", down, err)
	}
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "big", Data: make([]byte, MaxTransfer+1), Upload: true})
	wantCode(t, "upload over the limit", err, agentrpc.CodeInvalid)
	put(t, s, "shared/big", make([]byte, MaxTransfer+1))
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared/big"})
	wantCode(t, "download over the limit", err, agentrpc.CodeInvalid)
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared", Data: data, Upload: true})
	wantCode(t, "upload over a folder", err, agentrpc.CodeInvalid)
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared"})
	wantCode(t, "download of a folder", err, agentrpc.CodeInvalid)
	for _, p := range []string{"", "/", "."} {
		_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: p})
		wantCode(t, "transfer of the root "+p, err, agentrpc.CodeInvalid)
	}
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "nope/x", Data: data, Upload: true})
	wantCode(t, "upload into a missing folder", err, agentrpc.CodeNotFound)
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared/none"})
	wantCode(t, "download of a missing file", err, agentrpc.CodeNotFound)
	os.Symlink("/etc/hostname", filepath.Join(s.RootPath, "shared/host"))
	_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: "shared/host"})
	wantCode(t, "download through a link out of the site", err, agentrpc.CodeInvalid)
}

func TestManageWorkflowAndErrors(t *testing.T) {
	o, s := newSite(t)
	mk := func(p string) error {
		_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "mkdir", RelPath: p})
		return err
	}
	if err := mk("shared/assets"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(s.RootPath, "shared/assets"))
	if fi == nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("mkdir made %v", fi)
	}
	wantCode(t, "mkdir of an existing folder", mk("shared/assets"), agentrpc.CodeConflict)
	wantCode(t, "mkdir in a missing folder", mk("nope/x"), agentrpc.CodeNotFound)

	put(t, s, "shared/assets/a.txt", []byte("a"))
	put(t, s, "shared/assets/b.txt", []byte("b"))
	ren := func(from, to string) error {
		_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "rename", RelPath: from, DestPath: to})
		return err
	}
	wantCode(t, "rename over an existing file", ren("shared/assets/a.txt", "shared/assets/b.txt"), agentrpc.CodeConflict)
	if b, _ := os.ReadFile(filepath.Join(s.RootPath, "shared/assets/b.txt")); string(b) != "b" {
		t.Fatal("rename replaced the destination")
	}
	if err := ren("shared/assets/a.txt", "shared/assets/c.txt"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "rename of a missing file", ren("shared/assets/a.txt", "shared/x"), agentrpc.CodeNotFound)
	wantCode(t, "rename without a destination", ren("shared/assets/c.txt", " "), agentrpc.CodeInvalid)
	wantCode(t, "rename onto the root", ren("shared/assets/c.txt", "/"), agentrpc.CodeInvalid)
	wantCode(t, "rename of a folder into itself", ren("shared/assets", "shared/assets/in"), agentrpc.CodeInvalid)

	del := func(p string) error {
		_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "delete", RelPath: p})
		return err
	}
	if err := del("shared/assets"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.RootPath, "shared/assets")); !os.IsNotExist(err) {
		t.Fatal("the folder was not deleted")
	}
	wantCode(t, "delete of a missing file", del("shared/assets"), agentrpc.CodeNotFound)

	_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "chmod", RelPath: "x"})
	if e := agentrpc.AsError(err); e == nil || e.Code != agentrpc.CodeInvalid || e.Message != `unsupported file operation "chmod"` {
		t.Fatalf("chmod: %#v", err)
	}
	for _, op := range []string{"delete", "mkdir", "rename"} {
		for _, p := range []string{"", "/", ".", "//", "/.", ".."} {
			_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: op, RelPath: p, DestPath: "shared/moved"})
			wantCode(t, op+" of the root as "+p, err, agentrpc.CodeInvalid)
		}
	}
	if _, err := os.Stat(filepath.Join(s.RootPath, "shared")); err != nil {
		t.Fatal("the site's tree is damaged:", err)
	}
}

// delete removes a link as a link and never descends through one; the other
// operations refuse a parent that leads outside.
func TestManageNeverFollowsLinksOut(t *testing.T) {
	o, s := newSite(t)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	os.WriteFile(keep, []byte("x"), 0o644)
	shared := filepath.Join(s.RootPath, "shared")
	os.MkdirAll(filepath.Join(shared, "dir"), 0o755)
	os.Symlink(outside, filepath.Join(shared, "dir", "inner"))
	os.Symlink(outside, filepath.Join(shared, "link"))

	for _, p := range []string{"shared/dir", "shared/link"} {
		if _, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "delete", RelPath: p}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("deleting %s removed a file outside the site", p)
		}
	}
	os.Symlink(outside, filepath.Join(shared, "link"))
	_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "mkdir", RelPath: "shared/link/new"})
	wantCode(t, "mkdir through a link out", err, agentrpc.CodeInvalid)
	put(t, s, "shared/f.txt", []byte("y"))
	_, err = o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "rename", RelPath: "shared/f.txt", DestPath: "shared/link/f.txt"})
	wantCode(t, "rename through a link out", err, agentrpc.CodeInvalid)
	_, err = o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "delete", RelPath: "shared/link/keep.txt"})
	wantCode(t, "delete through a link out", err, agentrpc.CodeInvalid)
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Fatalf("the outside folder changed: %v", entries)
	}
}
