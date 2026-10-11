package sftp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/siteshell"
	"github.com/cloudground-it/cloudground/internal/state"
)

// spawned is what a fake Spawn saw.
type spawned struct {
	argv  []string
	stdio [3]*os.File
}

// gateFixture serves the gate for the test's own uid, which the user
// database says is cg-site-1 with its shell login.
type gateFixture struct {
	*shellFixture
	mu     sync.Mutex
	runs   []spawned
	status int
	hold   func(unit string) // runs inside wait, before the status
}

func newGate(t *testing.T, status int, hold func(unit string)) *gateFixture {
	t.Helper()
	g := &gateFixture{shellFixture: newShellOps(t), status: status, hold: hold}
	uid := os.Getuid()
	g.passwd(t, fmt.Sprintf("%s:x:%d:%d::%s:/usr/sbin/nologin", user, uid, uid, g.s.RootPath),
		fmt.Sprintf("%s:x:%d:%d::%s:/usr/local/bin/cg-site-shell", alias, uid, uid, g.s.RootPath))
	g.o.Spawn = func(argv []string, stdio [3]*os.File) (func() int, error) {
		// What systemd-run would get: its own copies of the descriptors.
		var own [3]*os.File
		for i, f := range stdio {
			if f != nil {
				fd, err := unix.Dup(int(f.Fd()))
				if err != nil {
					return nil, err
				}
				own[i] = os.NewFile(uintptr(fd), f.Name())
			}
		}
		g.mu.Lock()
		g.runs = append(g.runs, spawned{argv: argv, stdio: own})
		g.mu.Unlock()
		unit := ""
		for _, a := range argv {
			if u, ok := strings.CutPrefix(a, "--unit="); ok {
				unit = u
			}
		}
		return func() int {
			if g.hold != nil {
				g.hold(unit)
			}
			return g.status
		}, nil
	}
	gctx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- g.o.ServeGate(gctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("ServeGate: %v", err)
		}
	})
	waitSocket(t, g.paths.Socket)
	return g
}

func (g *gateFixture) last(t *testing.T) spawned {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.runs) == 0 {
		t.Fatal("no shell was started")
	}
	return g.runs[len(g.runs)-1]
}

// prop reports whether argv sets the unit property line with -p.
func prop(argv []string, line string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-p" && argv[i+1] == line {
			return true
		}
	}
	return false
}

func pipes(t *testing.T) [3]*os.File {
	t.Helper()
	var out [3]*os.File
	for i := range out {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		if i == 0 {
			out[i] = r
		} else {
			out[i] = w
		}
	}
	return out
}

func sameFile(a, b *os.File) bool {
	var sa, sb unix.Stat_t
	return a != nil && b != nil && unix.Fstat(int(a.Fd()), &sa) == nil && unix.Fstat(int(b.Fd()), &sb) == nil &&
		sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

func TestGateRunsACommandAsASandboxedService(t *testing.T) {
	g := newGate(t, 3, nil)
	stdio := pipes(t)
	code, err := siteshell.Run(g.paths.Socket, siteshell.Request{HasCommand: true, Command: "git --version", Term: "xterm",
		Env: []string{"LANG=C.UTF-8"}}, stdio)
	if err != nil || code != 3 {
		t.Fatalf("Run = %d %v", code, err)
	}
	run := g.last(t)
	a := run.argv
	unit := fmt.Sprintf("%s-%d.service", alias, os.Getpid())
	for _, want := range []string{"systemd-run", "--wait", "--collect", "--unit=" + unit, "--slice=" + user + ".slice",
		"--uid=" + user, "--gid=" + user, "--working-directory=" + g.s.RootPath, "--pipe",
		"--setenv=HOME=" + g.s.RootPath, "--setenv=TERM=xterm", "--setenv=LANG=C.UTF-8", "--setenv=TMPDIR=/tmp"} {
		if !slices.Contains(a, want) {
			t.Errorf("argv lacks %q: %q", want, a)
		}
	}
	// The PHP master's sandbox, every line of it.
	for _, line := range phpfpm.RunSandbox(state.Site{Domain: g.s.Domain, RootPath: g.s.RootPath}) {
		if !prop(a, line) {
			t.Errorf("argv lacks -p %q", line)
		}
	}
	for _, line := range ShellHardening {
		if !prop(a, line) {
			t.Errorf("argv lacks the shell's hardening -p %q", line)
		}
	}
	// The login script that reads /proc/sys/kernel/random, which the
	// sandbox hides, is hidden too (it printed two errors per login).
	if !prop(a, "InaccessiblePaths=-/etc/profile.d/80-systemd-osc-context.sh") {
		t.Error("the shell still reads systemd's terminal-integration profile script")
	}
	if !prop(a, "IgnoreSIGPIPE=no") {
		t.Error("SIGPIPE stays ignored in the shell")
	}
	if !slices.Equal(a[len(a)-4:], []string{"--", "/bin/bash", "-c", "git --version"}) {
		t.Errorf("command %q", a[len(a)-4:])
	}
	// systemd-run gets the client's own descriptors.
	for i := range stdio {
		if !sameFile(run.stdio[i], stdio[i]) {
			t.Errorf("stdio %d is not the client's", i)
		}
	}
}

// openPTY opens a pseudo-terminal pair, as sshd does for a session.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

func TestGateGivesALoginItsTerminal(t *testing.T) {
	g := newGate(t, 3, nil)
	_, pts := openPTY(t)
	if err := os.Mkdir(filepath.Join(g.s.RootPath, "current"), 0o750); err != nil {
		t.Fatal(err)
	}
	code, err := siteshell.Run(g.paths.Socket, siteshell.Request{Term: "xterm-256color"}, [3]*os.File{pts, pts, pts})
	if err != nil || code != 3 {
		t.Fatalf("Run = %d %v", code, err)
	}
	run := g.last(t)
	a := run.argv
	// W3: a login on a real terminal (TERM not "dumb") is where systemd's
	// terminal-integration script runs and read /proc/sys/kernel/random,
	// which the sandbox hides; the script is out of the session's reach.
	if !slices.Contains(a, "--setenv=TERM=xterm-256color") {
		t.Errorf("the login lost the client's TERM: %q", a)
	}
	if !prop(a, "InaccessiblePaths=-/etc/profile.d/80-systemd-osc-context.sh") || !prop(a, "ProcSubset=pid") {
		t.Errorf("an interactive login reads systemd's terminal-integration script: %q", a)
	}
	// systemd opens the terminal itself and makes it the service's
	// controlling terminal (job control, ^C, window size).
	if !prop(a, "TTYPath="+pts.Name()) || !prop(a, "StandardInput=tty-force") || !prop(a, "StandardOutput=tty") || !prop(a, "StandardError=tty") {
		t.Errorf("not on the terminal %s: %q", pts.Name(), a)
	}
	if slices.Contains(a, "--pipe") || run.stdio != [3]*os.File{} {
		t.Errorf("a terminal session also passed its descriptors: %q", a)
	}
	cur := filepath.Join(g.s.RootPath, "current")
	if !slices.Contains(a, "--working-directory="+cur) || !slices.Contains(a, "--setenv=PWD="+cur) {
		t.Errorf("a login does not start in ~/current: %q", a)
	}
	if !slices.Equal(a[len(a)-3:], []string{"--", "/bin/bash", "-l"}) {
		t.Errorf("command %q", a[len(a)-3:])
	}
}

func TestTTYOfRefusesWhatIsNotTheCallersTerminal(t *testing.T) {
	_, pts := openPTY(t)
	uid := uint32(os.Getuid())
	if got := ttyOf(pts, uid); got != pts.Name() {
		t.Fatalf("ttyOf(pts) = %q, want %q", got, pts.Name())
	}
	if got := ttyOf(pts, uid+1); got != "" {
		t.Fatalf("someone else's terminal: %q", got)
	}
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	if got := ttyOf(null, uid); got != "" {
		t.Fatalf("/dev/null: %q", got)
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if got := ttyOf(r, uid); got != "" {
		t.Fatalf("a pipe: %q", got)
	}
}

// rawClient speaks the gate's protocol by hand.
func rawClient(t *testing.T, socket string, req siteshell.Request, stdio [3]*os.File) (*net.UnixConn, *bufio.Reader) {
	t.Helper()
	nc, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*net.UnixConn)
	t.Cleanup(func() { c.Close() })
	line, _ := json.Marshal(req)
	var rights []byte
	if stdio[0] != nil {
		rights = unix.UnixRights(int(stdio[0].Fd()), int(stdio[1].Fd()), int(stdio[2].Fd()))
	}
	if _, _, err := c.WriteMsgUnix(append(line, '\n'), rights, nil); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	return c, bufio.NewReader(c)
}

func TestGateStopsTheServiceWhenTheClientGoes(t *testing.T) {
	stopped := make(chan struct{})
	gp := make(chan *gateFixture, 1)
	g := newGate(t, 143, func(unit string) {
		g := <-gp
		gp <- g
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if g.sd.Did("stop", unit) {
				close(stopped)
				return
			}
		}
	})
	gp <- g
	c, r := rawClient(t, g.paths.Socket, siteshell.Request{HasCommand: true, Command: "sleep 100"}, pipes(t))
	if l, _ := r.ReadString('\n'); !strings.HasPrefix(l, "ok cg-shell-1-") {
		t.Fatalf("answer %q", l)
	}
	if _, err := c.Write([]byte("stop\n")); err != nil {
		t.Fatal(err)
	}
	if l, _ := r.ReadString('\n'); l != "exit 143\n" {
		t.Fatalf("end %q", l)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the service was not stopped")
	}
}

func TestGateRefuses(t *testing.T) {
	g := newGate(t, 3, nil)
	uid := os.Getuid()
	refused := func(what string, req siteshell.Request, stdio [3]*os.File, want string) {
		t.Helper()
		_, r := rawClient(t, g.paths.Socket, req, stdio)
		l, _ := r.ReadString('\n')
		if !strings.HasPrefix(l, "error ") || !strings.Contains(l, want) {
			t.Errorf("%s: answer %q, want an error with %q", what, l, want)
		}
	}
	refused("no descriptors", siteshell.Request{}, [3]*os.File{}, "stdin, stdout and stderr")
	refused("a NUL in the command", siteshell.Request{HasCommand: true, Command: "a\x00b"}, pipes(t), "bad command")
	refused("a bad terminal", siteshell.Request{Term: "x;rm"}, pipes(t), "bad terminal")
	refused("a variable not passed on", siteshell.Request{Env: []string{"LD_PRELOAD=/tmp/x.so"}}, pipes(t), "bad variable")

	g.passwd(t, fmt.Sprintf("%s:x:%d:%d::%s:/x", user, uid, uid, g.s.RootPath))
	refused("no shell login", siteshell.Request{}, pipes(t), "no site shell")
	g.passwd(t, fmt.Sprintf("%s:x:%d:%d::%s:/x", user, uid, uid, g.s.RootPath), fmt.Sprintf("cg-site-2:x:%d:%d::/srv/sites/b.example:/x", uid, uid),
		fmt.Sprintf("%s:x:%d:%d::%s:/x", alias, uid, uid, g.s.RootPath))
	refused("two site users on the uid", siteshell.Request{}, pipes(t), "no site shell")
	g.passwd(t, fmt.Sprintf("%s:x:%d:%d::/etc:/x", user, uid, uid), fmt.Sprintf("%s:x:%d:%d::/etc:/x", alias, uid, uid))
	refused("a home that is not a site", siteshell.Request{}, pipes(t), "not a site")
	g.mu.Lock()
	n := len(g.runs)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d refused requests started a shell", n)
	}

	// Nothing sent: the gate gives up after its request timeout.
	nc, err := net.Dial("unix", g.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_ = nc.SetReadDeadline(time.Now().Add(gateRequestTimeout + 3*time.Second))
	start := time.Now()
	l, _ := bufio.NewReader(nc).ReadString('\n')
	if !strings.HasPrefix(l, "error ") || time.Since(start) > gateRequestTimeout+2*time.Second {
		t.Fatalf("a silent client: %q after %v", l, time.Since(start))
	}
}

// A tool that opens one session per command (a deploy script, Ansible)
// starts tens of them in a minute, one after the other: all are admitted.
// At 20 a minute the 21st command of a deploy run was refused.
func TestGateRateAdmitsAToolRun(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := &gateLimits{now: func() time.Time { return now }}
	for i := 0; i < 60; i++ {
		r, err := g.session(7)
		if err != nil {
			t.Fatalf("command %d of a tool run: %v", i+1, err)
		}
		r()
		now = now.Add(500 * time.Millisecond)
	}
}

func TestGateLimits(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := &gateLimits{now: func() time.Time { return now }}
	var releases []func()
	for i := 0; i < gateMaxPerUID; i++ {
		r, err := g.session(7)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		releases = append(releases, r)
	}
	if _, err := g.session(7); err == nil {
		t.Fatal("one session past the per-site cap was admitted")
	}
	if r, err := g.session(8); err != nil {
		t.Fatalf("another site was refused: %v", err)
	} else {
		r()
	}
	for _, r := range releases {
		r()
	}
	// The rate: gateRate starts per window, open or not.
	for i := gateMaxPerUID; i < gateRate; i++ {
		r, err := g.session(7)
		if err != nil {
			t.Fatalf("start %d within the rate: %v", i, err)
		}
		r()
	}
	if _, err := g.session(7); err == nil {
		t.Fatal("a start past the rate was admitted")
	}
	now = now.Add(gateRateWindow)
	if r, err := g.session(7); err != nil {
		t.Fatalf("after the window: %v", err)
	} else {
		r()
	}
	// The connections.
	for i := 0; i < gateMaxConns; i++ {
		if !g.enter() {
			t.Fatalf("connection %d refused", i)
		}
	}
	if g.enter() {
		t.Fatal("a connection past the cap was admitted")
	}
	g.leave()
	if !g.enter() {
		t.Fatal("a freed connection slot was not reused")
	}
}
