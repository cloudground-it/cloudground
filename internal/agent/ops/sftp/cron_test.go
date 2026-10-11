package sftp

import (
	"os"
	"path/filepath"
	"testing"
)

// The block an earlier build wrote into cron.deny goes; the operator's
// lines stay; a file that held only the block goes with it.
func TestRemoveDenyBlocks(t *testing.T) {
	dir := t.TempDir()
	cronDeny, atDeny := filepath.Join(dir, "cron.deny"), filepath.Join(dir, "at.deny")
	block := denyBegin + "\ncg-site-1\ncg-shell-1\n" + denyEnd + "\n"
	if err := os.WriteFile(cronDeny, []byte("guest\n"+block+"nobody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(atDeny, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &Ops{ShellPaths: ShellPaths{CronDeny: cronDeny, AtDeny: atDeny}}
	if err := o.RemoveDenyBlocks(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cronDeny); string(b) != "guest\nnobody\n" {
		t.Fatalf("cron.deny %q", b)
	}
	if _, err := os.Lstat(atDeny); !os.IsNotExist(err) {
		t.Fatalf("at.deny with only the block stayed: %v", err)
	}
	// Nothing to do: no file, no block.
	if err := o.RemoveDenyBlocks(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cronDeny); string(b) != "guest\nnobody\n" {
		t.Fatalf("cron.deny changed again: %q", b)
	}
}
