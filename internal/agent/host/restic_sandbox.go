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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// TreeSandbox is the view of the filesystem a restic run that walks a
// tenant's tree gets (SPEC: restic, the files backup): only Tree, read-only,
// and what restic itself needs. Nothing else of the machine is mounted in
// its namespace, so no link a tenant swaps in during the walk can lead the
// root-run restic to a file of another site or of the system.
type TreeSandbox struct {
	Tree     string   // the site root, bound read-only at its own path
	Writable []string // restic's cache, a local repository: bound read-write
}

// resticSandboxRO are the system paths bound read-only into a sandboxed
// restic run: the binaries, name resolution and TLS roots, the account
// names restic records with each file. The "-" prefix tolerates a path a
// machine does not have.
var resticSandboxRO = []string{
	"/usr", "-/lib", "-/lib64", "-/bin", "-/sbin",
	"-/etc/resolv.conf", "-/etc/hosts", "-/etc/nsswitch.conf", "-/etc/ssl", "-/etc/ca-certificates",
	"-/etc/passwd", "-/etc/group", "-/etc/localtime", "-/etc/gai.conf",
}

// ResticSandboxProps are the unit lines of a sandboxed restic run, beyond
// the binds: an empty root, a private /dev and /tmp, the one capability
// reading a tenant's files needs (CAP_DAC_READ_SEARCH: a site's files may be
// 0600), no other process visible, and SIGINT on stop so restic removes
// its repository lock.
var ResticSandboxProps = []string{
	"TemporaryFileSystem=/",
	"PrivateDevices=yes",
	"PrivateTmp=yes",
	"PrivateIPC=yes",
	"ProtectProc=invisible",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"ProtectHostname=yes",
	"ProtectClock=yes",
	"NoNewPrivileges=yes",
	"RestrictSUIDSGID=yes",
	"LockPersonality=yes",
	"RestrictNamespaces=yes",
	"CapabilityBoundingSet=CAP_DAC_READ_SEARCH",
	"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
	"KillSignal=SIGINT",
	"TimeoutStopSec=" + fmt.Sprint(int(ResticGrace/time.Second)),
}

// ResticExecName is the panel-agent subcommand a sandboxed restic run
// starts: it reads restic's secrets from stdin and executes restic with
// them, so they are never a unit property (which any local user can read
// over D-Bus) nor an argument.
const ResticExecName = "restic-exec"

var resticRuns atomic.Uint64

// sandboxArgs is the systemd-run argv (after the binary) of a sandboxed
// restic run of args.
func (r *ResticCLI) sandboxArgs(sb TreeSandbox, unit string, args []string) ([]string, error) {
	if !filepath.IsAbs(sb.Tree) || filepath.Clean(sb.Tree) != sb.Tree || sb.Tree == "/" {
		return nil, fmt.Errorf("restic sandbox: tree %q: %w", sb.Tree, ErrInvalidArgument)
	}
	props := append([]string{}, ResticSandboxProps...)
	for _, p := range resticSandboxRO {
		props = append(props, "BindReadOnlyPaths="+p)
	}
	agent := r.AgentBinary
	if agent == "" {
		agent = "/usr/local/bin/panel-agent"
	}
	props = append(props, "BindReadOnlyPaths="+sb.Tree)
	for _, w := range sb.Writable {
		if !filepath.IsAbs(w) || filepath.Clean(w) != w || w == "/" || strings.ContainsAny(w, " \t\n\"'\\") {
			return nil, fmt.Errorf("restic sandbox: writable path %q: %w", w, ErrInvalidArgument)
		}
		props = append(props, "BindPaths="+w)
	}
	if strings.ContainsAny(sb.Tree, " \t\n\"'\\") {
		return nil, fmt.Errorf("restic sandbox: tree %q: %w", sb.Tree, ErrInvalidArgument)
	}
	svc := Service{
		Unit: unit, Description: "restic backup of " + sb.Tree,
		User: "root", Slice: HelperSlice, Dir: "/", Props: props, MaxRuntime: 24 * time.Hour, Pipe: true,
		Argv: append([]string{agent, ResticExecName, "--"}, args...),
	}
	return ServiceArgs(svc), nil
}

// RunSandboxed runs restic args (a files backup of sb.Tree) in a transient
// service whose mount namespace shows only sb.Tree and what restic needs
// (TreeSandbox). The secrets go to restic-exec on stdin. Without Transient
// (tests) restic runs directly. A cancelled run stops the unit, which
// sends restic SIGINT.
func (r *ResticCLI) RunSandboxed(ctx context.Context, repo Repo, sb TreeSandbox, args ...string) ([]byte, error) {
	if !r.Transient {
		return r.Run(ctx, repo, args...)
	}
	if repo.URL == "" || repo.Password == "" {
		return nil, errors.New("restic: repository and password are required")
	}
	unit := fmt.Sprintf("cg-restic-%d-%d.service", time.Now().UnixNano(), resticRuns.Add(1))
	argv, err := r.sandboxArgs(sb, unit, args)
	if err != nil {
		return nil, err
	}
	var secrets bytes.Buffer
	for _, kv := range r.secretEnv(repo) {
		secrets.WriteString(kv)
		secrets.WriteByte(0)
	}
	// systemd-run does not follow ctx. With --pipe, restic writes to
	// systemd-run's own stdout and stderr, so killing systemd-run first
	// closes restic's pipes (the agent's end goes once Wait gives up on
	// them), and restic dies of SIGPIPE on its next write (a --json backup
	// reports progress all the time, and SIGINT makes it print a line)
	// before it has removed its lock. A cancelled run stops the unit
	// instead, and systemd-run is killed only if restic outlives its stop.
	runCtx, kill := context.WithCancel(context.WithoutCancel(ctx))
	defer kill()
	finished := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-finished:
		case <-ctx.Done():
			r.stopSandboxed(ctx, unit, finished, kill)
		}
	}()
	out := NewResticOutput(args)
	_, err = r.Exec.Run(runCtx, Cmd{Name: "systemd-run", Args: argv, Stdin: &secrets, Stdout: out,
		Env:     []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"},
		Timeout: 24 * time.Hour})
	close(finished)
	<-stopped
	if cerr := ctx.Err(); cerr != nil {
		return out.Bytes(), fmt.Errorf("systemd-run: %w", cerr)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		err = &ResticError{ExitError: ee, Kind: resticCodes[ee.Code]}
	}
	return out.Bytes(), withOutputErr(err, out)
}

// sandboxStopWait is how long a stopped sandboxed run has to end on its own
// (restic's SIGINT grace, then systemd's SIGKILL) before systemd-run is
// killed. A variable for the tests.
var sandboxStopWait = ResticGrace + 30*time.Second

// stopSandboxed stops a cancelled sandboxed run's unit (SIGINT to restic,
// which removes its lock and exits) until systemd-run has returned
// (finished), as a helper's (stopUnitUntil, with sandboxStopWait).
func (r *ResticCLI) stopSandboxed(ctx context.Context, unit string, finished <-chan struct{}, kill func()) {
	stopUnitUntil(ctx, r.Exec, unit, sandboxStopWait, finished, kill)
}

// secretEnv is what restic-exec hands restic beyond its fixed environment:
// the repository, its password, the allowed credentials and the cache.
func (r *ResticCLI) secretEnv(repo Repo) []string {
	env := []string{"RESTIC_REPOSITORY=" + repo.URL, "RESTIC_PASSWORD=" + repo.Password}
	if c := os.Getenv("RESTIC_CACHE_DIR"); c != "" {
		env = append(env, "RESTIC_CACHE_DIR="+c)
	}
	for k, v := range repo.Env {
		if ResticEnvKeys[k] && v != "" && !strings.ContainsAny(v, "\x00\n") {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// maxSecrets bounds what restic-exec reads from stdin.
const maxSecrets = 64 << 10

// ResticExecMain is `panel-agent restic-exec -- <restic args>`: it reads
// NUL-terminated KEY=value entries from stdin (only the repository, the
// password, the cache directory and ResticEnvKeys), then replaces itself
// with restic, whose stdin is /dev/null.
func ResticExecMain(args []string, stdin io.Reader) int {
	if len(args) == 0 || args[0] != "--" {
		fmt.Fprintln(os.Stderr, "usage: panel-agent restic-exec -- <restic arguments>")
		return 2
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxSecrets+1))
	if err != nil || len(b) > maxSecrets {
		fmt.Fprintln(os.Stderr, "restic-exec: cannot read the secrets")
		return 2
	}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "TMPDIR=/tmp", "HOME=/tmp"}
	for _, kv := range strings.Split(string(b), "\x00") {
		if kv == "" {
			continue
		}
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !(k == "RESTIC_REPOSITORY" || k == "RESTIC_PASSWORD" || k == "RESTIC_CACHE_DIR" || ResticEnvKeys[k]) {
			fmt.Fprintf(os.Stderr, "restic-exec: refusing variable %q\n", k)
			return 2
		}
		env = append(env, kv)
	}
	os.Setenv("PATH", env[0][len("PATH="):])
	bin, err := exec.LookPath("restic")
	if err != nil {
		fmt.Fprintln(os.Stderr, "restic-exec:", err)
		return 2
	}
	if null, err := os.Open(os.DevNull); err == nil {
		_ = unix.Dup2(int(null.Fd()), 0)
	}
	if err := syscall.Exec(bin, append([]string{"restic"}, args[1:]...), env); err != nil {
		fmt.Fprintln(os.Stderr, "restic-exec:", err)
	}
	return 2
}
