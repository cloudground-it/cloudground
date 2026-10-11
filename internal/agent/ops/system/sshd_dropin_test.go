package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

func TestApplySSHDDropIn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "11-x.conf")
	refuse := false
	ex := &host.FakeExec{Respond: func(c host.FakeCall) host.FakeReply {
		if refuse {
			return host.FakeReply{Code: 255, Stderr: "Bad configuration option"}
		}
		return host.FakeReply{}
	}}
	sd := host.NewFakeSystemd()
	sd.SetUnit(host.UnitProps{Name: "ssh.service", ActiveState: "active"})

	// A new file: written, checked, sshd reloaded.
	changed, err := ApplySSHDDropIn(ctx, ex, sd, path, []byte("Match User a\n"))
	if err != nil || !changed {
		t.Fatalf("first apply: changed=%v err=%v", changed, err)
	}
	if !ex.Ran("sshd -t") || !sd.Did("reload", "ssh.service") {
		t.Fatalf("calls %v, systemd %v", ex.Calls(), sd.Ops())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}

	// The same content: nothing at all.
	n, ops := len(ex.Calls()), len(sd.Ops())
	if changed, err := ApplySSHDDropIn(ctx, ex, sd, path, []byte("Match User a\n")); err != nil || changed {
		t.Fatalf("same content: changed=%v err=%v", changed, err)
	}
	if len(ex.Calls()) != n || len(sd.Ops()) != ops {
		t.Fatal("the same content ran sshd -t or reloaded")
	}

	// sshd refuses the new content: the previous file is back, no reload.
	refuse = true
	ops = len(sd.Ops())
	if _, err := ApplySSHDDropIn(ctx, ex, sd, path, []byte("Bogus yes\n")); err == nil {
		t.Fatal("a refused configuration was accepted")
	}
	if b, _ := os.ReadFile(path); string(b) != "Match User a\n" {
		t.Fatalf("after a refusal the file holds %q", b)
	}
	if len(sd.Ops()) != ops {
		t.Fatalf("reloaded after a refusal: %v", sd.Ops()[ops:])
	}

	// Refused with no file before: the new one is removed.
	fresh := filepath.Join(t.TempDir(), "12-y.conf")
	if _, err := ApplySSHDDropIn(ctx, ex, sd, fresh, []byte("Bogus yes\n")); err == nil {
		t.Fatal("accepted")
	}
	if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
		t.Fatalf("the refused file stayed: %v", err)
	}
}

func TestReloadSSHD(t *testing.T) {
	ctx := context.Background()
	// Only sshd.service runs (other distributions).
	sd := host.NewFakeSystemd()
	sd.SetUnit(host.UnitProps{Name: "ssh.service", ActiveState: "inactive"})
	sd.SetUnit(host.UnitProps{Name: "sshd.service", ActiveState: "active"})
	if err := reloadSSHD(ctx, sd); err != nil || !sd.Did("reload", "sshd.service") || sd.Did("reload", "ssh.service") {
		t.Fatalf("err %v, ops %v", err, sd.Ops())
	}
	// Nothing running (socket activation): fine, nothing reloaded.
	idle := host.NewFakeSystemd()
	if err := reloadSSHD(ctx, idle); err != nil || len(idle.Ops()) != 0 {
		t.Fatalf("idle: err %v, ops %v", err, idle.Ops())
	}
}
