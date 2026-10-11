package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A sandboxed files backup is a transient root service whose namespace
// holds an empty root plus the site's tree (read-only), restic's own paths
// and the system files restic needs; the secrets go on stdin, never in
// argv or a unit property.
func TestRunSandboxedArgv(t *testing.T) {
	fx := &FakeExec{}
	r := &ResticCLI{Exec: fx, Transient: true}
	repo := Repo{URL: "/var/backups/cloudground/repo", Password: "s3cret", Env: map[string]string{"AWS_ACCESS_KEY_ID": "AK"}}
	sb := TreeSandbox{Tree: "/srv/sites/a.example.com", Writable: []string{"/var/cache/cloudground/restic", repo.URL}}
	if _, err := r.RunSandboxed(context.Background(), repo, sb, "backup", "--json", "/srv/sites/a.example.com"); err != nil {
		t.Fatal(err)
	}
	calls := fx.Calls()
	if len(calls) != 1 || calls[0].Name != "systemd-run" {
		t.Fatalf("calls: %+v", calls)
	}
	argv := calls[0].Args
	joined := strings.Join(argv, "\n")
	for _, want := range []string{
		"--uid=root", "--pipe", "--wait",
		"TemporaryFileSystem=/", "BindReadOnlyPaths=/srv/sites/a.example.com",
		"BindPaths=/var/cache/cloudground/restic", "BindPaths=/var/backups/cloudground/repo",
		"CapabilityBoundingSet=CAP_DAC_READ_SEARCH", "NoNewPrivileges=yes", "KillSignal=SIGINT",
		"BindReadOnlyPaths=/usr", "BindReadOnlyPaths=-/etc/ssl",
	} {
		if !slices.Contains(argv, want) && !slices.Contains(argv, "-p"+want) {
			t.Errorf("argv lacks %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"s3cret", "AK", "/srv/sites\n", "BindReadOnlyPaths=/srv/sites\n", "BindReadOnlyPaths=/etc\n"} {
		if strings.Contains(joined+"\n", bad) {
			t.Errorf("argv carries %q", bad)
		}
	}
	tail := argv[slices.Index(argv, "--"):]
	if !slices.Equal(tail, []string{"--", "/usr/local/bin/panel-agent", ResticExecName, "--", "backup", "--json", "/srv/sites/a.example.com"}) {
		t.Errorf("command: %q", tail)
	}
	for _, want := range []string{"RESTIC_REPOSITORY=/var/backups/cloudground/repo\x00", "RESTIC_PASSWORD=s3cret\x00", "AWS_ACCESS_KEY_ID=AK\x00"} {
		if !strings.Contains(calls[0].Stdin, want) {
			t.Errorf("stdin lacks %q", want)
		}
	}
}

// Paths that would break out of a unit-file line are refused.
func TestRunSandboxedRefusesBadPaths(t *testing.T) {
	r := &ResticCLI{Exec: &FakeExec{}, Transient: true}
	repo := Repo{URL: "/r", Password: "p"}
	for _, sb := range []TreeSandbox{
		{Tree: "relative"}, {Tree: "/"}, {Tree: "/srv/sites/a b"},
		{Tree: "/srv/sites/a", Writable: []string{"/x y"}}, {Tree: "/srv/sites/a", Writable: []string{"/"}},
	} {
		if _, err := r.RunSandboxed(context.Background(), repo, sb, "backup"); err == nil {
			t.Errorf("%+v accepted", sb)
		}
	}
}

// restic-exec takes only restic's own variables.
func TestResticExecRefusesOtherVariables(t *testing.T) {
	if code := ResticExecMain([]string{"--", "version"}, strings.NewReader("LD_PRELOAD=/x\x00")); code != 2 {
		t.Errorf("exit %d", code)
	}
	if code := ResticExecMain([]string{"version"}, strings.NewReader("")); code != 2 {
		t.Errorf("no --: exit %d", code)
	}
}

// stopOrderExec is an Exec whose systemd-run runs until its context ends or
// a `systemctl stop` of its unit releases it (systemd stopping the service,
// after which systemd-run returns with restic's status). It records whether
// systemd-run was still running when the first stop came.
type stopOrderExec struct {
	ignoreStop bool // a unit that does not end on its stop

	mu            sync.Mutex
	release       chan struct{}
	returned      chan struct{}
	stops         int
	runningAtStop bool
	killed        bool
}

func newStopOrderExec(ignoreStop bool) *stopOrderExec {
	return &stopOrderExec{ignoreStop: ignoreStop, release: make(chan struct{}), returned: make(chan struct{})}
}

func (e *stopOrderExec) Run(ctx context.Context, c Cmd) (Result, error) {
	switch c.Name {
	case "systemd-run":
		defer close(e.returned)
		select {
		case <-e.release:
			return Result{}, &ExitError{Name: c.Name, Code: 130}
		case <-ctx.Done():
			e.mu.Lock()
			e.killed = true
			e.mu.Unlock()
			return Result{}, fmt.Errorf("systemd-run: %w", ctx.Err())
		}
	case "systemctl":
		e.mu.Lock()
		defer e.mu.Unlock()
		e.stops++
		if e.stops == 1 {
			select {
			case <-e.returned:
			default:
				e.runningAtStop = true
			}
			if !e.ignoreStop {
				close(e.release)
			}
		}
	}
	return Result{}, nil
}

// R-4.6: a cancelled sandboxed backup stops its unit while systemd-run is
// still up. systemd-run holds restic's stdout and stderr (--pipe): killed
// first, it left restic writing into closed pipes, and restic died of
// SIGPIPE before removing its repository lock.
func TestRunSandboxedCancelStopsUnitBeforeSystemdRun(t *testing.T) {
	e := newStopOrderExec(false)
	r := &ResticCLI{Exec: e, Transient: true}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := r.RunSandboxed(ctx, Repo{URL: "/r", Password: "p"}, TreeSandbox{Tree: "/srv/sites/a"}, "backup")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stops == 0 {
		t.Fatal("the unit was never stopped")
	}
	if !e.runningAtStop || e.killed {
		t.Errorf("systemd-run running at the stop: %v, killed: %v; want running, not killed", e.runningAtStop, e.killed)
	}
}

// A unit that outlives its stop has systemd-run killed in the end, after
// the stop was retried.
func TestRunSandboxedCancelKillsSystemdRunPastTheWait(t *testing.T) {
	defer func(w, rt time.Duration) { sandboxStopWait, stopRetry = w, rt }(sandboxStopWait, stopRetry)
	sandboxStopWait, stopRetry = 200*time.Millisecond, 20*time.Millisecond
	e := newStopOrderExec(true)
	r := &ResticCLI{Exec: e, Transient: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.RunSandboxed(ctx, Repo{URL: "/r", Password: "p"}, TreeSandbox{Tree: "/srv/sites/a"}, "backup")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.killed || e.stops < 2 {
		t.Errorf("systemd-run killed: %v, stops: %d; want killed after several stops", e.killed, e.stops)
	}
}
