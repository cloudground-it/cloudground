package panel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

type guardFixture struct {
	g    *Guard
	sd   *host.FakeSystemd
	dir  string
	bin  string
	proc string
}

// newGuardFixture is the state SelfUpdate leaves: new binaries live, the
// previous ones as .bak, both services running the previous ones.
func newGuardFixture(t *testing.T) *guardFixture {
	dir := t.TempDir()
	f := &guardFixture{dir: dir, bin: filepath.Join(dir, "bin"), proc: filepath.Join(dir, "proc")}
	for _, d := range []string{f.bin, f.proc, filepath.Join(dir, "systemd"), filepath.Join(dir, "state", "work", "update-backup")} {
		os.MkdirAll(d, 0o755)
	}
	for _, b := range Binaries {
		os.WriteFile(filepath.Join(f.bin, b), []byte("new"), 0o755)
		os.WriteFile(filepath.Join(f.bin, b+".bak"), []byte("old"), 0o755)
	}
	f.sd = host.NewFakeSystemd()
	f.sd.SetUnit(host.UnitProps{Name: AgentUnit, ActiveState: "active", MainPID: 10})
	f.sd.SetUnit(host.UnitProps{Name: APIUnit, ActiveState: "active", MainPID: 11})
	f.sd.SetUnit(host.UnitProps{Name: APISocketUnit, ActiveState: "active"})
	// The fake hands out PIDs 1001, 1002, … in start order: the agent, the
	// socket, then the API, for the update and again for a rollback. Each
	// process runs what its unit's binary is when it starts.
	f.exe(1001, "panel-agent")
	f.exe(1003, "panel-api")
	f.exe(1004, "panel-agent")
	f.exe(1006, "panel-api")
	f.g = &Guard{
		Plan: Plan{
			Version: "v0.4.0", Previous: "v0.3.0", BinDir: f.bin, Backed: Binaries,
			SystemdDir: filepath.Join(dir, "systemd"), UnitBackup: filepath.Join(dir, "state", "work", "update-backup"),
			APISocket: filepath.Join(dir, "api.sock"), StateDir: filepath.Join(dir, "state"),
		},
		Systemd: f.sd, Proc: f.proc,
		// The new API answers according to the binary that is live.
		Healthz: func(context.Context) (int, error) { return 200, nil },
		Timeout: 300 * time.Millisecond, Poll: 5 * time.Millisecond,
	}
	return f
}

// exe makes /proc/<pid>/exe point at the live binary's path.
func (f *guardFixture) exe(pid int, bin string) {
	d := filepath.Join(f.proc, strconv.Itoa(pid))
	os.MkdirAll(d, 0o755)
	os.Symlink(filepath.Join(f.bin, bin), filepath.Join(d, "exe"))
}

func (f *guardFixture) result(t *testing.T) Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.g.Plan.StateDir, ResultName))
	if err != nil {
		t.Fatal(err)
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *guardFixture) live(t *testing.T, bin string) string {
	b, _ := os.ReadFile(filepath.Join(f.bin, bin))
	return string(b)
}

func TestGuardKeepsAHealthyRelease(t *testing.T) {
	f := newGuardFixture(t)
	if err := f.g.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := strings.Join(f.sd.Ops(), ";")
	if !strings.HasPrefix(ops, "stop "+APISocketUnit+";stop "+APIUnit+";stop "+AgentUnit+";") ||
		!strings.Contains(ops, "restart "+AgentUnit+";start "+APISocketUnit+";start "+APIUnit) || strings.Contains(ops, "daemon-reload") {
		t.Fatalf("ops %s", ops)
	}
	if r := f.result(t); !r.OK || r.RolledBack || r.Version != "v0.4.0" {
		t.Fatalf("result %+v", r)
	}
	if HighestInstalled(f.g.Plan.StateDir) != "v0.4.0" {
		t.Fatal("a healthy release was not recorded")
	}
	if f.live(t, "panel-api") != "new" {
		t.Fatal("a healthy release was rolled back")
	}
}

// A release whose API does not answer 200 is rolled back, and the previous
// one checked healthy again.
func TestGuardRollsBackAnUnhealthyRelease(t *testing.T) {
	f := newGuardFixture(t)
	f.g.Healthz = func(context.Context) (int, error) {
		if f.live(t, "panel-api") == "new" {
			return 503, nil
		}
		return 200, nil
	}
	err := f.g.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	for _, b := range Binaries {
		if f.live(t, b) != "old" {
			t.Fatalf("%s was not rolled back", b)
		}
	}
	r := f.result(t)
	if r.OK || !r.RolledBack || !r.RestoredHealthy || !strings.Contains(r.Error, "503") {
		t.Fatalf("result %+v", r)
	}
	if HighestInstalled(f.g.Plan.StateDir) != "" {
		t.Fatal("a rolled-back release raised the downgrade floor")
	}
	if n := strings.Count(strings.Join(f.sd.Ops(), ";"), "restart "+AgentUnit); n != 2 {
		t.Fatalf("agent restarted %d times, want 2", n)
	}
	// The new agent is stopped before the snapshot goes back, as the old
	// one was before it was taken.
	if n := strings.Count(strings.Join(f.sd.Ops(), ";"), "stop "+AgentUnit); n != 2 {
		t.Fatalf("agent stopped %d times, want 2 (before the snapshot, before its restore)", n)
	}
}

// A service still running another binary than the live one (the restart did
// not take) is not healthy, whatever /healthz says.
func TestGuardChecksWhatBinaryRuns(t *testing.T) {
	f := newGuardFixture(t)
	os.Remove(filepath.Join(f.proc, "1001", "exe"))
	os.Symlink(filepath.Join(f.bin, "panel-agent.bak"), filepath.Join(f.proc, "1001", "exe"))
	if err := f.g.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("err = %v", err)
	}
	if r := f.result(t); !r.RolledBack || !r.RestoredHealthy {
		t.Fatalf("result %+v", r)
	}
}

// Changed units are reloaded; a changed socket unit is restarted with the
// API stopped; a rollback restores the unit files.
func TestGuardUnitsAndSocket(t *testing.T) {
	f := newGuardFixture(t)
	unit := filepath.Join(f.g.Plan.SystemdDir, AgentUnit)
	os.WriteFile(unit, []byte("new unit"), 0o644)
	os.WriteFile(filepath.Join(f.g.Plan.UnitBackup, AgentUnit), []byte("old unit"), 0o600)
	f.g.Plan.Units = []string{AgentUnit}
	f.g.Plan.SocketChanged = true
	f.g.Healthz = func(context.Context) (int, error) { return 502, nil }
	if err := f.g.Run(context.Background()); err == nil {
		t.Fatal("unhealthy release kept")
	}
	ops := strings.Join(f.sd.Ops(), ";")
	if !strings.HasPrefix(ops, "stop "+APISocketUnit+";stop "+APIUnit+";stop "+AgentUnit+";daemon-reload;") || !strings.Contains(ops, "start "+APISocketUnit+";start "+APIUnit) {
		t.Fatalf("ops %s", ops)
	}
	if b, _ := os.ReadFile(unit); string(b) != "old unit" {
		t.Fatalf("unit not restored: %q", b)
	}
	if r := f.result(t); r.RestoredHealthy {
		t.Fatal("the restored release cannot be healthy: /healthz always fails here")
	}
}

// A restart job that fails is a failed update.
func TestGuardRestartFailure(t *testing.T) {
	f := newGuardFixture(t)
	f.sd.FailJob[AgentUnit] = "failed"
	if err := f.g.Run(context.Background()); err == nil {
		t.Fatal("kept")
	}
	if r := f.result(t); !r.RolledBack || r.RestoredHealthy || f.live(t, "panel-agent") != "old" {
		t.Fatalf("result %+v", r)
	}
}

// The default health check asks /healthz over the API's Unix socket.
func TestGuardHealthzOverTheSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	g := &Guard{Plan: Plan{APISocket: sock}}
	if code, err := g.healthz(context.Background()); err != nil || code != 503 {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestReadPlan(t *testing.T) {
	dir := t.TempDir()
	good := Plan{Version: "v0.4.0", BinDir: "/usr/local/bin", SystemdDir: "/etc/systemd/system", UnitBackup: "/var/lib/x",
		StateDir: "/var/lib/cloudground-agent", APISocket: "/run/cloudground/api.sock", Backed: Binaries}
	p := filepath.Join(dir, "plan.json")
	if err := good.write(p); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPlan(p); err != nil || got.Version != "v0.4.0" {
		t.Fatalf("%+v %v", got, err)
	}
	os.Chmod(p, 0o644)
	if _, err := ReadPlan(p); err == nil {
		t.Fatal("a world-readable plan was accepted")
	}
	os.Chmod(p, 0o600)
	link := filepath.Join(dir, "link.json")
	os.Symlink(p, link)
	if _, err := ReadPlan(link); err == nil {
		t.Fatal("a symlinked plan was accepted")
	}
	bad := good
	bad.Backed = []string{"../../etc/shadow"}
	bad.write(p)
	if _, err := ReadPlan(p); err == nil {
		t.Fatal("a plan naming a path was accepted")
	}
}
