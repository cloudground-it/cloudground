package system

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The status poll starts no process: it was three `systemctl is-active` per
// call (measured on the old handler with the test runner: 3 per poll).
func TestSystemStatusStartsNoProcess(t *testing.T) {
	f := newFixture(t)
	f.sd.SetUnit(host.UnitProps{Name: "nginx", ActiveState: "active"})
	f.sd.SetUnit(host.UnitProps{Name: "mariadb", ActiveState: "failed"})
	f.sd.SetUnit(host.UnitProps{Name: "redis-server", ActiveState: "active"})
	for i := 0; i < 3; i++ {
		res, err := f.SystemStatus(context.Background())
		if err != nil {
			t.Skipf("no /proc here: %v", err)
		}
		if !res.NginxRunning || res.MariaDBRunning || !res.RedisRunning || res.AgentVersion == "" || res.CPUCount < 1 {
			t.Fatalf("status = %+v", res)
		}
		if res.CPUHeadroomPct < 0 || res.CPUHeadroomPct > 100 {
			t.Fatalf("cpu headroom %d", res.CPUHeadroomPct)
		}
	}
	if n := len(f.exec.Calls()); n != 0 {
		t.Fatalf("SystemStatus started %d processes: %v", n, f.exec.Calls())
	}
}

// A lost bus still returns the load numbers, with the services not running.
func TestSystemStatusWithoutSystemd(t *testing.T) {
	f := newFixture(t)
	f.sd.SetUnit(host.UnitProps{Name: "nginx", ActiveState: "active"})
	f.sd.Err = errors.New("connection refused")
	res, err := f.SystemStatus(context.Background())
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	if res.NginxRunning || res.MemTotalMB <= 0 {
		t.Fatalf("status = %+v", res)
	}
}

func TestCPUHeadroom(t *testing.T) {
	for _, c := range []struct {
		load float64
		cpus int
		want int
	}{{0, 4, 100}, {1, 4, 75}, {9, 4, 0}, {0.5, 0, 50}} {
		if got := cpuHeadroom(c.load, c.cpus); got != c.want {
			t.Errorf("cpuHeadroom(%v, %d) = %d, want %d", c.load, c.cpus, got, c.want)
		}
	}
}

func rows(t *testing.T, f *fixture) map[string]rpc.ServiceInfo {
	t.Helper()
	res, err := f.ServiceStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]rpc.ServiceInfo{}
	var order []string
	for _, s := range res.Services {
		m[s.Name] = s
		order = append(order, s.Name)
	}
	// Redis has a row only where its unit exists (it is optional).
	want := []string{"nginx", "mariadb", "site-php"}
	if u, _ := f.sd.UnitProps(context.Background(), "redis-server"); u.Exists() {
		want = []string{"nginx", "mariadb", "redis", "site-php"}
	}
	if !slices.Equal(order, want) {
		t.Fatalf("rows = %v, want %v", order, want)
	}
	return m
}

// The services table comes from typed unit properties; the version from the
// program (nginx answers on stderr).
func TestServiceStatusFromUnitProperties(t *testing.T) {
	f := newFixture(t)
	f.respond(map[string]host.FakeReply{
		"nginx":        {Stderr: "nginx version: nginx/1.28.0\n"},
		"mariadbd":     {Stdout: "mariadbd  Ver 11.4.7-MariaDB-0ubuntu0.25.04.1 for debian-linux-gnu\n"},
		"redis-server": {Err: errors.New("exec: \"redis-server\": executable file not found in $PATH")},
	})
	since := time.Date(2026, 10, 8, 19, 0, 0, 0, time.UTC)
	f.sd.SetUnit(host.UnitProps{Name: "nginx", ActiveState: "active", UnitFileState: "enabled",
		MemoryCurrent: 52428800, MemoryKnown: true, ActiveEnter: since})
	f.sd.SetUnit(host.UnitProps{Name: "mariadb", ActiveState: "failed", UnitFileState: "enabled", MemoryCurrent: 9, MemoryKnown: true})
	m := rows(t, f)
	ng := m["nginx"]
	if ng.State != "running" || !ng.Active || !ng.Enabled || ng.Detail != "active" || ng.MemoryBytes != 52428800 || !ng.Since.Equal(since) || ng.Version != "1.28.0" || ng.Unit != "nginx" {
		t.Fatalf("nginx = %+v", ng)
	}
	if db := m["mariadb"]; db.State != "failed" || db.Active || db.Detail != "failed" || db.MemoryBytes != 0 || db.Version != "11.4.7" {
		t.Fatalf("mariadb = %+v", db)
	}
	// redis-server is not installed (the installer leaves it out, R-6.1):
	// systemd does not know the unit, and the page has no row for it.
	if r, ok := m["redis"]; ok {
		t.Fatalf("a Redis that is not installed has a row: %+v", r)
	}
	// An upgraded server keeps it, switched off: a disabled row.
	f.sd.SetUnit(host.UnitProps{Name: "redis-server", ActiveState: "inactive", UnitFileState: "disabled"})
	if r := rows(t, f)["redis"]; r.State != "disabled" || r.Active || r.Unit != "redis-server" || r.Detail != "inactive" || r.Version != "" {
		t.Fatalf("redis = %+v", r)
	}
}

// A service whose unit systemd does not know is not installed: a restart
// is a conflict that says so, and no job is started (a job on a missing
// unit failed, and the API answered 502).
func TestRestartServiceNotInstalled(t *testing.T) {
	f := newFixture(t)
	_, err := f.RestartService(context.Background(), rpc.ServiceParams{Name: "redis"})
	if codeOf(err) != agentrpc.CodeConflict || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("restart of a Redis that is not installed: err = %v", err)
	}
	if ops := f.sd.Ops(); len(ops) != 0 {
		t.Fatalf("a job was started: %v", ops)
	}
}

func TestStateOf(t *testing.T) {
	for _, c := range []struct {
		active, file, want string
	}{
		{"active", "enabled", "running"},
		{"active", "disabled", "running"},
		{"failed", "enabled", "failed"},
		{"inactive", "disabled", "disabled"},
		{"inactive", "", "disabled"},
		{"inactive", "enabled", "stopped"},
	} {
		if got := stateOf(host.UnitProps{ActiveState: c.active, UnitFileState: c.file}); got != c.want {
			t.Errorf("stateOf(%s, %s) = %s, want %s", c.active, c.file, got, c.want)
		}
	}
}

// A version is asked once per start of the service, a failed probe included:
// the old cache kept only successes, so a box without redis forked
// `redis-server --version` on every poll of the services page.
func TestServiceVersionIsProbedOncePerStart(t *testing.T) {
	f := newFixture(t)
	f.respond(map[string]host.FakeReply{
		"nginx":        {Stderr: "nginx version: nginx/1.28.0"},
		"mariadbd":     {Stdout: "mariadbd Ver 11.4.7"},
		"redis-server": {Code: 127},
	})
	f.sd.SetUnit(host.UnitProps{Name: "nginx", ActiveState: "active", ActiveEnter: time.Unix(100, 0)})
	f.sd.SetUnit(host.UnitProps{Name: "mariadb", ActiveState: "active", ActiveEnter: time.Unix(100, 0)})
	// Its unit is there, its program answers nothing.
	f.sd.SetUnit(host.UnitProps{Name: "redis-server", ActiveState: "inactive", UnitFileState: "disabled"})
	rows(t, f)
	if n := len(f.exec.Calls()); n != 3 {
		t.Fatalf("first poll: %d probes, want 3", n)
	}
	for i := 0; i < 5; i++ {
		rows(t, f)
	}
	if n := len(f.exec.Calls()); n != 3 {
		t.Fatalf("later polls probed again: %d calls %v", n, f.exec.Calls())
	}
	// An upgrade restarts nginx: its version is asked again, once.
	f.respond(map[string]host.FakeReply{"nginx": {Stderr: "nginx version: nginx/1.29.1"}})
	f.sd.SetUnit(host.UnitProps{Name: "nginx", ActiveState: "active", ActiveEnter: time.Unix(200, 0)})
	if v := rows(t, f)["nginx"].Version; v != "1.29.1" {
		t.Fatalf("version after restart = %q", v)
	}
	rows(t, f)
	if n := len(f.exec.Calls()); n != 4 {
		t.Fatalf("%d calls after the restart, want 4", n)
	}
}

// Site PHP is one row over every site master. A deleted site's unit that
// systemd still remembers is skipped, and the read does not reset it: the
// old code ran reset-failed from inside this read.
func TestSitePHPRow(t *testing.T) {
	f := newFixture(t)
	f.sd.SetUnit(host.UnitProps{Name: "cloudground-php-cg-site-10", ActiveState: "active"})
	f.sd.SetUnit(host.UnitProps{Name: "cloudground-php-cg-site-182", LoadState: "not-found", ActiveState: "failed"})
	if php := rows(t, f)["site-php"]; php.State != "running" || !php.Active || php.Detail != "1 running" || php.Unit != "cloudground-php-*" {
		t.Fatalf("site-php = %+v", php)
	}
	if ops := f.sd.Ops(); len(ops) != 0 {
		t.Fatalf("a read changed systemd: %v", ops)
	}
	f.sd.SetUnit(host.UnitProps{Name: "cloudground-php-cg-site-2", ActiveState: "failed"})
	f.sd.SetUnit(host.UnitProps{Name: "nginx", ActiveState: "failed"}) // not a site
	php := rows(t, f)["site-php"]
	if php.State != "failed" || php.Active || php.Detail != "1 running, 1 not running: cg-site-2 failed" {
		t.Fatalf("site-php = %+v", php)
	}
}

// A systemd that cannot be asked is an error, not a page of stopped services.
func TestServiceStatusBusFailure(t *testing.T) {
	f := newFixture(t)
	f.sd.Err = errors.New("dial unix /run/systemd/private: connection refused")
	if _, err := f.ServiceStatus(context.Background()); err == nil {
		t.Fatal("no error with systemd unreachable")
	}
}

func TestRestartServiceWaitsOnSystemd(t *testing.T) {
	f := newFixture(t)
	f.sd.SetUnit(host.UnitProps{Name: "redis-server", ActiveState: "active", MainPID: 10})
	res, err := f.RestartService(context.Background(), rpc.ServiceParams{Name: "redis"})
	if err != nil {
		t.Fatal(err)
	}
	if res["restarted"] != "redis-server" || !f.sd.Did("restart", "redis-server") {
		t.Fatalf("res %v ops %v", res, f.sd.Ops())
	}
	if u, _ := f.sd.Unit("redis-server"); u.MainPID == 10 {
		t.Fatal("not restarted")
	}
	f.sd.FailJob["redis-server.service"] = "failed"
	if _, err := f.RestartService(context.Background(), rpc.ServiceParams{Name: "redis"}); !errors.Is(err, host.ErrJobFailed) {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"sshd", "php-fpm", "redis-server", ""} {
		if _, err := f.RestartService(context.Background(), rpc.ServiceParams{Name: name}); codeOf(err) != agentrpc.CodeInvalid {
			t.Fatalf("restart %q: err = %v", name, err)
		}
	}
	if len(f.exec.Calls()) != 0 {
		t.Fatalf("a process was started: %v", f.exec.Calls())
	}
}

// A service switched off on purpose stays off: a restart of a stopped,
// disabled unit would start it (Redis, listening on loopback for every
// site). A disabled one that runs, or a stopped enabled one, is restarted.
func TestRestartServiceLeavesADisabledServiceOff(t *testing.T) {
	f := newFixture(t)
	f.sd.SetUnit(host.UnitProps{Name: "redis-server.service", ActiveState: "inactive", UnitFileState: "disabled"})
	if _, err := f.RestartService(context.Background(), rpc.ServiceParams{Name: "redis"}); codeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("restart of a disabled, stopped redis: err = %v", err)
	}
	if f.sd.Did("restart", "redis-server") {
		t.Fatal("a disabled redis was started")
	}
	for _, u := range []host.UnitProps{
		{Name: "redis-server.service", ActiveState: "active", UnitFileState: "disabled"},
		{Name: "redis-server.service", ActiveState: "failed", UnitFileState: "enabled"},
	} {
		f.sd.SetUnit(u)
		if _, err := f.RestartService(context.Background(), rpc.ServiceParams{Name: "redis"}); err != nil {
			t.Fatalf("%s/%s: %v", u.ActiveState, u.UnitFileState, err)
		}
	}
}
