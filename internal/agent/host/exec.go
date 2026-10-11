// Package host holds the agent's adapters over the operating system:
// systemd, user accounts, MariaDB, nginx, restic, wp-cli, ufw and certbot.
// Each is an interface with a real implementation and a fake for tests.
// SPEC.md in this directory is the contract they are written from.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultTimeout bounds a command whose Cmd and runner set no timeout.
const DefaultTimeout = 10 * time.Minute

// stderrLimit is how much of a command's stderr is kept: the tail, where
// tools put the line that explains the failure.
const stderrLimit = 64 << 10

// Cmd is one command to run. Args are an argv array: there is never a shell.
type Cmd struct {
	Name string
	Args []string
	// Env nil inherits the agent's environment; non-nil is exactly the
	// child's environment.
	Env   []string
	Dir   string
	Stdin io.Reader
	// Stdout nil captures stdout into Result.Stdout; otherwise it is
	// streamed here and Result.Stdout is empty.
	Stdout  io.Writer
	Timeout time.Duration
	// Interrupt, when set, is how long a cancelled or timed-out command may
	// take to stop on its own: it is sent SIGINT first (the process alone,
	// not its group, so a helper it talks to, an SFTP backend's ssh say,
	// stays up while it cleans up), and the whole group is killed when the
	// time is up or it has exited. Zero kills the group at once.
	Interrupt time.Duration
}

// Result is what a successful command produced.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// Exec runs commands.
type Exec interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// ExitError is a command that ran and exited non-zero (Code), or was killed
// by a signal (Code -1). Stderr is for people; nothing classifies on it.
type ExitError struct {
	Name   string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s: exit status %d", e.Name, e.Code)
	if e.Code < 0 {
		msg = e.Name + ": killed by a signal"
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// ExitCode is err's exit status if it is (or wraps) an *ExitError, else -1.
func ExitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

// OSExec runs real processes.
type OSExec struct {
	// Timeout applies when the Cmd sets none; zero means DefaultTimeout.
	Timeout time.Duration
}

// Run starts c in its own process group and waits for it. Cancelling ctx, or
// reaching the timeout, kills the whole group: a tool's children (wp-cli's
// PHP, certbot's hooks) go with it. With c.Interrupt the tool is first asked
// to stop (SIGINT) and given that long to do so.
func (o OSExec) Run(ctx context.Context, c Cmd) (Result, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = o.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The group is signalled only while its leader is unreaped (alive or a
	// zombie): its pid, which is the group's id (Setpgid above), cannot be
	// reused until then. exited is set once the leader has exited, before
	// Wait reaps it; mu orders the kills against it.
	var (
		mu          sync.Mutex
		exited      bool
		interrupted bool
		grace       *time.Timer
	)
	killGroup := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if cmd.Process == nil || exited {
			return nil
		}
		if c.Interrupt <= 0 {
			return killGroup()
		}
		interrupted = true
		grace = time.AfterFunc(c.Interrupt, func() {
			mu.Lock()
			defer mu.Unlock()
			if !exited {
				_ = killGroup()
			}
		})
		return syscall.Kill(cmd.Process.Pid, syscall.SIGINT)
	}
	// A grandchild that inherited stdout must not keep Wait blocked after
	// the group was killed or the tool itself exited. An interrupted
	// command has its grace first.
	cmd.WaitDelay = 5*time.Second + max(c.Interrupt, 0)

	var stdout bytes.Buffer
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = &stdout
	}
	stderr := &tailBuffer{limit: stderrLimit}
	cmd.Stderr = stderr

	err := cmd.Start()
	if err == nil {
		waitExit(cmd.Process.Pid)
		mu.Lock()
		exited = true
		if grace != nil {
			grace.Stop()
		}
		if interrupted || ctx.Err() != nil {
			// Stopped: what it left in its group goes too, while the
			// leader, a zombie, still holds the group's id.
			_ = killGroup()
		}
		mu.Unlock()
		err = cmd.Wait()
	}
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("%s: %w", c.Name, ctxErr)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return res, &ExitError{Name: c.Name, Code: ee.ExitCode(), Stderr: string(res.Stderr)}
	}
	return res, fmt.Errorf("%s: %w", c.Name, err)
}

// waitExit blocks until the child pid has exited, without reaping it
// (waitid with WNOWAIT): it stays a zombie, its pid and process group id
// reserved, until Wait collects it.
func waitExit(pid int) {
	for {
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != unix.EINTR {
			return
		}
	}
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

// FakeCall is one command a FakeExec was asked to run.
type FakeCall struct {
	Name      string
	Args      []string
	Env       []string
	Stdin     string
	Timeout   time.Duration // the Cmd's own, zero when it set none
	Interrupt time.Duration // the Cmd's own
}

// Line is the call as one space-joined string, for assertions.
func (c FakeCall) Line() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// FakeReply is how a FakeExec answers a call.
type FakeReply struct {
	Stdout string
	Stderr string
	Code   int   // non-zero becomes an *ExitError
	Err    error // returned as is (a missing binary, a timeout)
}

// FakeExec records calls and answers them through Respond (nil: success,
// no output). It is safe for concurrent use.
type FakeExec struct {
	Respond func(FakeCall) FakeReply

	mu    sync.Mutex
	calls []FakeCall
}

// Run implements Exec.
func (f *FakeExec) Run(ctx context.Context, c Cmd) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("%s: %w", c.Name, err)
	}
	call := FakeCall{Name: c.Name, Args: append([]string(nil), c.Args...), Env: append([]string(nil), c.Env...), Timeout: c.Timeout, Interrupt: c.Interrupt}
	if c.Stdin != nil {
		b, err := io.ReadAll(c.Stdin)
		if err != nil {
			return Result{}, err
		}
		call.Stdin = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	respond := f.Respond
	f.mu.Unlock()
	var r FakeReply
	if respond != nil {
		r = respond(call)
	}
	if r.Err != nil {
		return Result{}, r.Err
	}
	res := Result{Stderr: []byte(r.Stderr)}
	if c.Stdout != nil {
		if _, err := io.WriteString(c.Stdout, r.Stdout); err != nil {
			return res, err
		}
	} else {
		res.Stdout = []byte(r.Stdout)
	}
	if r.Code != 0 {
		return res, &ExitError{Name: c.Name, Code: r.Code, Stderr: r.Stderr}
	}
	return res, nil
}

// Calls returns a copy of every call so far.
func (f *FakeExec) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}

// Ran reports whether a call's Line() equals line.
func (f *FakeExec) Ran(line string) bool {
	for _, c := range f.Calls() {
		if c.Line() == line {
			return true
		}
	}
	return false
}
