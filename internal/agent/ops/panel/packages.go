package panel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/syspkg"
)

// EnsurePackagesName is `panel-agent ensure-packages`, the agent unit's
// ExecStartPre (SPEC §2.6).
const EnsurePackagesName = "ensure-packages"

// EnsureBudget bounds ensure-packages: the update guard waits at most 2
// minutes for the agent's restart job (host.DefaultJobTimeout, in this
// release and the ones before it), and the agent unit's TimeoutStartSec
// is above both.
const EnsureBudget = 100 * time.Second

// Packages installs the system packages this release needs and the server
// lacks (SPEC §2.6).
type Packages struct {
	Exec host.Exec
	// Want is the list, syspkg.Release() in production.
	Want []string
	Log  *slog.Logger
}

// Missing is the packages of Want that dpkg does not have installed
// ("install ok installed", or "hold ok installed": the operator pinned it).
func (p *Packages) Missing(ctx context.Context) ([]string, error) {
	res, err := p.Exec.Run(ctx, host.Cmd{Name: "dpkg-query", Args: append([]string{"-W", "-f=${Package}\t${Status}\n"}, p.Want...),
		Env: host.AptEnv[:2], Timeout: 30 * time.Second})
	// dpkg-query exits 1 when a name is not in its database at all, and
	// still prints the others.
	var ee *host.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.Code == 1) {
		return nil, fmt.Errorf("dpkg-query: %w", err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		name, status, ok := strings.Cut(line, "\t")
		if ok && (status == "install ok installed" || status == "hold ok installed") {
			// A multi-arch package may print as name:arch.
			name, _, _ = strings.Cut(name, ":")
			have[name] = true
		}
	}
	var missing []string
	for _, w := range p.Want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing, nil
}

// Ensure installs the missing packages, as a transient root service
// (host.AptUnitArgs), and returns them. apt's package lists may be older
// than the archive (a server up for months): a failed install is tried once
// more after apt-get update.
func (p *Packages) Ensure(ctx context.Context) ([]string, error) {
	missing, err := p.Missing(ctx)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	p.log().Info("installing the system packages this release needs", "packages", missing)
	install := func() error {
		args := append([]string{"-o", "DPkg::Lock::Timeout=60", "install", "-y", "-qq"}, missing...)
		return p.apt(ctx, "install "+strings.Join(missing, " "), args...)
	}
	if err = install(); err != nil && ctx.Err() == nil {
		p.log().Warn("apt-get install failed; updating the package lists and trying again", "err", err)
		if err = p.apt(ctx, "update the package lists", "-o", "DPkg::Lock::Timeout=60", "update", "-qq"); err == nil {
			err = install()
		}
	}
	if err != nil {
		return missing, fmt.Errorf("install %s: %w", strings.Join(missing, " "), err)
	}
	if syspkg.NeedsReexec(missing) {
		p.reexec(ctx)
	}
	return missing, nil
}

// reexec runs `systemctl daemon-reexec`, so that PID 1 loads the libbpf
// just installed and enforces the sandbox's SocketBindDeny (phpfpm SPEC
// §4.1). Running units and jobs survive it. The update guard's restart of
// the agent, which waits on this very start, loses its connection and
// enqueues the restart again (host.DBusSystemd retries an idempotent job
// once); that start finds nothing missing and does not re-execute. A
// failure is logged, never fatal: the sandbox is then enforced from the
// next boot.
func (p *Packages) reexec(ctx context.Context) {
	if _, err := p.Exec.Run(ctx, host.Cmd{Name: "systemctl", Args: []string{"daemon-reexec"}, Env: host.AptEnv[:2], Timeout: 2 * time.Minute}); err != nil {
		p.log().Warn("systemctl daemon-reexec after installing "+syspkg.LibBPF+" failed: the sites' port restriction takes effect at the next boot", "err", err)
		return
	}
	p.log().Info("systemd re-executed to load " + syspkg.LibBPF + ": the sites' port restriction is enforced")
}

func (p *Packages) apt(ctx context.Context, what string, args ...string) error {
	_, err := p.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: host.AptUnitArgs("CloudGround: "+what, 10*time.Minute, args...),
		Env: host.AptEnv[:2], Timeout: EnsureBudget})
	return err
}

func (p *Packages) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// EnsureMain is `panel-agent ensure-packages`, run by systemd before the
// agent starts; it also brings in the release's kernel settings (Kernel). A failure fails the start only while an update is being
// applied (the guard's unit is running): the guard then rolls the update
// back. At any other start the agent starts anyway, without the package,
// and says so in its journal.
func EnsureMain(args []string, log *slog.Logger) int {
	if len(args) != 0 {
		log.Error("usage: panel-agent " + EnsurePackagesName)
		return 2
	}
	k := &Kernel{Exec: host.OSExec{}, Path: managed.Sysctl, Log: log}
	if want, err := KernelSettings(); err != nil {
		log.Warn("the kernel settings of this release could not be rendered", "err", err)
		k = nil
	} else {
		k.Want = want
	}
	return ensureMain(&Packages{Exec: host.OSExec{}, Want: syspkg.Release(), Log: log}, k, host.NewSystemd(), log)
}

func ensureMain(p *Packages, k *Kernel, sd host.Systemd, log *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), EnsureBudget)
	defer cancel()
	// The kernel settings first, a second's work, so a long apt run cannot
	// leave them written and not loaded; a failure is logged and never
	// fails the start (SPEC §2.7).
	if k != nil {
		if _, kerr := k.Ensure(ctx); kerr != nil {
			log.Warn("the kernel settings this release needs could not be written", "file", k.Path, "err", kerr)
		}
	}
	installed, err := p.Ensure(ctx)
	if err == nil {
		if len(installed) > 0 {
			log.Info("installed the system packages this release needs", "packages", installed)
		}
		return 0
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if g, gerr := sd.UnitProps(sctx, GuardUnit); gerr == nil && (g.ActiveState == "active" || g.ActiveState == "activating") {
		log.Error("the system packages this release needs could not be installed: the update is rolled back", "err", err)
		return 1
	}
	attrs := []any{"err", err}
	if len(installed) > 0 {
		attrs = append(attrs, "fix", "apt-get install "+strings.Join(installed, " "))
	}
	log.Warn("the system packages this release needs could not be installed; starting without them", attrs...)
	return 0
}
