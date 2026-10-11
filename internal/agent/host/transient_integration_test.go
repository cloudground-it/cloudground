//go:build hostintegration

// RunTransient against a real systemd: a throwaway container booted with
// systemd as PID 1, run as root (see integration_test.go).
//
//	go test -tags hostintegration -run IntegrationRunTransient ./internal/agent/host/
package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireSystemd(t *testing.T) {
	requireRoot(t)
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("systemd is not PID 1 here")
	}
}

// procsOf counts the processes whose command line contains marker.
func procsOf(marker string) int {
	ents, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range ents {
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && bytes.Contains(b, []byte(marker)) && e.Name() != "self" {
			n++
		}
	}
	return n
}

func TestIntegrationRunTransient(t *testing.T) {
	requireSystemd(t)
	sd := NewSystemd()
	defer sd.Close()
	ctx := context.Background()
	run := func(unit, script string, limit time.Duration) (TransientResult, string, error) {
		var out bytes.Buffer
		res, err := sd.RunTransient(ctx, TransientRun{
			Unit: unit, Description: "cg integration", Argv: []string{"/bin/sh", "-c", script},
			User: "nobody", Dir: "/tmp", Env: []string{"PATH=/usr/bin:/bin", "CG_MARK=1"}, MaxRuntime: limit, Output: &out,
		})
		return res, out.String(), err
	}

	// Identity, directory, environment, both streams, exit status.
	res, out, err := run("cg-it-status", `id -un; pwd; echo "mark=$CG_MARK home=$HOME"; echo to-stderr >&2; exit 3`, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "exit-code" || res.Status != 3 {
		t.Fatalf("result %+v, output %q", res, out)
	}
	for _, want := range []string{"nobody\n", "/tmp\n", "mark=1", "to-stderr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	// The failed unit was reset: the name is free again, and a clean run
	// is collected.
	res, out, err = run("cg-it-status", "echo ok", time.Minute)
	if err != nil || !res.OK() || out != "ok\n" {
		t.Fatalf("second run: %+v %q %v", res, out, err)
	}
	if p, _ := sd.UnitProps(ctx, "cg-it-status"); p.Exists() {
		t.Fatalf("a finished run is still loaded: %+v", p)
	}

	// The time limit kills the whole cgroup, children included, even one
	// that closed its output and detached.
	const marker = "299.4242" // a sleep length nothing else uses
	start := time.Now()
	res, _, err = run("cg-it-timeout", `(sleep `+marker+` >/dev/null 2>&1 &) ; sleep `+marker, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "timeout" {
		t.Fatalf("result %+v", res)
	}
	if d := time.Since(start); d > 2*time.Second+StopGrace+5*time.Second {
		t.Fatalf("took %s", d)
	}
	if n := procsOf(marker); n != 0 {
		t.Fatalf("%d processes of the run outlived its time limit", n)
	}

	// The main process exits at once and leaves children behind, one of them
	// in a session of its own: they go with the unit.
	res, out, err = run("cg-it-orphans", `(sleep `+marker+` >/dev/null 2>&1 &); setsid sleep `+marker+` </dev/null >/dev/null 2>&1 & echo started`, time.Minute)
	if err != nil || !res.OK() || out != "started\n" {
		t.Fatalf("orphans: %+v %q %v", res, out, err)
	}
	if n := procsOf(marker); n != 0 {
		t.Fatalf("%d children outlived a run whose main process exited", n)
	}

	// A unit still running is ErrUnitExists; cancelling the caller stops it.
	cctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	var cerr error
	go func() {
		defer wg.Done()
		_, cerr = sd.RunTransient(cctx, TransientRun{Unit: "cg-it-busy", Argv: []string{"/bin/sh", "-c", "sleep " + marker},
			User: "nobody", Dir: "/tmp", MaxRuntime: time.Minute})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, _ := sd.UnitProps(ctx, "cg-it-busy")
		if p.ActiveState == "active" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, _, err := run("cg-it-busy", "true", time.Minute); !errors.Is(err, ErrUnitExists) {
		t.Fatalf("second run of a busy unit: %v", err)
	}
	cancel()
	wg.Wait()
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("cancelled run: %v", cerr)
	}
	if n := procsOf(marker); n != 0 {
		t.Fatalf("%d processes outlived the cancelled run", n)
	}

	// A user that does not exist: the start job fails, the name is freed.
	_, err = sd.RunTransient(ctx, TransientRun{Unit: "cg-it-nouser", Argv: []string{"/bin/true"}, User: "cg-no-such-user", Dir: "/tmp", MaxRuntime: time.Minute})
	if err == nil {
		t.Fatal("ran as a user that does not exist")
	}
	if p, _ := sd.UnitProps(ctx, "cg-it-nouser"); p.ActiveState == "failed" {
		t.Fatal("the unit that never started was left failed")
	}
}
