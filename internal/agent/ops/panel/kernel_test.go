package panel

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

// A self-update brings no installer file, so the release's kernel settings
// come with the agent's start: a file that differs, or is missing, is
// written and loaded by sysctl in a root unit; a file that matches is left
// alone and nothing runs.
func TestKernelSettingsAfterAnUpdate(t *testing.T) {
	want, err := KernelSettings()
	if err != nil || !bytes.Contains(want, []byte("net.ipv4.ip_local_port_range = 32768 65535\n")) {
		t.Fatalf("the release's settings: %v\n%s", err, want)
	}
	path := filepath.Join(t.TempDir(), "60-cloudground.conf")
	for _, c := range []struct {
		name, before string
		write        bool
	}{
		{"an older release's file", "net.ipv4.ip_local_port_range = 1024 65535\n", true},
		{"no file", "", true},
		{"this release's file", string(want), false},
	} {
		os.Remove(path)
		if c.before != "" {
			if err := os.WriteFile(path, []byte(c.before), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		fx := &host.FakeExec{}
		wrote, err := (&Kernel{Exec: fx, Path: path, Want: want}).Ensure(context.Background())
		if err != nil || wrote != c.write {
			t.Fatalf("%s: wrote %v, %v", c.name, wrote, err)
		}
		got, _ := os.ReadFile(path)
		fi, _ := os.Stat(path)
		if !bytes.Equal(got, want) || (c.write && fi.Mode().Perm() != 0o644) {
			t.Errorf("%s: file %v %q", c.name, fi.Mode(), got)
		}
		loads := slices.ContainsFunc(fx.Calls(), func(call host.FakeCall) bool {
			return call.Name == "systemd-run" && strings.HasSuffix(call.Line(), " -- sysctl -p "+path)
		})
		if loads != c.write || (len(fx.Calls()) == 1) != c.write {
			t.Errorf("%s: calls %v", c.name, fx.Calls())
		}
	}
}

// A sysctl that fails leaves the file written and the start going, during
// an update too, with the command that loads it in the journal.
func TestKernelSettingsLoadFailureIsAWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "60-cloudground.conf")
	var buf bytes.Buffer
	fx := &host.FakeExec{Respond: func(host.FakeCall) host.FakeReply { return host.FakeReply{Code: 1} }}
	k := &Kernel{Exec: fx, Path: path, Want: []byte("x = 1\n"), Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if wrote, err := k.Ensure(context.Background()); !wrote || err != nil {
		t.Fatalf("wrote %v, %v", wrote, err)
	}
	if !strings.Contains(buf.String(), "sysctl -p "+path) {
		t.Errorf("no fix in the warning: %s", buf.String())
	}
	sd := host.NewFakeSystemd()
	sd.SetUnit(host.UnitProps{Name: GuardUnit, ActiveState: "active"})
	ok := &Packages{Exec: &host.FakeExec{Respond: dpkgAnswer(map[string]string{"nftables": "install ok installed"})}, Want: []string{"nftables"}}
	os.Remove(path)
	if code := ensureMain(ok, k, sd, k.Log); code != 0 {
		t.Fatalf("a kernel setting that does not load failed the update: exit %d", code)
	}
	if got, _ := os.ReadFile(path); string(got) != "x = 1\n" {
		t.Errorf("ensureMain did not write the settings: %q", got)
	}
}
