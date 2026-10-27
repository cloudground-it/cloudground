// Package runtimes installs Node.js and Python for managed app sites and
// runs a release's dependency step (#28, SPEC.md).
package runtimes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	rt "github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// InstallName is the panel-agent subcommand that installs a Node.js major
// in its transient unit (SPEC §2).
const InstallName = "runtime-install"

// Limits (SPEC §2, §3).
const (
	installLimit = 15 * time.Minute
	prepareLimit = 20 * time.Minute
	outputLimit  = 4 << 10
)

// Ops holds what the handlers use.
type Ops struct {
	Exec host.Exec
	// Dir is where Node.js majors live; "" is runtimes.Dir.
	Dir string
	// UsrBin and UsrLib are where the distro's interpreters and their
	// modules are; "" is /usr/bin and /usr/lib (tests move them).
	UsrBin, UsrLib string
	// AgentBinary runs the install subcommand; "" is /usr/local/bin/panel-agent.
	AgentBinary string
}

// Register wires the methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.Runtimes, func(ctx context.Context, _ agentrpc.None) (rpc.RuntimesResult, error) {
		return o.List(), nil
	})
	agentrpc.Handle(s, agentrpc.RuntimeInstall, o.Install)
}

func (o *Ops) dir() string    { return or(o.Dir, rt.Dir) }
func (o *Ops) usrBin() string { return or(o.UsrBin, "/usr/bin") }
func (o *Ops) usrLib() string { return or(o.UsrLib, "/usr/lib") }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var pythonBinRe = regexp.MustCompile(`^python(3\.[0-9]{1,2})$`)

// List is what the server has (SPEC §1).
func (o *Ops) List() rpc.RuntimesResult {
	res := rpc.RuntimesResult{Node: []rpc.RuntimeVersion{}, Python: []rpc.RuntimeVersion{}}
	for _, major := range rt.NodeMajors {
		d := filepath.Join(o.dir(), rt.Node, major)
		v, err := os.ReadFile(filepath.Join(d, "VERSION"))
		if fi, ferr := os.Lstat(filepath.Join(d, "bin", "node")); err == nil && ferr == nil && fi.Mode().IsRegular() {
			res.Node = append(res.Node, rpc.RuntimeVersion{Version: major, Full: strings.TrimSpace(string(v))})
		}
	}
	for _, v := range o.distroPythons() {
		lib := filepath.Join(o.usrLib(), "python"+v)
		if exists(filepath.Join(lib, "venv", "__init__.py")) && exists(filepath.Join(lib, "ensurepip", "__init__.py")) {
			res.Python = append(res.Python, rpc.RuntimeVersion{Version: v, Full: "Python " + v})
		}
	}
	return res
}

// distroPythons are the 3.N with a /usr/bin/python3.N, newest first.
func (o *Ops) distroPythons() []string {
	entries, _ := os.ReadDir(o.usrBin())
	var out []string
	for _, e := range entries {
		if m := pythonBinRe.FindStringSubmatch(e.Name()); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Slice(out, func(i, j int) bool { return minor(out[i]) > minor(out[j]) })
	return out
}

func minor(v string) int {
	n := 0
	fmt.Sscanf(strings.TrimPrefix(v, "3."), "%d", &n)
	return n
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// installProps are the install unit's lines (SPEC §2): it may write the
// runtimes' StateDirectory and nothing else of the system.
var installProps = []string{
	"ProtectSystem=strict",
	"StateDirectory=cloudground-runtimes",
	"StateDirectoryMode=0755",
	"ProtectHome=yes",
	"PrivateTmp=yes",
	"PrivateDevices=yes",
	"NoNewPrivileges=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"RestrictNamespaces=yes",
	"LockPersonality=yes",
	"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
	"CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER",
	"MemoryMax=512M",
}

var runs atomic.Uint64

var commandEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}

// InstallService is the Node.js install's transient unit (tests read it).
func (o *Ops) InstallService(major string) host.Service {
	agent := or(o.AgentBinary, "/usr/local/bin/panel-agent")
	return host.Service{
		Unit:        fmt.Sprintf("cg-runtime-%d-%d.service", time.Now().UnixNano(), runs.Add(1)),
		Description: "CloudGround: install Node.js " + major,
		User:        "root", Dir: "/", Env: commandEnv, Props: installProps, MaxRuntime: installLimit, Pipe: true,
		Argv: []string{agent, InstallName, rt.Node, major},
	}
}

// Install is RuntimeInstall (SPEC §2).
func (o *Ops) Install(ctx context.Context, p rpc.RuntimeInstallParams) (rpc.RuntimesResult, error) {
	if err := p.Validate(); err != nil {
		return rpc.RuntimesResult{}, agentrpc.Invalid("%s", err.Error())
	}
	switch p.Runtime {
	case rt.Node:
		svc := o.InstallService(p.Version)
		res, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: host.ServiceArgs(svc), Env: commandEnv, Timeout: installLimit + time.Minute})
		if err != nil {
			msg := strings.TrimSpace(string(res.Stderr))
			if msg == "" {
				msg = err.Error()
			}
			return rpc.RuntimesResult{}, fmt.Errorf("install Node.js %s: %s", p.Version, msg)
		}
	case rt.Python:
		if !slices.Contains(o.distroPythons(), p.Version) {
			return rpc.RuntimesResult{}, agentrpc.Invalid("Python %s is not this system's: it has %s", p.Version, strings.Join(o.distroPythons(), ", "))
		}
		pkg := "python" + p.Version + "-venv"
		args := host.AptUnitArgs("CloudGround: install "+pkg, 720*time.Second,
			"-o", "DPkg::Lock::Timeout=120", "install", "-y", "-qq", "--no-install-recommends", pkg)
		if _, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: args, Env: commandEnv, Timeout: 13 * time.Minute}); err != nil {
			return rpc.RuntimesResult{}, fmt.Errorf("apt-get install %s: %w", pkg, err)
		}
	}
	return o.List(), nil
}

// PrepareService is the dependency step's transient service (SPEC §3).
func PrepareService(s state.Site, dir string) host.Service {
	return host.Service{
		Unit:        fmt.Sprintf("cg-app-prep-%s-%d-%d.service", s.SystemUser, time.Now().UnixNano(), runs.Add(1)),
		Description: "CloudGround: install the app's dependencies for " + s.Domain,
		User:        s.SystemUser, Slice: site.Slice(s.SystemUser), Dir: dir,
		Env: rt.PrepareEnv(s), Props: phpfpm.RunSandbox(s), MaxRuntime: prepareLimit, Pipe: true,
		Argv: []string{"/bin/sh", "-c", "exec 2>&1\n" + rt.PrepareScript(s)},
	}
}

// Prepare installs a release's dependencies as the site user (SPEC §3).
func (o *Ops) Prepare(ctx context.Context, s state.Site, dir string) (string, error) {
	if !s.Type.IsApp() {
		return "", nil
	}
	if !site.ValidSystemUser(s.SystemUser) {
		return "", agentrpc.Invalid("invalid system user %q", s.SystemUser)
	}
	have := o.List()
	if !have.Has(rt.Of(s.Type), s.Config.App.Version) {
		return "", agentrpc.Invalid("%s %s is not installed on this server", rt.Label(rt.Of(s.Type)), s.Config.App.Version)
	}
	svc := PrepareService(s, dir)
	out := host.NewTailBuffer(outputLimit)
	res, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: host.ServiceArgs(svc), Env: commandEnv, Stdout: out, Timeout: prepareLimit + time.Minute})
	if err != nil && ctx.Err() != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = o.Exec.Run(sctx, host.Cmd{Name: "systemctl", Args: []string{"stop", svc.Unit}, Env: commandEnv})
		cancel()
		return "", ctx.Err()
	}
	text := strings.TrimSpace(string(out.Bytes()))
	var ee *host.ExitError
	switch {
	case err == nil:
		return text, nil
	case errors.As(err, &ee):
		if text == "" {
			text = strings.TrimSpace(string(res.Stderr))
		}
		return text, agentrpc.Invalid("installing the app's dependencies failed: %s", text)
	}
	return text, fmt.Errorf("run the dependency step: %w", err)
}
