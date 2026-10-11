package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func wpSite() state.Site {
	return state.Site{ID: 12, Domain: "shop.example.com", Type: state.SiteWordPress, SystemUser: "cg-site-12", RootPath: "/srv/sites/shop.example.com"}
}

func newOps(t *testing.T) (*Ops, *host.FakeExec, *host.FakeSystemd) {
	ex, sd := &host.FakeExec{}, host.NewFakeSystemd()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, agentBinary), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Ops{Exec: ex, Systemd: sd, CrontabDir: t.TempDir(), CronDir: t.TempDir(),
		SpecDir: filepath.Join(t.TempDir(), "spec"), BinDir: bin, RunDir: filepath.Join(t.TempDir(), "runs")}, ex, sd
}

func TestRender(t *testing.T) {
	jobs := []cronjob.Job{
		{ID: 9, Schedule: "0 3 * * *", Command: "php artisan schedule:run", Enabled: true},
		{ID: 3, Schedule: "*/15  * * * *", Command: "echo 50%", Enabled: true},
		{ID: 5, Schedule: "* * * * *", Command: "echo off", Enabled: false},
	}
	out, spec, n, err := Render(wpSite(), jobs, "/usr/local/bin")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if n != 3 || len(lines) != 6 {
		t.Fatalf("%d entries:\n%s", n, out)
	}
	if lines[1] != `MAILTO=""` || !strings.HasPrefix(lines[2], "PATH=") {
		t.Fatalf("header:\n%s", out)
	}
	// Each line runs the runner as root; no command text is in the file.
	if lines[3] != "2-59/5 * * * * root /usr/local/bin/cg-site-cron cg-site-12 wp" ||
		lines[4] != "*/15 * * * * root /usr/local/bin/cg-site-cron cg-site-12 3" ||
		lines[5] != "0 3 * * * root /usr/local/bin/cg-site-cron cg-site-12 9" {
		t.Fatalf("lines:\n%s", out)
	}
	if strings.Contains(out, "echo") || strings.Contains(out, "%") {
		t.Fatalf("a job's command reached the cron file:\n%s", out)
	}
	// The spec holds the enabled jobs' commands, verbatim.
	if !spec.WordPress || spec.Jobs["3"] != "echo 50%" || spec.Jobs["9"] != "php artisan schedule:run" || len(spec.Jobs) != 2 {
		t.Fatalf("spec %+v", spec)
	}

	static := wpSite()
	static.Type = state.SiteStatic
	if out, _, n, err := Render(static, jobs[2:], "/usr/local/bin"); err != nil || n != 0 || out != "" {
		t.Fatalf("%q %d %v", out, n, err)
	}
}

// One bad job refuses the request, naming it; nothing is written.
func TestRenderRefuses(t *testing.T) {
	for name, jobs := range map[string][]cronjob.Job{
		"newline in schedule": {{ID: 1, Schedule: "* * * * *\n* * * * * id", Command: "true", Enabled: true}},
		"out of range":        {{ID: 1, Schedule: "61 * * * *", Command: "true", Enabled: true}},
		"newline in command":  {{ID: 1, Schedule: "* * * * *", Command: "true\nid", Enabled: true}},
		"disabled but bad":    {{ID: 1, Schedule: "* * * * * *", Command: "true"}},
		"duplicate":           {{ID: 1, Schedule: "* * * * *", Command: "a", Enabled: true}, {ID: 1, Schedule: "* * * * *", Command: "b", Enabled: true}},
		"too long":            {{ID: 1, Schedule: "* * * * *", Command: strings.Repeat("x", 1000), Enabled: true}},
		"no id":               {{Schedule: "* * * * *", Command: "true", Enabled: true}},
	} {
		o, _, _ := newOps(t)
		_, err := o.Apply(context.Background(), rpc.CronParams{Site: wpSite(), Jobs: jobs})
		if agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%s: err = %v", name, err)
		}
		if ents, _ := os.ReadDir(o.CronDir); len(ents) != 0 {
			t.Errorf("%s: a cron file was written", name)
		}
	}
}

// Apply writes the spec (root's, 0600) and the cron.d file (0644), links
// the runner, and removes a per-user crontab from before; with nothing to
// schedule it removes all of it. crontab(1) never runs. The modes hold under
// the agent's UMask=0027: cron ignores a cron.d file that is not 0644-ish
// readable, and the umask must not decide it.
func TestApply(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	o, ex, _ := newOps(t)
	s := wpSite()
	s.Type = state.SitePHP
	spool := filepath.Join(o.CrontabDir, "cg-site-12")
	os.WriteFile(spool, []byte("old"), 0o600)
	res, err := o.Apply(context.Background(), rpc.CronParams{Site: s, Jobs: []cronjob.Job{{ID: 1, Schedule: "* * * * *", Command: "true", Enabled: true}}})
	if err != nil || res.Entries != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(ex.Calls()) != 0 {
		t.Fatalf("a tool ran: %v", ex.Calls())
	}
	cf := filepath.Join(o.CronDir, "cloudground-cg-site-12")
	if b, err := os.ReadFile(cf); err != nil || !strings.Contains(string(b), "* * * * * root "+filepath.Join(o.BinDir, ClientName)+" cg-site-12 1\n") {
		t.Fatalf("cron file %q %v", b, err)
	}
	if fi, _ := os.Stat(cf); fi.Mode().Perm() != 0o644 {
		t.Fatalf("cron file mode %v", fi.Mode())
	}
	sf := SpecFile(o.SpecDir, "cg-site-12")
	var spec Spec
	if b, err := os.ReadFile(sf); err != nil || json.Unmarshal(b, &spec) != nil || spec.Jobs["1"] != "true" || spec.Root != s.RootPath {
		t.Fatalf("spec %q %v", b, err)
	}
	if fi, _ := os.Stat(sf); fi.Mode().Perm() != 0o600 {
		t.Fatalf("spec mode %v", fi.Mode())
	}
	if l, err := os.Readlink(filepath.Join(o.BinDir, ClientName)); err != nil || l != agentBinary {
		t.Fatalf("runner link %q %v", l, err)
	}
	if _, err := os.Stat(spool); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the per-user crontab from before stayed")
	}

	res, err = o.Apply(context.Background(), rpc.CronParams{Site: s, Jobs: []cronjob.Job{{ID: 1, Schedule: "* * * * *", Command: "true"}}})
	if err != nil || res.Entries != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, p := range []string{cf, sf} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabling the last job left %s", p)
		}
	}
	if _, err := o.Apply(context.Background(), rpc.CronParams{Site: s}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledArgv(t *testing.T) {
	spec := Spec{User: "cg-site-12", Domain: "shop.example.com", Root: "/srv/sites/shop.example.com", WordPress: true,
		Jobs: map[string]string{"3": "echo 50% done"}}
	now := time.Unix(1_700_000_000, 0)
	argv, err := ScheduledArgv(spec, "3", now, "/srv/sites")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "systemd-run" {
		t.Fatalf("argv %q", argv)
	}
	for _, want := range []string{"--wait", "--uid=cg-site-12", "--slice=cg-site-12.slice", "--unit=cg-cron-cg-site-12-3-1700000000.service",
		"--working-directory=/srv/sites/shop.example.com", "RuntimeMaxSec=21600", "--setenv=HOME=/srv/sites/shop.example.com"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %q", want)
		}
	}
	for _, line := range phpfpm.RunSandbox(state.Site{Domain: spec.Domain, RootPath: spec.Root}) {
		i := slices.Index(argv, line)
		if i < 1 || argv[i-1] != "-p" {
			t.Errorf("sandbox line %q missing", line)
		}
	}
	tail := argv[len(argv)-6:]
	if !slices.Equal(tail, host.SystemdCommand([]string{"/bin/sh", "-c", cronjob.Wrapper, "cg-cron", "echo 50% done", "logs/cron-3.log"})) {
		t.Fatalf("command %q", tail)
	}
	if argv, err := ScheduledArgv(spec, "wp", now, "/srv/sites"); err != nil || argv[len(argv)-1] != "/srv/sites/shop.example.com/current" {
		t.Fatalf("wp: %q %v", argv, err)
	}
	for name, bad := range map[string]func() (Spec, string){
		"unknown job":     func() (Spec, string) { return spec, "4" },
		"not a number":    func() (Spec, string) { return spec, "x" },
		"wp on a non-WP":  func() (Spec, string) { s := spec; s.WordPress = false; return s, "wp" },
		"root elsewhere":  func() (Spec, string) { s := spec; s.Root = "/etc"; return s, "3" },
		"domain mismatch": func() (Spec, string) { s := spec; s.Domain = "other.example.com"; return s, "3" },
		"bad user":        func() (Spec, string) { s := spec; s.User = "root"; return s, "3" },
	} {
		s, which := bad()
		if _, err := ScheduledArgv(s, which, now, "/srv/sites"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRun(t *testing.T) {
	o, ex, sd := newOps(t)
	ex.Respond = func(c host.FakeCall) host.FakeReply {
		return host.FakeReply{Stdout: strings.Repeat("a", outputLimit) + "tail", Code: 4,
			Stderr: "Running as unit: x\nFinished with result: exit-code\nMain processes terminated with: code=exited/status=4\n"}
	}
	res, err := o.Run(context.Background(), rpc.RunCronParams{Site: wpSite(), Job: cronjob.Job{ID: 7, Command: "php x.php"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.ExitCode != 4 || len(res.Output) != outputLimit || !strings.HasSuffix(res.Output, "tail") {
		t.Fatalf("%+v", res.Status)
	}
	c := ex.Calls()[0]
	for _, want := range []string{"--unit=cg-cron-cg-site-12-7.service", "--uid=cg-site-12", "--slice=cg-site-12.slice", "--pipe",
		"--working-directory=/srv/sites/shop.example.com", "RuntimeMaxSec=300", "--setenv=HOME=/srv/sites/shop.example.com",
		"--setenv=PATH=/usr/local/bin:/usr/bin:/bin"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("argv lacks %q: %q", want, c.Line())
		}
	}
	if slices.Contains(c.Args, "--quiet") {
		t.Error("--quiet hides the result line")
	}
	for _, line := range phpfpm.RunSandbox(state.Site{Domain: "shop.example.com", RootPath: "/srv/sites/shop.example.com"}) {
		if !slices.Contains(c.Args, line) {
			t.Errorf("sandbox line %q missing", line)
		}
	}
	if !strings.HasSuffix(c.Line(), "cg-cron php x.php") {
		t.Fatalf("command %q", c.Line())
	}

	for result, want := range map[string]string{"success": "succeeded", "timeout": "timeout", "signal": "failed"} {
		ex.Respond = func(host.FakeCall) host.FakeReply {
			code := 1
			if result == "success" {
				code = 0
			}
			return host.FakeReply{Code: code, Stderr: "Finished with result: " + result + "\n"}
		}
		res, err := o.Run(context.Background(), rpc.RunCronParams{Site: wpSite(), Job: cronjob.Job{ID: 7, Command: "x"}, MaxSeconds: 3})
		if err != nil || res.Status != want {
			t.Errorf("%s: %s %v", result, res.Status, err)
		}
		if result == "signal" && res.ExitCode != -1 {
			t.Errorf("a signal reported exit code %d", res.ExitCode)
		}
	}
	if c := ex.Calls()[len(ex.Calls())-1]; !slices.Contains(c.Args, "RuntimeMaxSec=3") {
		t.Fatalf("MaxSeconds 3: %q", c.Line())
	}
	// systemd 259's summary, verbatim (Ubuntu 26.04): the labels are
	// right-aligned, so the result line starts with blanks. Read as no
	// result line, a timeout came back as "Could not run the cron job".
	for _, c := range []struct {
		stderr, status string
		exit, code     int
	}{
		{"Running as unit: cg-cron-cg-site-12-7.service; invocation ID: accdaa09f48f43f7adf949a9078cf440\n" +
			"          Finished with result: timeout\n" +
			"Main processes terminated with: code=killed, status=15/TERM\n" +
			"               Service runtime: 3.132s\n", "timeout", 1, -1},
		{"Running as unit: run-p550521-i553136.service\n" +
			"          Finished with result: exit-code\n" +
			"Main processes terminated with: code=exited, status=4/NOPERMISSION\n", "failed", 4, 4},
	} {
		ex.Respond = func(host.FakeCall) host.FakeReply { return host.FakeReply{Code: c.exit, Stderr: c.stderr} }
		res, err := o.Run(context.Background(), rpc.RunCronParams{Site: wpSite(), Job: cronjob.Job{ID: 7, Command: "x"}, MaxSeconds: 3})
		if err != nil || res.Status != c.status || res.ExitCode != c.code {
			t.Errorf("systemd 259, %s: %+v %v", c.status, res, err)
		}
	}

	// systemd-run itself failing (no result line): an error.
	ex.Respond = func(host.FakeCall) host.FakeReply {
		return host.FakeReply{Code: 1, Stderr: "Failed to start transient service unit"}
	}
	if _, err := o.Run(context.Background(), rpc.RunCronParams{Site: wpSite(), Job: cronjob.Job{ID: 7, Command: "x"}}); err == nil {
		t.Fatal("a run that never started reported a status")
	}

	// A job still running: conflict.
	sd.SetUnit(host.UnitProps{Name: "cg-cron-cg-site-12-7.service", ActiveState: "active"})
	if _, err := o.Run(context.Background(), rpc.RunCronParams{Site: wpSite(), Job: cronjob.Job{ID: 7, Command: "x"}}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("err = %v", err)
	}
	n := len(ex.Calls())
	for _, j := range []cronjob.Job{{ID: 8, Command: "a\nb"}, {ID: 8, Command: " "}, {ID: 0, Command: "x"}} {
		if _, err := o.Run(context.Background(), rpc.RunCronParams{Site: wpSite(), Job: j}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v: %v", j, err)
		}
	}
	if len(ex.Calls()) != n {
		t.Fatal("an invalid job ran")
	}
}

// Site deletion stops the user's runs in flight and removes the schedule,
// also once the user is gone; another site's runs are left alone.
func TestRemoveSite(t *testing.T) {
	o, _, sd := newOps(t)
	sd.SetUnit(host.UnitProps{Name: "cg-cron-cg-site-12-7.service", ActiveState: "active"})
	sd.SetUnit(host.UnitProps{Name: "cg-cron-cg-site-12-wp-1700000000.service", ActiveState: "active"})
	sd.SetUnit(host.UnitProps{Name: "cg-cron-cg-site-120-7.service", ActiveState: "active"})
	s := wpSite()
	if _, err := o.Apply(context.Background(), rpc.CronParams{Site: s}); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(o.CrontabDir, "cg-site-12")
	os.WriteFile(spool, []byte("x"), 0o600)
	if err := o.RemoveSite(context.Background(), "cg-site-12"); err != nil {
		t.Fatal(err)
	}
	if !sd.Did("stop", "cg-cron-cg-site-12-7.service") || !sd.Did("stop", "cg-cron-cg-site-12-wp-1700000000.service") || sd.Did("stop", "cg-cron-cg-site-120-7.service") {
		t.Fatalf("ops %v", sd.Ops())
	}
	for _, p := range []string{spool, filepath.Join(o.CronDir, "cloudground-cg-site-12"), SpecFile(o.SpecDir, "cg-site-12")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind", p)
		}
	}
	if err := o.RemoveSite(context.Background(), "cg-site-12"); err != nil {
		t.Fatalf("a retried deletion failed: %v", err)
	}
	if err := o.RemoveSite(context.Background(), "../etc"); err == nil {
		t.Fatal("a bad user name reached the files")
	}
}

// Through the server: the site is validated and must still exist.
func TestApplyThroughServer(t *testing.T) {
	sites := t.TempDir()
	o, _, _ := newOps(t)
	s := agentrpc.NewServer(agentrpc.Config{SitesRoot: sites, Locker: liveLocker{}})
	o.Register(s)
	site := wpSite()
	site.RootPath = filepath.Join(sites, site.Domain)
	raw, _ := json.Marshal(rpc.CronParams{Site: site, Jobs: []cronjob.Job{{ID: 1, Schedule: "* * * * *", Command: "true", Enabled: true}}})
	if _, e := s.Invoke(context.Background(), agentrpc.ApplyCron.Name(), raw); e != nil {
		t.Fatal(e)
	}
	var spec Spec
	b, _ := os.ReadFile(SpecFile(o.SpecDir, site.SystemUser))
	if json.Unmarshal(b, &spec) != nil || spec.Root != site.RootPath {
		t.Fatalf("spec %s", b)
	}
	site.SystemUser = "root"
	raw, _ = json.Marshal(rpc.CronParams{Site: site})
	if _, e := s.Invoke(context.Background(), agentrpc.ApplyCron.Name(), raw); e == nil || e.Code != agentrpc.CodeInvalid {
		t.Fatalf("a schedule for root: %v", e)
	}
}

type liveLocker struct{}

func (liveLocker) LockSite(string) func() { return func() {} }
func (liveLocker) LockLiveSite(state.Site) (func(), error) {
	return func() {}, nil
}
