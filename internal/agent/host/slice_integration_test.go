//go:build hostintegration

// Per-site slices against a real systemd (a container booted with systemd
// as PID 1, as root; see integration_test.go):
//
//	go test -tags hostintegration -run IntegrationSlice ./internal/agent/host/
package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cgroupOf is the cgroup path of pid ("" when it is gone).
func cgroupOf(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// pidsOf lists the processes whose command line contains marker.
func pidsOf(marker string) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && strings.Contains(string(b), marker) {
			out = append(out, pid)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A persistent unit with a Slice= drop-in, a transient run with Slice and
// a wp-cli-style scope all land in the site's slice, and one stop of the
// slice ends all three.
func TestIntegrationSlice(t *testing.T) {
	requireSystemd(t)
	sd := NewSystemd()
	defer sd.Close()
	ctx := context.Background()
	const slice = "cg-site-990.slice"
	const marker = "311.4242" // a sleep length nothing else uses

	unit := "/etc/systemd/system/cg-it-master.service"
	t.Cleanup(func() {
		_ = sd.Stop(ctx, slice)
		os.RemoveAll(unit + ".d")
		os.Remove(unit)
		_ = sd.DaemonReload(ctx)
	})
	if err := os.WriteFile(unit, []byte("[Service]\nUser=nobody\nExecStart=/bin/sleep "+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unit+".d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit+".d", "cloudground-slice.conf"), []byte("[Service]\nSlice="+slice+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sd.DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sd.Start(ctx, "cg-it-master.service"); err != nil {
		t.Fatal(err)
	}
	p, err := sd.UnitProps(ctx, "cg-it-master.service")
	if err != nil || p.MainPID == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	want := "/cg.slice/cg-site.slice/" + slice + "/"
	if cg := cgroupOf(int(p.MainPID)); !strings.Contains(cg, want) {
		t.Fatalf("the unit runs in %q", cg)
	}

	runDone := make(chan TransientResult, 1)
	go func() {
		res, _ := sd.RunTransient(ctx, TransientRun{Unit: "cg-it-run", Argv: []string{"/bin/sleep", marker},
			User: "nobody", Dir: "/tmp", MaxRuntime: time.Minute, Slice: slice})
		runDone <- res
	}()

	scope := exec.Command("systemd-run", append(ScopeArgs(slice), "runuser", "-u", "nobody", "--", "/bin/sleep", marker)...)
	if err := scope.Start(); err != nil {
		t.Fatal(err)
	}
	scopeDone := make(chan error, 1)
	go func() { scopeDone <- scope.Wait() }()

	waitFor(t, "three processes in the slice", func() bool {
		in := 0
		for _, pid := range pidsOf(marker) {
			if strings.Contains(cgroupOf(pid), want) {
				in++
			}
		}
		return in >= 3
	})

	if err := sd.Stop(ctx, slice); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the slice's processes to end", func() bool { return len(pidsOf(marker)) == 0 })
	select {
	case <-scopeDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the scoped command outlived its slice")
	}
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the transient run outlived its slice")
	}
	if p, _ := sd.UnitProps(ctx, "cg-it-master.service"); p.ActiveState == "active" {
		t.Fatal("the unit is still active")
	}
}
