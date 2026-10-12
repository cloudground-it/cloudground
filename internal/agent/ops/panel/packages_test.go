package panel

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/units"
)

// dpkgAnswer plays dpkg-query for a server that has installed, and exits 1
// as dpkg-query does when a name is not in its database.
func dpkgAnswer(installed map[string]string) func(host.FakeCall) host.FakeReply {
	return func(c host.FakeCall) host.FakeReply {
		var out strings.Builder
		code := 0
		for _, a := range c.Args[2:] {
			st, ok := installed[a]
			if !ok {
				code = 1
				continue
			}
			out.WriteString(a + "\t" + st + "\n")
		}
		return host.FakeReply{Stdout: out.String(), Code: code}
	}
}

// #113: what dpkg has installed (or held) is not missing; a package it does
// not know, or knows as removed, is.
func TestPackagesMissing(t *testing.T) {
	fx := &host.FakeExec{Respond: dpkgAnswer(map[string]string{
		"nginx": "install ok installed", "cron": "hold ok installed", "git": "deinstall ok config-files",
	})}
	p := &Packages{Exec: fx, Want: []string{"nginx", "cron", "git", "nftables"}}
	got, err := p.Missing(context.Background())
	if err != nil || !slices.Equal(got, []string{"git", "nftables"}) {
		t.Fatalf("missing = %v, %v", got, err)
	}
	if c := fx.Calls()[0]; c.Name != "dpkg-query" || c.Args[0] != "-W" {
		t.Fatalf("ran %s", c.Line())
	}
}

// Nothing missing: dpkg is asked, apt never runs.
func TestEnsureNothingMissing(t *testing.T) {
	fx := &host.FakeExec{Respond: dpkgAnswer(map[string]string{"nginx": "install ok installed"})}
	got, err := (&Packages{Exec: fx, Want: []string{"nginx"}}).Ensure(context.Background())
	if err != nil || got != nil || len(fx.Calls()) != 1 {
		t.Fatalf("ensure = %v, %v; calls %v", got, err, fx.Calls())
	}
}

// The missing packages, and only they, are installed by apt-get run as a
// transient root service (outside the agent's sandbox), waited for.
func TestEnsureInstallsTheMissingAsARootUnit(t *testing.T) {
	dpkg := dpkgAnswer(map[string]string{"nginx": "install ok installed"})
	fx := &host.FakeExec{Respond: func(c host.FakeCall) host.FakeReply {
		if c.Name == "dpkg-query" {
			return dpkg(c)
		}
		return host.FakeReply{}
	}}
	got, err := (&Packages{Exec: fx, Want: []string{"nginx", "nftables"}}).Ensure(context.Background())
	if err != nil || !slices.Equal(got, []string{"nftables"}) {
		t.Fatalf("ensure = %v, %v", got, err)
	}
	calls := fx.Calls()
	if len(calls) != 2 || calls[1].Name != "systemd-run" {
		t.Fatalf("calls %v", calls)
	}
	line := calls[1].Line()
	for _, want := range []string{" --wait", " --pipe", " --collect", "--setenv=DEBIAN_FRONTEND=noninteractive", "RuntimeMaxSec=", "-- apt-get ", " install -y -qq nftables"} {
		if !strings.Contains(line, want) {
			t.Errorf("argv lacks %q: %s", want, line)
		}
	}
	for _, not := range []string{"--uid", "--slice", " nginx"} {
		if strings.Contains(line, not) {
			t.Errorf("argv has %q: %s", not, line)
		}
	}
}

// A failed install is tried once more after apt-get update; a second
// failure is the error.
func TestEnsureRetriesAfterUpdate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAgain bool
	}{{"the retry works", false}, {"the retry fails too", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dpkg := dpkgAnswer(map[string]string{})
			installs := 0
			fx := &host.FakeExec{Respond: func(c host.FakeCall) host.FakeReply {
				switch {
				case c.Name == "dpkg-query":
					return dpkg(c)
				case strings.Contains(c.Line(), " install "):
					installs++
					if installs == 1 || tc.failAgain {
						return host.FakeReply{Code: 100, Stderr: "E: Unable to fetch some archives"}
					}
				}
				return host.FakeReply{}
			}}
			_, err := (&Packages{Exec: fx, Want: []string{"nftables"}}).Ensure(context.Background())
			if (err != nil) != tc.failAgain {
				t.Fatalf("err = %v", err)
			}
			var seq []string
			for _, c := range fx.Calls()[1:] {
				if strings.Contains(c.Line(), " update -qq") {
					seq = append(seq, "update")
				} else {
					seq = append(seq, "install")
				}
			}
			if !slices.Equal(seq, []string{"install", "update", "install"}) {
				t.Fatalf("apt runs %v", seq)
			}
		})
	}
}

// A failure fails the agent's start while the update guard runs (the
// update is rolled back), and only then.
func TestEnsureMainFailsOnlyDuringAnUpdate(t *testing.T) {
	failing := func() *Packages {
		dpkg := dpkgAnswer(map[string]string{})
		return &Packages{Exec: &host.FakeExec{Respond: func(c host.FakeCall) host.FakeReply {
			if c.Name == "dpkg-query" {
				return dpkg(c)
			}
			return host.FakeReply{Code: 100}
		}}, Want: []string{"nftables"}}
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	sd := host.NewFakeSystemd()
	if code := ensureMain(failing(), sd, log); code != 0 {
		t.Fatalf("outside an update: exit %d", code)
	}
	if !strings.Contains(buf.String(), "apt-get install nftables") {
		t.Errorf("the warning gives no fix: %s", buf.String())
	}
	for _, st := range []string{"active", "activating"} {
		sd.SetUnit(host.UnitProps{Name: GuardUnit, ActiveState: st})
		if code := ensureMain(failing(), sd, log); code != 1 {
			t.Fatalf("guard %s: exit %d, want 1", st, code)
		}
	}
	ok := &Packages{Exec: &host.FakeExec{Respond: dpkgAnswer(map[string]string{"nftables": "install ok installed"})}, Want: []string{"nftables"}}
	if code := ensureMain(ok, sd, log); code != 0 {
		t.Fatalf("nothing missing during an update: exit %d", code)
	}
}

// The agent's unit runs the check before the agent itself, with a start
// timeout above the check's budget.
func TestAgentUnitEnsuresPackages(t *testing.T) {
	b, _ := units.Get(AgentUnit)
	s := string(b)
	pre := strings.Index(s, "\nExecStartPre=/usr/local/bin/panel-agent "+EnsurePackagesName+"\n")
	if pre < 0 || pre > strings.Index(s, "\nExecStart=") {
		t.Fatalf("no ExecStartPre=panel-agent %s before ExecStart:\n%s", EnsurePackagesName, s)
	}
	if !strings.Contains(s, "\nTimeoutStartSec=3min\n") || EnsureBudget >= 2*60e9 {
		t.Fatal("the check must fit the guard's 2-minute restart wait and the unit's start timeout")
	}
}
