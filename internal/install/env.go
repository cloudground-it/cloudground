// Package install is `cgctl install` and `cgctl doctor`: the server
// installer as a list of idempotent steps, each with a check and an apply.
// The behaviour is specified in SPEC.md next to this file.
package install

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Commands runs the system tools the steps need (nginx -t, sshd -t, sysctl,
// useradd, ufw, journalctl…). The output is stdout and stderr together, as
// some tools write what is asked of them to stderr (`nginx -V`).
type Commands interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	// Quiet is Run for a command whose output is a secret: it is returned,
	// never logged (stdout only).
	Quiet(ctx context.Context, name string, args ...string) (string, error)
	// LookPath reports whether a command is installed.
	LookPath(name string) bool
}

// Packages is the distribution's package manager.
type Packages interface {
	// Missing returns the packages of pkgs that are not installed.
	Missing(ctx context.Context, pkgs []string) ([]string, error)
	// Update refreshes the package lists.
	Update(ctx context.Context) error
	// Install installs pkgs (after an Update).
	Install(ctx context.Context, pkgs []string) error
	// AddPPA adds a Launchpad PPA ("ondrej/php").
	AddPPA(ctx context.Context, ppa string) error
}

// Systemd is the service manager.
type Systemd interface {
	// Running reports whether systemd is PID 1 (false in a container).
	Running() bool
	Exists(ctx context.Context, unit string) (bool, error)
	Enabled(ctx context.Context, unit string) (bool, error)
	Active(ctx context.Context, unit string) (bool, error)
	// Do runs `systemctl <args>`.
	Do(ctx context.Context, args ...string) error
}

// Accounts looks up and creates users and groups.
type Accounts interface {
	User(name string) (uid, gid int, ok bool)
	Group(name string) (gid int, ok bool)
	AddSystemUser(ctx context.Context, name string) error
	AddSystemGroup(ctx context.Context, name string) error
}

// Files owns what the filesystem cannot say portably: file ownership, which
// a test cannot set without root.
type Files interface {
	Owner(path string) (uid, gid int, err error)
	// Chown sets the owner; -1 leaves that id as it is.
	Chown(path string, uid, gid int) error
	// Rename moves a file over another (the last step of every write).
	Rename(from, to string) error
}

// Fetcher downloads over HTTPS.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Env is the machine the steps work on.
type Env struct {
	// Root prefixes every path the steps read or write ("/" on a server,
	// a temporary directory in tests).
	Root string
	Cmd  Commands
	Pkg  Packages
	Sd   Systemd
	Acct Accounts
	FS   Files
	Net  Fetcher
	// Log receives what the installer did, for the install log.
	Log io.Writer
	// Sleep waits (a no-op in tests).
	Sleep func(time.Duration)
	// EUID, Arch ("x86_64"), FreeMB(path) and Listening(port) are the
	// preflight's view of the machine.
	EUID      int
	Arch      string
	FreeMB    func(path string) (int64, error)
	Listening func(port int) bool
	// PanelHealth asks the API's /healthz over its socket, which answers
	// 200 only when the API runs and reaches the agent: nil then (SPEC §3
	// step 23). Nil leaves that question out.
	PanelHealth func(ctx context.Context) error
}

// path maps an absolute system path into Root.
func (e *Env) path(p string) string {
	if e.Root == "" || e.Root == "/" {
		return p
	}
	return filepath.Join(e.Root, p)
}

// Path is path, for the commands that work on an Env (cgctl uninstall).
func (e *Env) Path(p string) string { return e.path(p) }

// Exists reports whether p (under Root) exists, without following a link.
func (e *Env) Exists(p string) bool { return e.exists(p) }

func (e *Env) logf(format string, args ...any) {
	if e.Log != nil {
		_, _ = io.WriteString(e.Log, fmt.Sprintf(format+"\n", args...))
	}
}

func (e *Env) sleep(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
	}
}

func (e *Env) readFile(p string) ([]byte, error) { return os.ReadFile(e.path(p)) }

func (e *Env) exists(p string) bool {
	_, err := os.Lstat(e.path(p))
	return err == nil
}

func (e *Env) stat(p string) (fs.FileInfo, error) { return os.Lstat(e.path(p)) }
