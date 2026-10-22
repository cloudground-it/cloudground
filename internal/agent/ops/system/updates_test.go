package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// simulated is apt-get -s upgrade --with-new-pkgs as Ubuntu 24.04 prints it.
const simulated = `NOTE: This is only a simulation!
      apt-get needs root privileges for real execution.
Reading package lists...
Building dependency tree...
The following NEW packages will be installed:
  linux-image-6.8.0-50-generic
The following packages will be upgraded:
  libc6 nginx linux-image-generic
Inst libc6 [2.39-0ubuntu8.3] (2.39-0ubuntu8.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64]) []
Inst linux-image-6.8.0-50-generic (6.8.0-50.51 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst nginx [1.24.0-2ubuntu7.1] (1.24.0-2ubuntu7.2 Ubuntu:24.04/noble-updates [amd64])
Inst linux-image-generic [6.8.0-49.49] (6.8.0-50.50 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst Bad;Name [1] (2 Ubuntu:24.04/noble-security [amd64])
Conf libc6 (2.39-0ubuntu8.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
`

func TestParseUpgrade(t *testing.T) {
	got := parseUpgrade([]byte(simulated))
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if strings.Join(names, " ") != "libc6 linux-image-6.8.0-50-generic linux-image-generic nginx" {
		t.Fatalf("order %v", names)
	}
	if p := got[0]; p.From != "2.39-0ubuntu8.3" || p.To != "2.39-0ubuntu8.4" || !p.Security || p.New ||
		p.Origin != "Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security" {
		t.Fatalf("libc6 %+v", p)
	}
	if p := got[1]; !p.New || p.From != "" || !p.Security {
		t.Fatalf("new kernel %+v", p)
	}
	if p := got[3]; p.Security {
		t.Fatalf("nginx from -updates only is not a security update: %+v", p)
	}
}

// updatesFixture answers the apt runs: the simulation with out, the rest
// with success.
func updatesFixture(t *testing.T, out string) *fixture {
	f := newFixture(t)
	f.Paths.RebootRequired = filepath.Join(f.dir, "reboot-required")
	f.Paths.RebootPackages = filepath.Join(f.dir, "reboot-required.pkgs")
	f.Paths.AptUpdateStamp = filepath.Join(f.dir, "update-success-stamp")
	f.exec.Respond = func(c host.FakeCall) host.FakeReply {
		if strings.Contains(c.Line(), " -s ") {
			return host.FakeReply{Stdout: out}
		}
		return host.FakeReply{}
	}
	return f
}

// aptRuns are the apt-get argument lists run, after the systemd-run part.
func aptRuns(f *fixture) []string {
	var out []string
	for _, c := range f.exec.Calls() {
		if c.Name != "systemd-run" {
			continue
		}
		line := c.Line()
		if i := strings.Index(line, " -- apt-get "); i >= 0 {
			out = append(out, line[i+len(" -- apt-get "):])
		}
	}
	return out
}

func TestSystemUpdates(t *testing.T) {
	f := updatesFixture(t, simulated)
	res, err := f.SystemUpdates(context.Background(), rpc.SystemUpdatesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packages) != 4 || res.Security != 3 || res.RebootRequired || !res.ListsUpdatedAt.IsZero() {
		t.Fatalf("%+v", res)
	}
	if runs := aptRuns(f); len(runs) != 1 || runs[0] != "-s -q -o Debug::NoLocking=1 upgrade --with-new-pkgs" {
		t.Fatalf("apt runs %q", runs)
	}
	// A refresh runs apt-get update first; the reboot notice and the lists'
	// age come from their files.
	os.WriteFile(f.Paths.RebootRequired, []byte("*** System restart required ***\n"), 0o644)
	os.WriteFile(f.Paths.RebootPackages, []byte("linux-image-6.8.0-50-generic\nlibc6\n../etc\nlibc6\n"), 0o644)
	os.WriteFile(f.Paths.AptUpdateStamp, nil, 0o644)
	res, err = f.SystemUpdates(context.Background(), rpc.SystemUpdatesParams{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RebootRequired || strings.Join(res.RebootPackages, ",") != "linux-image-6.8.0-50-generic,libc6" || res.ListsUpdatedAt.IsZero() {
		t.Fatalf("%+v", res)
	}
	if runs := aptRuns(f); len(runs) != 3 || runs[1] != "-o DPkg::Lock::Timeout=60 update -qq" {
		t.Fatalf("apt runs %q", runs)
	}
	for _, c := range f.exec.Calls() {
		if !strings.Contains(c.Line(), "--unit=cg-apt-") {
			t.Fatalf("an apt run outside a cg-apt- unit: %s", c.Line())
		}
	}
}

func TestApplyUpdates(t *testing.T) {
	f := updatesFixture(t, simulated)
	res, err := f.ApplyUpdates(context.Background(), rpc.ApplyUpdatesParams{SecurityOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	runs := aptRuns(f)
	opts := strings.Join(aptOptions, " ")
	if res.Upgraded != 2 || len(runs) != 3 || runs[1] != opts+" install --only-upgrade libc6 linux-image-generic" {
		t.Fatalf("security only: %d, runs %q", res.Upgraded, runs)
	}
	// needrestart's apt hook is suspended for the upgrade (it restarted the
	// agent mid-call on the e2e box), and its own pass runs after, with the
	// panel's services left alone.
	suspended, passes := false, 0
	for _, c := range f.exec.Calls() {
		line := c.Line()
		if strings.Contains(line, " install --only-upgrade ") {
			suspended = strings.Contains(line, "--setenv=NEEDRESTART_SUSPEND=1 --")
		}
		if strings.Contains(line, "-- /bin/sh -c rm -f /run/needrestart/unpacked") && strings.Contains(line, `qr(^cloudground-(agent|api)\.service$)} = 0`) &&
			strings.Contains(line, "needrestart -c /run/cloudground/needrestart.conf -r a -b") {
			passes++
		}
	}
	if !suspended || passes != 1 {
		t.Fatalf("needrestart suspended %v, passes %d: %q", suspended, passes, f.exec.Calls())
	}
	f = updatesFixture(t, simulated)
	res, err = f.ApplyUpdates(context.Background(), rpc.ApplyUpdatesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if runs := aptRuns(f); res.Upgraded != 4 || runs[1] != opts+" upgrade --with-new-pkgs" {
		t.Fatalf("all: %d, runs %q", res.Upgraded, runs)
	}
	// Nothing pending: nothing is run.
	f = updatesFixture(t, "Reading package lists...\n")
	if res, err := f.ApplyUpdates(context.Background(), rpc.ApplyUpdatesParams{}); err != nil || res.Upgraded != 0 || len(aptRuns(f)) != 1 {
		t.Fatalf("nothing pending: %+v %v %q", res, err, aptRuns(f))
	}
	// A failed upgrade is the error.
	f = updatesFixture(t, simulated)
	f.exec.Respond = func(c host.FakeCall) host.FakeReply {
		if strings.Contains(c.Line(), " -s ") {
			return host.FakeReply{Stdout: simulated}
		}
		return host.FakeReply{Code: 100, Stderr: "dpkg was interrupted"}
	}
	if _, err := f.ApplyUpdates(context.Background(), rpc.ApplyUpdatesParams{}); err == nil {
		t.Fatal("a failed upgrade succeeded")
	}
}
