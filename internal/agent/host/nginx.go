package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrConfigInvalid matches every *ConfigError.
var ErrConfigInvalid = errors.New("nginx configuration is invalid")

// ErrReloadTimeout is a reload after which no new worker generation came up:
// the master kept the old configuration.
var ErrReloadTimeout = errors.New("nginx did not start a new worker generation")

// ConfigError is a failed `nginx -t`, with nginx's own verdict.
type ConfigError struct{ Output string }

func (e *ConfigError) Error() string {
	return "nginx config test failed: " + strings.TrimSpace(e.Output)
}

// Is makes errors.Is(err, ErrConfigInvalid) true.
func (e *ConfigError) Is(target error) bool { return target == ErrConfigInvalid }

// Nginx tests and reloads the web server.
type Nginx interface {
	Test(ctx context.Context) error
	// Reload applies the configuration and returns once a new worker
	// generation is running and no old worker still accepts connections.
	Reload(ctx context.Context) error
	// BuildInfo is `nginx -V`, which nginx writes to stderr.
	BuildInfo(ctx context.Context) (string, error)
}

// NginxCtl is the real Nginx.
type NginxCtl struct {
	Exec    Exec
	Systemd Systemd
	Unit    string        // nginx.service
	PIDFile string        // /run/nginx.pid
	Proc    string        // /proc
	Timeout time.Duration // new-generation wait; 0 means 10 s
	Poll    time.Duration // 0 means 10 ms
}

// NewNginx returns the real adapter with the Ubuntu layout.
func NewNginx(e Exec, sd Systemd) *NginxCtl {
	return &NginxCtl{Exec: e, Systemd: sd, Unit: "nginx.service", PIDFile: "/run/nginx.pid", Proc: "/proc"}
}

func (n *NginxCtl) Test(ctx context.Context) error {
	res, err := n.Exec.Run(ctx, Cmd{Name: "nginx", Args: []string{"-t"}})
	if err == nil {
		return nil
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return &ConfigError{Output: string(res.Stderr)}
	}
	return err
}

func (n *NginxCtl) BuildInfo(ctx context.Context) (string, error) {
	res, err := n.Exec.Run(ctx, Cmd{Name: "nginx", Args: []string{"-V"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stderr) + string(res.Stdout)), nil
}

func (n *NginxCtl) Reload(ctx context.Context) error {
	master, err := n.masterPID()
	if err != nil {
		return fmt.Errorf("nginx reload: %w", err)
	}
	before, err := n.workers(master)
	if err != nil {
		return fmt.Errorf("nginx reload: %w", err)
	}
	if err := n.Systemd.Reload(ctx, n.Unit); err != nil {
		return err
	}
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	poll := n.Poll
	if poll <= 0 {
		poll = 10 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		// The master can change only on a binary upgrade; re-read it.
		if m, err := n.masterPID(); err == nil {
			master = m
		}
		if now, err := n.workers(master); err == nil && newGeneration(before, now) {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("nginx reload: %w", ErrReloadTimeout)
			}
			return fmt.Errorf("nginx reload: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// newGeneration is true once a worker that was not there before is serving
// and none of the old ones still is. The master starts the new workers
// before it tells the old ones to stop, and until then an old worker accepts
// connections with the old configuration: a reload reported done in that
// window had the next request served the configuration it replaced.
func newGeneration(before, now map[int]bool) bool {
	fresh := false
	for pid := range now {
		if before[pid] {
			return false
		}
		fresh = true
	}
	return fresh
}

func (n *NginxCtl) masterPID() (int, error) {
	b, err := os.ReadFile(n.PIDFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("bad pid file %s", n.PIDFile)
	}
	return pid, nil
}

// workers returns the PIDs of master's children that are serving workers
// (title "nginx: worker process", not "… is shutting down").
func (n *NginxCtl) workers(master int) (map[int]bool, error) {
	entries, err := os.ReadDir(n.Proc)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, ok := procPPID(filepath.Join(n.Proc, e.Name(), "stat")); !ok || ppid != master {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(n.Proc, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		title := strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))
		if title == "nginx: worker process" {
			out[pid] = true
		}
	}
	return out, nil
}

// procPPID reads the parent pid from /proc/<pid>/stat. The command name is
// in parentheses and may itself contain spaces or ')', so the fields are
// counted from the last ')'.
func procPPID(statPath string) (int, bool) {
	b, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

// FakeNginx is an in-memory Nginx.
type FakeNginx struct {
	mu sync.Mutex
	// TestErr is returned by Test (nil: the configuration is valid).
	TestErr error
	// ReloadErr is returned by Reload.
	ReloadErr error
	Build     string
	tests     int
	reloads   int
}

func (f *FakeNginx) Test(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tests++
	return f.TestErr
}

func (f *FakeNginx) Reload(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	return f.ReloadErr
}

func (f *FakeNginx) BuildInfo(ctx context.Context) (string, error) { return f.Build, nil }

// Counts returns how many tests and reloads ran.
func (f *FakeNginx) Counts() (tests, reloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tests, f.reloads
}

var _ Nginx = (*NginxCtl)(nil)
var _ Nginx = (*FakeNginx)(nil)
