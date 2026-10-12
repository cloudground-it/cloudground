package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/units"
)

func helperLook(name string) (string, error) { return "/usr/bin/" + name, nil }

// argvAfter returns the command after systemd-run's "--".
func argvAfter(argv []string) []string { return argv[slices.Index(argv, "--")+1:] }

// hasProp reports whether argv sets the unit line p ("-p", p).
func hasProp(argv []string, p string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-p" && argv[i+1] == p {
			return true
		}
	}
	return false
}

// #66: a helper runs in a transient unit of its own in cg-helper.slice,
// with the agent's sandbox and its own memory limit; its environment (with
// restic's secrets) and its stdin data go on stdin, never in argv.
func TestHelperExecRunsInItsOwnUnit(t *testing.T) {
	fx := &FakeExec{}
	h := HelperExec{Exec: fx, LookPath: helperLook}
	_, err := h.Run(context.Background(), Cmd{Name: "restic", Args: []string{"backup", "--stdin", "--json"},
		Env: []string{"PATH=/usr/bin", "RESTIC_PASSWORD=s3cret"}, Stdin: strings.NewReader("DUMP"),
		Stdout: io.Discard, Interrupt: ResticGrace, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	calls := fx.Calls()
	if len(calls) != 1 || calls[0].Name != "systemd-run" {
		t.Fatalf("calls: %+v", calls)
	}
	argv := calls[0].Args
	for _, want := range []string{"--wait", "--pipe", "--collect", "--uid=root", "--slice=" + HelperSlice} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %q: %q", want, argv)
		}
	}
	for _, p := range append(slices.Clone(HelperProps), "MemoryMax=75%", "KillMode=mixed", "KillSignal=SIGINT", "TimeoutStopSec=20") {
		if !hasProp(argv, p) {
			t.Errorf("argv lacks -p %q", p)
		}
	}
	if got := argvAfter(argv); !slices.Equal(got, []string{"/usr/local/bin/panel-agent", HelperExecName, "--", "/usr/bin/restic", "backup", "--stdin", "--json"}) {
		t.Errorf("command: %q", got)
	}
	if strings.Contains(strings.Join(argv, " "), "s3cret") {
		t.Error("a secret is in argv")
	}
	if want := "PATH=/usr/bin\x00RESTIC_PASSWORD=s3cret\x00\x00DUMP"; calls[0].Stdin != want {
		t.Errorf("stdin = %q, want %q", calls[0].Stdin, want)
	}
	if calls[0].Timeout <= time.Hour {
		t.Errorf("systemd-run's own timeout %v would end it before the tool's deadline", calls[0].Timeout)
	}
}

// Without Interrupt a stop kills the unit's whole cgroup at once, as OSExec
// kills the process group; a nil Env is the agent's own environment.
func TestHelperExecKillsAtOnceAndInheritsTheEnvironment(t *testing.T) {
	t.Setenv("CG_HELPER_TEST", "inherited")
	fx := &FakeExec{}
	h := HelperExec{Exec: fx, LookPath: helperLook}
	if _, err := h.Run(context.Background(), Cmd{Name: "certbot", Args: []string{"delete"}}); err != nil {
		t.Fatal(err)
	}
	c := fx.Calls()[0]
	if !hasProp(c.Args, "KillSignal=SIGKILL") || hasProp(c.Args, "KillMode=mixed") {
		t.Errorf("argv: %q", c.Args)
	}
	if !strings.Contains(c.Stdin, "\x00CG_HELPER_TEST=inherited\x00") && !strings.HasPrefix(c.Stdin, "CG_HELPER_TEST=inherited\x00") {
		t.Errorf("the agent's environment is not passed: %q", c.Stdin)
	}
	if !strings.HasSuffix(c.Stdin, "\x00\x00") {
		t.Errorf("no stdin data, the environment ends the stream: %q", c.Stdin)
	}
}

// Every other command runs as it did, in the agent.
func TestHelperExecPassesOtherCommands(t *testing.T) {
	fx := &FakeExec{}
	h := HelperExec{Exec: fx, LookPath: helperLook}
	if _, err := h.Run(context.Background(), Cmd{Name: "nginx", Args: []string{"-t"}}); err != nil {
		t.Fatal(err)
	}
	if !fx.Ran("nginx -t") || len(fx.Calls()) != 1 {
		t.Errorf("calls: %+v", fx.Calls())
	}
}

// The error a caller gets is the one OSExec gives: the tool's exit status
// under the tool's name (restic's typed codes still classify), a missing
// binary as exec.ErrNotFound with nothing started.
func TestHelperExecReportsLikeOSExec(t *testing.T) {
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		return FakeReply{Code: 3, Stderr: "some files unreadable"}
	}}
	r := &ResticCLI{Exec: HelperExec{Exec: fx, LookPath: helperLook}, BaseEnv: []string{}}
	err := r.RunIO(context.Background(), Repo{URL: "/r", Password: "p"}, nil, io.Discard, "backup")
	var ee *ExitError
	if !errors.Is(err, ErrIncomplete) || !errors.As(err, &ee) || ee.Name != "restic" || ee.Code != 3 || ee.Stderr != "some files unreadable" {
		t.Errorf("err = %v", err)
	}

	fx = &FakeExec{}
	h := HelperExec{Exec: fx, LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	_, err = h.Run(context.Background(), Cmd{Name: "mariadb-dump"})
	if !errors.Is(err, exec.ErrNotFound) || !strings.HasPrefix(err.Error(), "mariadb-dump: ") || len(fx.Calls()) != 0 {
		t.Errorf("err = %v, calls %d", err, len(fx.Calls()))
	}
}

// A cancelled helper has its unit stopped while systemd-run is still up
// (it holds the tool's stdout), and the call fails with the context's
// error under the tool's name, as OSExec's does.
func TestHelperExecCancelStopsTheUnit(t *testing.T) {
	e := newStopOrderExec(false)
	h := HelperExec{Exec: e, LookPath: helperLook}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := h.Run(ctx, Cmd{Name: "restic", Interrupt: ResticGrace})
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "restic: ") {
		t.Errorf("err = %v, want restic: context canceled", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stops == 0 || !e.runningAtStop || e.killed {
		t.Errorf("stops %d, systemd-run running at the stop %v, killed %v", e.stops, e.runningAtStop, e.killed)
	}
}

// A helper past its timeout is stopped the same way, with
// DeadlineExceeded; one that outlives its stop has systemd-run killed.
func TestHelperExecTimeoutStopsTheUnit(t *testing.T) {
	defer func(w, r time.Duration) { helperStopWait, stopRetry = w, r }(helperStopWait, stopRetry)
	helperStopWait, stopRetry = 200*time.Millisecond, 20*time.Millisecond
	e := newStopOrderExec(true)
	h := HelperExec{Exec: e, LookPath: helperLook}
	_, err := h.Run(context.Background(), Cmd{Name: "mariadb-dump", Timeout: 50 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "mariadb-dump: ") {
		t.Errorf("err = %v", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.killed || e.stops < 2 {
		t.Errorf("killed %v, stops %d; want killed after several stops", e.killed, e.stops)
	}
}

// A captured stdout past StdoutLimit fails the call with ErrStdoutTooLarge,
// the first StdoutLimit bytes kept, and stops the unit, which systemd-run's
// end would not.
func TestHelperExecStdoutLimitStopsTheUnit(t *testing.T) {
	big := strings.Repeat("x", StdoutLimit+1)
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		if c.Name == "systemd-run" {
			return FakeReply{Stdout: big}
		}
		return FakeReply{}
	}}
	h := HelperExec{Exec: fx, LookPath: helperLook}
	res, err := h.Run(context.Background(), Cmd{Name: "certbot"})
	if !errors.Is(err, ErrStdoutTooLarge) || !strings.HasPrefix(err.Error(), "certbot: ") || len(res.Stdout) != StdoutLimit {
		t.Errorf("err = %v, %d bytes", err, len(res.Stdout))
	}
	calls := fx.Calls()
	unit := ""
	for _, a := range calls[0].Args {
		if strings.HasPrefix(a, "--unit=") {
			unit = strings.TrimPrefix(a, "--unit=")
		}
	}
	if len(calls) != 2 || calls[1].Line() != "systemctl stop "+unit || !strings.HasPrefix(unit, "cg-helper-certbot-") {
		t.Errorf("calls: %+v", calls)
	}
}

// The sandboxed files backup runs in the helpers' slice too.
func TestRunSandboxedInTheHelperSlice(t *testing.T) {
	fx := &FakeExec{}
	r := &ResticCLI{Exec: fx, Transient: true}
	if _, err := r.RunSandboxed(context.Background(), Repo{URL: "/r", Password: "p"}, TreeSandbox{Tree: "/srv/sites/a"}, "backup"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fx.Calls()[0].Args, "--slice="+HelperSlice) {
		t.Errorf("argv: %q", fx.Calls()[0].Args)
	}
}

// agentOnly are cloudground-agent.service's lines that are about the agent
// itself, not the sandbox its helpers inherited.
var agentOnly = map[string]bool{
	"Type": true, "EnvironmentFile": true, "Environment": true, "ExecStart": true, "ExecStartPre": true, "TimeoutStartSec": true, "Restart": true, "RestartSec": true,
	"RuntimeDirectory": true, "RuntimeDirectoryMode": true, "RuntimeDirectoryPreserve": true,
	"MemoryHigh": true, "MemoryMax": true, "OOMPolicy": true,
}

// HelperProps is the agent unit's sandbox, line for line: a helper moved
// out of the agent's cgroup keeps every restriction it had in it, and a
// line added to the unit later reaches the helpers too.
func TestHelperPropsAreTheAgentsSandbox(t *testing.T) {
	b, ok := units.Get("cloudground-agent.service")
	if !ok {
		t.Fatal("no agent unit")
	}
	var sandbox []string
	inService := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			inService = line == "[Service]"
		case inService:
			k, _, _ := strings.Cut(line, "=")
			if !agentOnly[k] {
				sandbox = append(sandbox, line)
			}
		}
	}
	if len(sandbox) < 10 {
		t.Fatalf("only %d sandbox lines read: %q", len(sandbox), sandbox)
	}
	props := slices.DeleteFunc(slices.Clone(HelperProps), func(p string) bool { return strings.HasPrefix(p, "MemoryMax=") })
	if !slices.Equal(sandbox, props) {
		t.Errorf("HelperProps differ from the agent unit's sandbox:\nunit:  %q\nprops: %q", sandbox, props)
	}
}

func TestReadHelperEnvLeavesTheRest(t *testing.T) {
	r := strings.NewReader("A=1\x00B=x=y\x00\x00the tool's stdin")
	env, err := readHelperEnv(r)
	if err != nil || !slices.Equal(env, []string{"A=1", "B=x=y"}) {
		t.Fatalf("env %q, err %v", env, err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "the tool's stdin" {
		t.Errorf("rest = %q", rest)
	}
	for _, bad := range []string{"", "A=1\x00", "NOEQUALS\x00\x00"} {
		if _, err := readHelperEnv(strings.NewReader(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := helperEnv([]string{"A=1\x00B=2"}); err == nil {
		t.Error("a NUL inside an entry accepted")
	}
}

func TestHelperExecMainUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"--"}, {"/bin/sh"}, {"--", "sh"}} {
		if code := HelperExecMain(args, os.Stdin); code != 126 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

// helper-exec, run for real: the tool gets exactly the environment sent
// and, on its stdin, what followed it.
func TestHelperExecMainExecsTheTool(t *testing.T) {
	if os.Getenv("CG_HELPER_EXEC_CHILD") == "1" {
		os.Exit(HelperExecMain([]string{"--", "/bin/sh", "-c", `printf '%s|%s|' "$CG_X" "${CG_HELPER_EXEC_CHILD:-unset}"; cat`}, os.Stdin))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExecMainExecsTheTool$")
	cmd.Env = append(os.Environ(), "CG_HELPER_EXEC_CHILD=1")
	cmd.Stdin = strings.NewReader("CG_X=hello\x00PATH=/usr/bin:/bin\x00\x00payload")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != "hello|unset|payload" {
		t.Errorf("output %q", out)
	}
}
