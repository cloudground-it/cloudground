//go:build hostintegration

// The D-Bus connection to a real systemd: a container booted with systemd
// as PID 1, as root (see integration_test.go and SPEC.md, "Connection").
//
//	go test -tags hostintegration -run IntegrationSystemdConn ./internal/agent/host/
package host

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

// testUnit writes a unit to /run/systemd/system (gone with the container)
// and loads it.
func testUnit(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile("/run/systemd/system/"+name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", name).Run()
		os.Remove("/run/systemd/system/" + name)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reload: %v %s", err, out)
	}
}

func systemdVersion() string {
	out, _ := exec.Command("systemctl", "--version").Output()
	line, _, _ := strings.Cut(string(out), "\n")
	return line
}

// Measured: what closed the agent's connection. A connection go-systemd
// dials with a context (NewSystemdConnectionContext, which the adapter used
// with the first caller's request context) is closed by godbus when that
// context ends; a job enqueued on it never reports. The agent's RPC server
// cancels every request's context when the handler returns.
func TestIntegrationSystemdConnDialContextKillsIt(t *testing.T) {
	requireSystemd(t)
	t.Log(systemdVersion())
	testUnit(t, "cgit-slow.service", "[Service]\nType=oneshot\nExecStart=/bin/sleep 2\n")
	ctx, cancel := context.WithCancel(context.Background())
	c, err := sddbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch := make(chan string, 1)
	if _, err := c.StartUnitContext(context.Background(), "cgit-slow.service", "replace", ch); err != nil {
		t.Fatal(err)
	}
	cancel() // the request that dialled it returns
	time.Sleep(100 * time.Millisecond)
	if c.Connected() {
		t.Fatal("the connection survived its dial context: the root cause in SPEC.md no longer holds")
	}
	select {
	case res := <-ch:
		t.Fatalf("a result (%s) came on the closed connection", res)
	case <-time.After(4 * time.Second):
	}
	// systemd itself ran the job to the end.
	out, _ := exec.Command("systemctl", "show", "-p", "Result", "cgit-slow.service").Output()
	t.Logf("job result never delivered; systemd says %s", strings.TrimSpace(string(out)))
}

// Measured: systemd keeps its private-socket clients across daemon-reload
// (it answers Reload once done, on the same connection) and drops them on
// daemon-reexec. The adapter keeps one connection through reloads and
// re-dials after a re-exec.
func TestIntegrationSystemdConnReloadAndReexec(t *testing.T) {
	requireSystemd(t)
	t.Log(systemdVersion())
	testUnit(t, "cgit-svc.service", "[Service]\nExecStart=/bin/sleep infinity\n")
	sd := NewSystemd()
	defer sd.Close()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := sd.DaemonReload(ctx); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
			t.Fatal(err)
		}
	}
	if err := sd.Restart(ctx, "cgit-svc"); err != nil {
		t.Fatal(err)
	}
	if sd.gen != 1 {
		t.Fatalf("daemon-reload dropped the connection: %d dials", sd.gen)
	}
	sd.mu.Lock()
	conn := sd.cur
	sd.mu.Unlock()

	if out, err := exec.Command("systemctl", "daemon-reexec").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reexec: %v %s", err, out)
	}
	select {
	case <-conn.Done():
		t.Log("daemon-reexec dropped the private-socket connection")
	case <-time.After(10 * time.Second):
		t.Fatal("daemon-reexec kept the connection")
	}
	if err := sd.Restart(ctx, "cgit-svc"); err != nil {
		t.Fatalf("after re-exec: %v", err)
	}
	if sd.gen != 2 {
		t.Fatalf("dials %d", sd.gen)
	}
}

// The storm, against real systemd: many callers, each with its own request
// context cancelled as soon as its call returns (as the RPC server does),
// restarting units and reloading the daemon at once, with systemctl
// reloading it too, several callers on the same unit (systemd merges their
// jobs: see unitLock). Every call succeeds, in bounded time, on one
// connection.
func TestIntegrationSystemdConnConcurrentReloadsAndRestarts(t *testing.T) {
	requireSystemd(t)
	t.Log(systemdVersion())
	const units = 4
	for i := 0; i < units; i++ {
		// No start rate limit: the storm restarts each unit dozens of times.
		testUnit(t, fmt.Sprintf("cgit-conc-%d.service", i), "[Unit]\nStartLimitIntervalSec=0\n[Service]\nExecStart=/bin/sleep infinity\n")
	}
	sd := NewSystemd()
	defer sd.Close()
	sd.JobTimeout = 60 * time.Second
	const workers, rounds = 16, 12
	errs := make(chan error, workers*rounds)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for r := 0; r < rounds; r++ {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
				unit := fmt.Sprintf("cgit-conc-%d", rng.Intn(units))
				var err error
				switch rng.Intn(5) {
				case 0:
					err = sd.DaemonReload(ctx)
				case 1:
					err = sd.ReloadOrRestart(ctx, unit)
				case 2:
					err = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
				case 3:
					err = sd.Stop(ctx, unit)
				default:
					err = sd.Restart(ctx, unit)
				}
				cancel() // the request is over
				if err != nil {
					errs <- fmt.Errorf("worker %d round %d: %w", w, r, err)
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		t.Fatal("the storm did not finish")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	t.Logf("%d calls in %v on %d connection(s)", workers*rounds, time.Since(start).Round(time.Millisecond), sd.gen)
	if sd.gen != 1 {
		t.Fatalf("dials %d: the connection did not outlive its callers", sd.gen)
	}
}
