package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOSExecCapturesStdoutAndStderrSeparately(t *testing.T) {
	res, err := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Fatalf("stdout %q stderr %q", res.Stdout, res.Stderr)
	}
}

func TestOSExecExitErrorCarriesCodeAndStderr(t *testing.T) {
	_, err := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo nope >&2; exit 9"}})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.Code != 9 || ee.Stderr != "nope\n" || ExitCode(err) != 9 {
		t.Fatalf("ExitError = %+v", ee)
	}
	if !strings.Contains(err.Error(), "exit status 9") {
		t.Fatalf("message %q", err)
	}
}

func TestOSExecMissingBinary(t *testing.T) {
	_, err := OSExec{}.Run(context.Background(), Cmd{Name: "cg-no-such-binary-xyz"})
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err = %v, want exec.ErrNotFound", err)
	}
	if ExitCode(err) != -1 {
		t.Fatal("a missing binary has no exit code")
	}
}

func TestOSExecStdinEnvDirAndStreaming(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res, err := OSExec{}.Run(context.Background(), Cmd{
		Name:   "sh",
		Args:   []string{"-c", `read x; echo "$x/$CG_ONLY/$HOME/$(pwd)"`},
		Stdin:  strings.NewReader("secret\n"),
		Env:    []string{"CG_ONLY=1", "PATH=" + os.Getenv("PATH")},
		Dir:    dir,
		Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 0 {
		t.Fatal("streamed stdout was also captured")
	}
	real, _ := filepath.EvalSymlinks(dir)
	if got := strings.TrimSpace(out.String()); got != "secret/1//"+real {
		t.Fatalf("got %q (env must be exactly Cmd.Env: no HOME)", got)
	}
}

// A timeout kills the whole process group, a backgrounded grandchild
// included, and is reported as a deadline, not as "signal: killed".
func TestOSExecTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := OSExec{}.Run(context.Background(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", "sleep 30 & echo $! > " + pidFile + "; wait"},
		Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v: the group was not killed", d)
	}
	if runtime.GOOS != "linux" {
		return
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(2 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		// Gone, or a zombie waiting for a reaper: either way not running.
		if err != nil || zombie(stat) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still running: %s", pid, stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func zombie(stat []byte) bool {
	i := bytes.LastIndexByte(stat, ')')
	f := strings.Fields(string(stat[i+1:]))
	return len(f) > 0 && f[0] == "Z"
}

func TestOSExecCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, err := OSExec{}.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

// gone waits until pid has exited (or is a zombie), failing after 3 s.
func gone(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || zombie(stat) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running: %s", pid, stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// waitFile waits for path to exist.
func waitFile(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

// With Interrupt, a cancelled command gets SIGINT and the time to clean up
// (restic removes its lock), its group's helper stays up meanwhile, and the
// helper goes once the command has exited.
func TestOSExecInterruptLetsTheCommandCleanUp(t *testing.T) {
	dir := t.TempDir()
	lock, helper, ready, sawHelper := filepath.Join(dir, "lock"), filepath.Join(dir, "helper"), filepath.Join(dir, "ready"), filepath.Join(dir, "saw-helper")
	// The trap checks the helper is still alive when it runs, then removes
	// the "lock" and exits 130, as restic does.
	script := `touch ` + lock + `; sleep 30 & echo $! > ` + helper + `
trap 'kill -0 $(cat ` + helper + `) && touch ` + sawHelper + `; rm -f ` + lock + `; exit 130' INT
touch ` + ready + `
while :; do sleep 0.05; done`
	ctx, cancel := context.WithCancel(context.Background())
	go func() { waitFile(t, ready); cancel() }()
	start := time.Now()
	_, err := OSExec{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", script}, Interrupt: 5 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v: the command was not interrupted, it waited for the kill", d)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("the command was killed before it could remove its lock")
	}
	if _, err := os.Stat(sawHelper); err != nil {
		t.Fatal("the helper in the command's group was killed before the command had cleaned up")
	}
	gone(t, readPid(t, helper))
}

// A command that ignores SIGINT is killed, with its group, when its grace
// runs out; a timeout interrupts the same way as a cancellation.
func TestOSExecInterruptThenKill(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	start := time.Now()
	_, err := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c",
		`trap '' INT; sleep 30 & echo $! > ` + child + `; while :; do sleep 0.05; done`},
		Timeout: 300 * time.Millisecond, Interrupt: 500 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	d := time.Since(start)
	if d < 700*time.Millisecond {
		t.Fatalf("killed after %v: the grace was not given", d)
	}
	if d > 5*time.Second {
		t.Fatalf("took %v: not killed when the grace ran out", d)
	}
	gone(t, readPid(t, child))
}

// waitExit returns once the child has exited but leaves it a zombie: its
// pid, the group's id Run then signals, cannot have been reused, and Wait
// still collects its status.
func TestWaitExitLeavesAZombie(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitExit(cmd.Process.Pid)
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat")
	if err != nil || !zombie(stat) {
		t.Fatalf("after waitExit the child is not a zombie: %q %v", stat, err)
	}
	if err := cmd.Wait(); exitCodeOf(err) != 7 {
		t.Fatalf("Wait after waitExit = %v, want exit 7", err)
	}
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := &tailBuffer{limit: 4}
	tb.Write([]byte("abc"))
	tb.Write([]byte("defg"))
	if got := string(tb.Bytes()); got != "defg" {
		t.Fatalf("got %q", got)
	}
}

func TestFakeExecRecordsAndAnswers(t *testing.T) {
	f := &FakeExec{Respond: func(c FakeCall) FakeReply {
		if c.Name == "bad" {
			return FakeReply{Code: 6, Stderr: "no"}
		}
		return FakeReply{Stdout: "ok:" + c.Stdin}
	}}
	res, err := f.Run(context.Background(), Cmd{Name: "good", Args: []string{"a"}, Stdin: strings.NewReader("in")})
	if err != nil || string(res.Stdout) != "ok:in" {
		t.Fatalf("%q %v", res.Stdout, err)
	}
	_, err = f.Run(context.Background(), Cmd{Name: "bad"})
	if ExitCode(err) != 6 {
		t.Fatalf("err = %v", err)
	}
	if !f.Ran("good a") || len(f.Calls()) != 2 {
		t.Fatalf("calls %+v", f.Calls())
	}
}
