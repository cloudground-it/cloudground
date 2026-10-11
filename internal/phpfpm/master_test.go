package phpfpm

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// SPEC §3.1: memory decides, three workers per CPU cap it, an explicit
// request wins, and 2…64 bounds everything.
func TestWorkerSizing(t *testing.T) {
	for _, c := range []struct {
		cpus, budget, requested, want int
	}{
		{64, 1024, 0, 12},
		{64, 4096, 0, 51},
		{64, 100, 0, 2},
		{64, 1 << 20, 0, 64},
		{2, 4096, 0, 6},
		{2, 4096, 10, 10},
		{2, 4096, 1, 2},
		{2, 4096, 500, 64},
		{0, 4096, 0, 51}, // no CPU count known: memory alone
	} {
		withCPUs(t, c.cpus)
		w := SizeWorkers(c.budget, c.requested)
		if w.Max != c.want {
			t.Errorf("cpus %d budget %d requested %d: %d workers, want %d", c.cpus, c.budget, c.requested, w.Max, c.want)
		}
		if w.StartServe < 1 || w.MinSpare < 1 || w.MaxSpare < w.StartServe || w.MaxSpare > w.Max || w.MaxRequests != 1000 {
			t.Errorf("inconsistent spares for %d workers: %+v", w.Max, w)
		}
	}
}

func site(t state.SiteType, p state.Profile) state.Site {
	return state.Site{ID: 3, Domain: "a.example", Type: t, Profile: p, PHPVersion: "8.4",
		SystemUser: "cg-site-3", RootPath: "/srv/sites/a.example"}
}

// SPEC §3.4: only a Balanced non-store keeps no idle workers.
func TestProcessManagerByProfile(t *testing.T) {
	for _, c := range []struct {
		s    state.Site
		want string
	}{
		{site(state.SiteWordPress, state.ProfileBalanced), "pm = ondemand\n"},
		{site(state.SitePHP, state.ProfileBalanced), "pm = ondemand\n"},
		{site(state.SiteWooCommerce, state.ProfileBalanced), "pm = dynamic\n"},
		{site(state.SiteWordPress, state.ProfileCommerce), "pm = dynamic\n"},
		{site(state.SiteWordPress, state.ProfileMaximum), "pm = dynamic\n"},
	} {
		_, pool, err := RenderPool(c.s, 1024)
		if err != nil || !strings.Contains(pool, c.want) {
			t.Errorf("%s/%s: want %q (err %v)", c.s.Type, c.s.Profile, strings.TrimSpace(c.want), err)
		}
		if strings.Contains(pool, "pm = ondemand") == strings.Contains(pool, "pm.min_spare_servers") {
			t.Errorf("%s/%s: spare servers must go with dynamic only", c.s.Type, c.s.Profile)
		}
	}
}

// SPEC §3.3: defaults, the store's memory, the overrides, and the
// wall-clock kill following max_execution_time.
func TestPoolLimits(t *testing.T) {
	s := site(state.SiteWooCommerce, state.ProfileCommerce)
	_, pool, _ := RenderPool(s, 0)
	for _, want := range []string{"php_admin_value[memory_limit] = 512M\n", "php_admin_value[upload_max_filesize] = 64M\n",
		"php_admin_value[max_execution_time] = 120\n", "request_terminate_timeout = 120s\n"} {
		if !strings.Contains(pool, want) {
			t.Errorf("store defaults: missing %q", strings.TrimSpace(want))
		}
	}
	s.Config.PHP = state.PHPSettings{MemoryLimitMB: 300, UploadMaxMB: 10, MaxExecutionSeconds: 300}
	_, pool, _ = RenderPool(s, 0)
	for _, want := range []string{"php_admin_value[memory_limit] = 300M\n", "php_admin_value[post_max_size] = 10M\n",
		"php_admin_value[max_execution_time] = 300\n", "request_terminate_timeout = 310s\n"} {
		if !strings.Contains(pool, want) {
			t.Errorf("overrides: missing %q", strings.TrimSpace(want))
		}
	}
}

// SPEC §5.2 (R-5.2): a reload waits for running requests up to 30 s (it
// was 10 s), never the full request limit: the site serves no uncached
// request while it waits.
func TestReloadDrainIsCapped(t *testing.T) {
	for exec, terminate := range map[int]string{0: "120s", 60: "120s", 300: "310s", 900: "910s"} {
		s := site(state.SiteWordPress, state.ProfileBalanced)
		s.Config.PHP.MaxExecutionSeconds = exec
		_, pool, _ := RenderPool(s, 1024)
		if !strings.Contains(pool, "\nprocess_control_timeout = 30s\n") || !strings.Contains(pool, "\nrequest_terminate_timeout = "+terminate+"\n") {
			t.Errorf("max_execution_seconds %d: want process_control_timeout = 30s and request_terminate_timeout = %s", exec, terminate)
		}
	}
}

// SPEC §1, §3.2: the boundary is the namespace, and master-wide cache sizes
// are on the command line, never in the pool where PHP ignores them.
func TestPoolAndUnitSplit(t *testing.T) {
	s := site(state.SiteWooCommerce, state.ProfileCommerce)
	_, pool, _ := RenderPool(s, 2048)
	_, unit, _ := RenderUnit(s, "/usr/sbin")
	for _, never := range []string{"[open_basedir]", "opcache.memory_consumption", "apc.shm_size", "opcache.jit_buffer_size", "\nuser ="} {
		if strings.Contains(pool, never) {
			t.Errorf("the pool sets %q", never)
		}
	}
	for _, want := range []string{"ExecStart=/usr/sbin/php-fpm8.4 --nodaemonize --fpm-config /etc/cloudground-php/cg-site-3.conf ",
		"-d opcache.memory_consumption=128 ", "-d apc.shm_size=64M\n", "User=cg-site-3\n", "ExecReload=/bin/kill -USR2 $MAINPID\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q", want)
		}
	}
}

func TestRefusesIncompleteSites(t *testing.T) {
	if _, _, err := RenderPool(state.Site{Domain: "x", PHPVersion: "8.4"}, 0); err == nil {
		t.Error("a pool without a system user was rendered")
	}
	if _, _, err := RenderUnit(state.Site{Domain: "x", PHPVersion: "8.4", SystemUser: "cg-site-1"}, "/usr/sbin"); err == nil {
		t.Error("a unit without a root was rendered")
	}
}

// SPEC §4: PHP reads the JIT mode only when the master starts, so it is on
// the master's command line for every site type; in the pool it was
// ignored and the JIT stayed off (issue #10).
func TestJITOnMasterCommandLine(t *testing.T) {
	for _, ty := range []state.SiteType{state.SiteWordPress, state.SiteWooCommerce, state.SitePHP} {
		s := site(ty, state.ProfileCommerce)
		_, unit, err := RenderUnit(s, "/usr/sbin")
		if err != nil {
			t.Fatal(err)
		}
		exec := ""
		for l := range strings.Lines(unit) {
			if strings.HasPrefix(l, "ExecStart=") {
				exec = l
			}
		}
		if !strings.Contains(exec, " -d opcache.jit=tracing ") || !strings.Contains(exec, " -d opcache.jit_buffer_size=") {
			t.Errorf("%s: the JIT mode and buffer must be on the master's command line: %q", ty, exec)
		}
		_, pool, err := RenderPool(s, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(pool, "[opcache.jit]") {
			t.Errorf("%s: the pool sets opcache.jit, which PHP ignores after startup", ty)
		}
	}
}

// SPEC §4.1: the master has an IPC namespace of its own, on top of the
// whole Sandbox, and not the run-only hardening that is not measured there.
func TestMasterHasPrivateIPC(t *testing.T) {
	s := site(state.SiteWooCommerce, state.ProfileCommerce)
	_, unit, err := RenderUnit(s, "/usr/sbin")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range append(Sandbox(s), "PrivateIPC=yes") {
		if !strings.Contains(unit, "\n"+l+"\n") {
			t.Errorf("the master's unit lacks %q", l)
		}
	}
	for _, l := range []string{"SystemCallFilter=", "RestrictSUIDSGID="} {
		if strings.Contains(unit, l) {
			t.Errorf("the master's unit has %q, which is not measured on the request path", l)
		}
	}
}
