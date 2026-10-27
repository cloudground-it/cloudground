package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// HelperSlice is the slice the heavy helpers run in (cg.slice/cg-helper.slice,
// beside the sites' cg-site.slice): out of cloudground-agent.service's cgroup,
// so the agent's own memory limit never cuts a large backup or restore
// (SPEC: helpers).
const HelperSlice = "cg-helper.slice"

// HelperExecName is the panel-agent subcommand a helper unit starts: it reads
// the tool's environment from stdin and executes the tool, leaving the rest
// of stdin to it.
const HelperExecName = "helper-exec"

// HelperNames are the commands HelperExec moves into their own units: the
// ones whose memory grows with the data they move (a repository's index, a
// database, certbot's Python).
var HelperNames = map[string]bool{"restic": true, "mariadb-dump": true, "mariadb": true, "certbot": true}

// HelperMemoryMax is each helper unit's own memory limit, provisional (bench/
// agent-memory.md): three quarters of the machine, so a runaway helper is
// killed in its own unit before the kernel picks a site's PHP or MariaDB,
// while any backup that fits in memory at all still runs. Swap is not
// limited.
const HelperMemoryMax = "75%"

// HelperProps are the unit lines of every helper: the sandbox of
// cloudground-agent.service, line for line, which the helpers inherited
// while they ran in its cgroup (a test holds the two together), and their
// own memory limit. The agent's environment travels on stdin (helper-exec),
// never as a unit property.
var HelperProps = []string{
	"UMask=0027",
	"ProtectSystem=strict",
	"ReadWritePaths=/etc /srv /var/lib/cloudground-agent /var/cache/cloudground /var/log /var/lib/letsencrypt /var/www/cloudground-acme /var/spool/cron /run /usr/local/bin",
	"ReadWritePaths=-/var/backups -/mnt",
	"ProtectHome=read-only",
	"PrivateTmp=yes",
	"NoNewPrivileges=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"ProtectClock=yes",
	"ProtectHostname=yes",
	"RestrictNamespaces=yes",
	"RestrictRealtime=yes",
	"LockPersonality=yes",
	"SystemCallArchitectures=native",
	"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
	"CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_FSETID CAP_KILL CAP_SETUID CAP_SETGID CAP_SETPCAP CAP_NET_ADMIN CAP_NET_RAW CAP_AUDIT_WRITE CAP_SYS_RESOURCE",
	"MemoryMax=" + HelperMemoryMax,
}

// helperStopWait is how long a stopped helper has, beyond its Interrupt, to
// end before systemd-run is killed. A variable for the tests.
var helperStopWait = 30 * time.Second

// HelperExec is an Exec that runs HelperNames each in a transient service
// of HelperSlice (`systemd-run --wait --pipe`) and every other command
// through Exec unchanged. A caller sees what OSExec would give it: the same
// stdout (captured within StdoutLimit, or streamed), stderr, exit status,
// timeout, cancellation and Interrupt (SPEC: helpers).
type HelperExec struct {
	// Exec runs systemd-run and systemctl (and the other commands);
	// OSExec on a real machine.
	Exec Exec
	// Timeout applies when the Cmd sets none, as OSExec's; zero means
	// DefaultTimeout.
	Timeout time.Duration
	// AgentBinary is the panel-agent that runs helper-exec; "" is
	// /usr/local/bin/panel-agent.
	AgentBinary string
	// LookPath resolves the tool in the agent's PATH, as os/exec would;
	// nil is exec.LookPath. Tests replace it.
	LookPath func(string) (string, error)
}

var helperRuns atomic.Uint64

// helperEnvLimit bounds the environment helper-exec reads from stdin.
const helperEnvLimit = 1 << 20

// HelperService is the transient service a helper command c runs in, its
// tool resolved to path.
func (h HelperExec) HelperService(c Cmd, path string, timeout time.Duration) Service {
	agent := h.AgentBinary
	if agent == "" {
		agent = "/usr/local/bin/panel-agent"
	}
	props := append([]string{}, HelperProps...)
	if c.Interrupt > 0 {
		// SIGINT to the tool alone, its group killed when it has exited or
		// the grace is over: what OSExec's Interrupt does.
		props = append(props, "KillMode=mixed", "KillSignal=SIGINT",
			"TimeoutStopSec="+strconv.FormatInt(int64((c.Interrupt+time.Second-1)/time.Second), 10))
	} else {
		props = append(props, "KillSignal=SIGKILL")
	}
	return Service{
		Unit:        fmt.Sprintf("cg-helper-%s-%d-%d.service", c.Name, time.Now().UnixNano(), helperRuns.Add(1)),
		Description: "CloudGround helper: " + c.Name,
		User:        "root",
		Slice:       HelperSlice,
		Dir:         c.Dir,
		Props:       props,
		// A backstop for an agent that died: its own timer ends the run
		// first, with the error OSExec gives.
		MaxRuntime: timeout + c.Interrupt + helperStopWait + time.Minute,
		Pipe:       true,
		Argv:       append([]string{agent, HelperExecName, "--", path}, c.Args...),
	}
}

// helperEnv is what helper-exec reads first on stdin: each KEY=value
// NUL-terminated, then an empty entry.
func helperEnv(env []string) ([]byte, error) {
	var b bytes.Buffer
	for _, kv := range env {
		if kv == "" || strings.IndexByte(kv, 0) >= 0 || !strings.Contains(kv, "=") {
			return nil, fmt.Errorf("helper environment entry %q: %w", kv, ErrInvalidArgument)
		}
		b.WriteString(kv)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	if b.Len() > helperEnvLimit {
		return nil, fmt.Errorf("helper environment of %d bytes: %w", b.Len(), ErrInvalidArgument)
	}
	return b.Bytes(), nil
}

// Run implements Exec.
func (h HelperExec) Run(ctx context.Context, c Cmd) (Result, error) {
	if !HelperNames[c.Name] {
		return h.Exec.Run(ctx, c)
	}
	look := h.LookPath
	if look == nil {
		look = exec.LookPath
	}
	path, err := look(c.Name)
	if err != nil {
		// What os/exec reports for a missing binary (exec.ErrNotFound).
		return Result{}, fmt.Errorf("%s: %w", c.Name, err)
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	head, err := helperEnv(env)
	if err != nil {
		return Result{}, err
	}
	var stdin io.Reader = bytes.NewReader(head)
	if c.Stdin != nil {
		stdin = io.MultiReader(stdin, c.Stdin)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = h.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	svc := h.HelperService(c, path, timeout)
	wait := c.Interrupt + helperStopWait

	// The run's deadline and cancellation are the agent's, as OSExec's
	// are; systemd-run itself runs on a context they do not end. The tool
	// is not systemd-run's child: killing systemd-run would leave it
	// running, and with --pipe it would write into closed pipes. So the
	// unit is stopped (SIGKILL, or SIGINT and the grace with Interrupt)
	// while systemd-run is still up, and systemd-run returns on its own
	// once the unit has gone; only past wait is it killed.
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runCtx, kill := context.WithCancel(context.WithoutCancel(ctx))
	defer kill()
	finished := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-finished:
		case <-tctx.Done():
			stopUnitUntil(tctx, h.Exec, svc.Unit, wait, finished, kill)
		}
	}()
	res, err := h.Exec.Run(runCtx, Cmd{Name: "systemd-run", Args: ServiceArgs(svc),
		Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"},
		Stdin: stdin, Stdout: c.Stdout, Timeout: timeout + wait + time.Minute})
	close(finished)
	<-stopped
	if ctxErr := tctx.Err(); err != nil && ctxErr != nil {
		return res, fmt.Errorf("%s: %w", c.Name, ctxErr)
	}
	if errors.Is(err, ErrStdoutTooLarge) {
		// systemd-run was killed for printing past the limit; the unit
		// would run on.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
		_, _ = h.Exec.Run(sctx, Cmd{Name: "systemctl", Args: []string{"stop", svc.Unit}})
		cancel()
		return res, fmt.Errorf("%s: %w (more than %d bytes)", c.Name, ErrStdoutTooLarge, StdoutLimit)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		// systemd-run exits with the tool's status (--wait); the error
		// names the tool, as OSExec's does.
		return res, &ExitError{Name: c.Name, Code: ee.Code, Stderr: ee.Stderr}
	}
	return res, err
}

// stopRetry spaces the stops of a unit systemd has not loaded yet (the run
// was cancelled before systemd-run had created it). A variable for the
// tests.
var stopRetry = time.Second

// stopUnitUntil stops a cancelled run's unit until its systemd-run has
// returned (finished). A stop that finds no unit, because systemd-run has
// not created it yet, is retried. Past wait systemd-run is killed, and the
// unit stopped once more in case it started only then.
func stopUnitUntil(ctx context.Context, ex Exec, unit string, wait time.Duration, finished <-chan struct{}, kill func()) {
	base := context.WithoutCancel(ctx)
	stop := func() {
		sctx, cancel := context.WithTimeout(base, wait)
		defer cancel()
		_, _ = ex.Run(sctx, Cmd{Name: "systemctl", Args: []string{"stop", unit}})
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		stop()
		retry := time.NewTimer(stopRetry)
		select {
		case <-finished:
			retry.Stop()
			return
		case <-deadline.C:
			retry.Stop()
			kill()
			stop()
			return
		case <-retry.C:
		}
	}
}

// readHelperEnv reads helper-exec's environment from r one byte at a time,
// so that nothing past its end is consumed: the rest of stdin is the tool's.
func readHelperEnv(r io.Reader) ([]string, error) {
	var (
		env []string
		cur []byte
		n   int
		one [1]byte
	)
	for {
		k, err := r.Read(one[:])
		if k == 0 {
			if err == nil {
				continue
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if n++; n > helperEnvLimit {
			return nil, errors.New("environment too large")
		}
		if one[0] != 0 {
			cur = append(cur, one[0])
			continue
		}
		if len(cur) == 0 {
			return env, nil
		}
		if !bytes.ContainsRune(cur, '=') {
			return nil, fmt.Errorf("environment entry %q", cur)
		}
		env = append(env, string(cur))
		cur = nil
	}
}

// HelperExecMain is `panel-agent helper-exec -- <absolute tool path> <args>`,
// the command of a helper unit: it reads the tool's environment from stdin
// (helperEnv) and replaces itself with the tool, whose stdin is what
// follows. Its own failures exit 126, as a shell's "cannot execute".
func HelperExecMain(args []string, stdin *os.File) int {
	if len(args) < 2 || args[0] != "--" || !filepath.IsAbs(args[1]) {
		fmt.Fprintln(os.Stderr, "usage: panel-agent helper-exec -- <absolute path> [arguments]")
		return 126
	}
	env, err := readHelperEnv(stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper-exec: cannot read the environment:", err)
		return 126
	}
	path := args[1]
	argv := append([]string{filepath.Base(path)}, args[2:]...)
	if err := syscall.Exec(path, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "helper-exec:", err)
	}
	return 126
}

var _ Exec = HelperExec{}
