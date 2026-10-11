package files

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// siteUID owns the test site's tree when the tests run as root.
const siteUID = 4242

// The file manager never changes the site's skeleton (SPEC §1.1): the
// entries the panel manages, and anything directly in .ssh or releases/,
// which root owns; also not through an in-tree link a tenant planted.
func TestManagedEntriesAreRefused(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root: the skeleton is root's")
	}
	o, s := newSite(t)
	ssh := filepath.Join(s.RootPath, ".ssh")
	if err := os.Mkdir(ssh, 0o750); err != nil {
		t.Fatal(err)
	}
	put(t, s, ".ssh/shell_keys", []byte("k\n"))
	for _, d := range []string{"logs", "tmp", "src"} {
		if err := os.Mkdir(filepath.Join(s.RootPath, d), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(filepath.Join(s.RootPath, d), siteUID, siteUID); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{s.RootPath, ssh, filepath.Join(s.RootPath, "releases")} {
		if err := os.Lchown(p, 0, siteUID); err != nil {
			t.Fatal(err)
		}
	}
	// A tenant's link into .ssh, in its own shared/.
	if err := os.Symlink("../.ssh", filepath.Join(s.RootPath, "shared/k")); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(filepath.Join(s.RootPath, "shared/k"), siteUID, siteUID); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{".ssh/shell_keys", ".ssh/authorized_keys", "shared/k/shell_keys", "current", "releases", "shared"} {
		_, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: p, Content: "x"})
		wantCode(t, "write "+p, err, agentrpc.CodeInvalid)
		_, err = o.Transfer(ctx, rpc.TransferFileParams{Site: s, RelPath: p, Data: []byte("x"), Upload: true})
		wantCode(t, "upload "+p, err, agentrpc.CodeInvalid)
	}
	for _, p := range []string{"current", "releases", ".ssh", "shared", "logs", "tmp", "src", ".ssh/shell_keys", "shared/k/shell_keys", "releases/r1"} {
		_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "delete", RelPath: p})
		wantCode(t, "delete "+p, err, agentrpc.CodeInvalid)
	}
	for _, c := range [][2]string{{"current", "cur2"}, {"shared", "s2"}, {".ssh/shell_keys", "shared/x"}, {"shared/k/shell_keys", "shared/y"}} {
		_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "rename", RelPath: c[0], DestPath: c[1]})
		wantCode(t, "rename "+c[0], err, agentrpc.CodeInvalid)
	}
	put(t, s, "shared/new.txt", []byte("n"))
	for _, d := range []string{"tmp", ".ssh/new", "shared/k/new", "releases/r2"} {
		_, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "rename", RelPath: "shared/new.txt", DestPath: d})
		wantCode(t, "rename onto "+d, err, agentrpc.CodeInvalid)
		_, err = o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "mkdir", RelPath: d})
		wantCode(t, "mkdir "+d, err, agentrpc.CodeInvalid)
	}
	if b, _ := os.ReadFile(filepath.Join(ssh, "shell_keys")); string(b) != "k\n" {
		t.Errorf("shell_keys changed: %q", b)
	}
	for _, p := range []string{"current", "releases/r1", ".ssh", "shared"} {
		if _, err := os.Lstat(filepath.Join(s.RootPath, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}

	// What is inside the managed folders stays the file manager's.
	if _, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "shared/a.txt", Content: "a"}); err != nil {
		t.Errorf("write in shared/: %v", err)
	}
	if _, err := o.Write(ctx, rpc.WriteFileParams{Site: s, RelPath: "current/index.php", Content: "<?php"}); err != nil {
		t.Errorf("write in the live release: %v", err)
	}
	if _, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "mkdir", RelPath: "public"}); err != nil {
		t.Errorf("a folder of the tenant's own in the root: %v", err)
	}
	if _, err := o.Manage(ctx, rpc.ManageFileParams{Site: s, Operation: "delete", RelPath: "shared/k"}); err != nil {
		t.Errorf("delete the link itself: %v", err)
	}
}
