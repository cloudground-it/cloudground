// Package host holds the agent's adapters over the operating system:
// systemd, user accounts, MariaDB, nginx, restic, wp-cli, ufw and certbot.
// Each is an interface with a real implementation and a fake for tests.
// SPEC.md in this directory is the contract they are written from.
package host

import (
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

// StdoutLimit bounds the stdout OSExec.Run captures when the Cmd streams it
// nowhere (SPEC: exec). The commands read this way print kilobytes; a child
// that writes past it, site code under wp-cli say, must not grow the
// agent's heap without end.
const StdoutLimit = 16 << 20

// ErrStdoutTooLarge is a command whose captured stdout passed its limit:
// the command was stopped, and what was kept is incomplete.
var ErrStdoutTooLarge = errors.New("output too large")

// Cmd is one command to run. Args are an argv array: there is never a shell.
type Cmd struct {
	Name string
	Args []string
	// Env nil inherits the agent's environment; non-nil is exactly the
	// child's environment.
	Env   []string
	Dir   string
	Stdin io.Reader
	// Stdout nil captures stdout into Result.Stdout, at most StdoutLimit
	// bytes; otherwise it is streamed here and Result.Stdout is empty.
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
// to stop (SIGINT) and given that long to do so. Captured stdout past
// StdoutLimit stops the command the same way and fails it with
// ErrStdoutTooLarge.
func (o OSExec) Run(ctx context.Context, c Cmd) (Result, error) {
	return o.run(ctx, c, StdoutLimit)
}

func (o OSExec) run(ctx context.Context, c Cmd, stdoutLimit int) (Result, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = o.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// runCtx is also cancelled when the captured stdout overflows.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	cmd := exec.CommandContext(runCtx, c.Name, c.Args...)
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

	stdout := &capBuffer{limit: stdoutLimit, over: stop}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = stdout
	}
	stderr := NewTailBuffer(stderrLimit)
	cmd.Stderr = stderr

	err := cmd.Start()
	if err == nil {
		waitExit(cmd.Process.Pid)
		mu.Lock()
		exited = true
		if grace != nil {
			grace.Stop()
		}
		if interrupted || runCtx.Err() != nil {
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
	if stdout.overflowed() {
		return res, fmt.Errorf("%s: %w (more than %d bytes)", c.Name, ErrStdoutTooLarge, stdoutLimit)
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

// capBuffer keeps the first limit bytes written to it. The first write past
// them calls over, which stops the command; later writes are dropped
// without an error, so the pipe drains while the command is stopped.
type capBuffer struct {
	mu    sync.Mutex
	limit int
	over  func()
	buf   []byte
	full  bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return len(p), nil
	}
	if len(b.buf)+len(p) > b.limit {
		b.buf = append(b.buf, p[:b.limit-len(b.buf)]...)
		b.full = true
		b.over()
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *capBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf
}

func (b *capBuffer) overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.full
}

// TailBuffer keeps the last limit bytes written to it: stderr, where tools
// put the line that explains a failure, or output a caller shows only the
// end of.
type TailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

// NewTailBuffer returns a TailBuffer that keeps the last limit bytes.
func NewTailBuffer(limit int) *TailBuffer { return &TailBuffer{limit: limit} }

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *TailBuffer) Bytes() []byte {
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
	} else if len(r.Stdout) > StdoutLimit {
		res.Stdout = []byte(r.Stdout[:StdoutLimit])
		return res, fmt.Errorf("%s: %w (more than %d bytes)", c.Name, ErrStdoutTooLarge, StdoutLimit)
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
