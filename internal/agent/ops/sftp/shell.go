package sftp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agent/ops/system"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/siteshell"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The site shell's names (SPEC §5).
const (
	// ShellGroup is every shell login's one supplementary group; only it
	// may reach the gate socket.
	ShellGroup = "cg-shell"
	// GateName is the name panel-agent answers to as a login shell.
	GateName = "cg-site-shell"
	// AgentBinary is the binary the gate link points at, in BinDir.
	AgentBinary = "panel-agent"
	// SourceDir, in the site root, is the site user's folder for
	// checkouts that become releases.
	SourceDir = "src"
)

// shellDropIn is the sshd configuration of the shell logins (SPEC §5.3).
const shellDropIn = `# Managed by CloudGround: site shells (R-4.5). Do not edit.
# cg-shell-<id> is a site's shell login: the site user's uid, keys only, the
# keys of type "shell" only, no chroot; its login shell hands the session to
# the agent, which runs bash in a sandboxed service in the site's slice. The SFTP login (cg-site-*) is configured in
# 10-cloudground-sftp.conf and is not affected.
Match User cg-shell-*
    AuthorizedKeysFile .ssh/shell_keys
    ChrootDirectory none
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    PermitEmptyPasswords no
    PermitUserRC no
    DisableForwarding yes
    AllowTcpForwarding no
    X11Forwarding no
    PermitTTY yes
    ForceCommand ` + siteshell.Forced + `
`

// ShellSystemd is what the site shell needs of systemd.
type ShellSystemd interface {
	Stop(ctx context.Context, unit string) error
	Reload(ctx context.Context, unit string) error
	UnitProps(ctx context.Context, unit string) (host.UnitProps, error)
	ListUnits(ctx context.Context, patterns ...string) ([]host.UnitState, error)
	DaemonReload(ctx context.Context) error
}

// ShellPaths is where the site shell's files are.
type ShellPaths struct {
	DropIn     string // the sshd drop-in (§5.3)
	BinDir     string // panel-agent's directory, where the gate link goes
	SystemdDir string // where a user manager is masked
	Socket     string // the gate socket
	Passwd     string // the user database the gate reads
	SitesRoot  string // where site trees are (a site's home must be below it)
	CronDeny   string // cron's deny list (§5.7)
	AtDeny     string // at's deny list
	SSHDRunDir string // sshd's privilege separation directory
}

// DefaultShellPaths is the installer's layout.
func DefaultShellPaths() ShellPaths {
	return ShellPaths{
		DropIn:     "/etc/ssh/sshd_config.d/11-cloudground-shell.conf",
		BinDir:     "/usr/local/bin",
		SystemdDir: "/etc/systemd/system",
		Socket:     "/run/cloudground/shell.sock",
		Passwd:     "/etc/passwd",
		SitesRoot:  "/srv/sites",
		CronDeny:   "/etc/cron.deny",
		AtDeny:     "/etc/at.deny",
		SSHDRunDir: "/run/sshd",
	}
}

func (o *Ops) shellReady() error {
	if o.Systemd == nil || o.Exec == nil || o.ShellPaths.Socket == "" {
		return errors.New("the site shell is not available on this agent")
	}
	return nil
}

// SetShell turns a site's SSH shell on or off (SPEC §5.5). The server
// holds the site's lock.
func (o *Ops) SetShell(ctx context.Context, p rpc.ShellParams) (map[string]string, error) {
	if err := o.shellReady(); err != nil {
		return nil, err
	}
	alias := site.ShellUser(p.Site.SystemUser)
	if alias == "" {
		return nil, agentrpc.Invalid("invalid system user %q", p.Site.SystemUser)
	}
	if !p.Enable {
		if err := o.disableShell(ctx, p.Site); err != nil {
			return nil, err
		}
		return map[string]string{"shell": "disabled"}, nil
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return nil, err
	}
	own := r.Owner()
	if own.UID <= 0 {
		r.Close()
		return nil, fmt.Errorf("site user %s resolves to uid %d", p.Site.SystemUser, own.UID)
	}
	// The root is root's (the SFTP chroot): src/ is where a checkout made
	// in the shell lives before it becomes a release.
	err = r.MkdirAll(SourceDir, 0o750)
	r.Close()
	if err != nil {
		return nil, fmt.Errorf("the site's %s folder: %w", SourceDir, err)
	}
	if err := o.ensureShellHost(ctx); err != nil {
		return nil, err
	}
	err = o.Accounts.CreateUser(ctx, host.UserSpec{
		Name: alias, System: true, Home: p.Site.RootPath, Shell: o.gatePath(),
		PrimaryGroup: p.Site.SystemUser, Groups: []string{ShellGroup},
		UID: own.UID, NonUnique: true,
	})
	if errors.Is(err, host.ErrUserExists) {
		// A repeated enable: the login must be this site's.
		uid, ok, lerr := o.passwdUID(alias)
		switch {
		case lerr != nil:
			return nil, lerr
		case ok && uid != own.UID:
			return nil, agentrpc.Conflict("the user %s exists with another uid", alias)
		}
		err = o.Accounts.AddToGroup(ctx, alias, ShellGroup)
	}
	if err != nil {
		return nil, fmt.Errorf("create the shell login %s: %w", alias, err)
	}
	if err := o.SetAccess(ctx, alias, p.Site.Config.SFTPEnabled); err != nil {
		return nil, err
	}
	if err := o.maskUserManager(ctx, own.UID, true); err != nil {
		return nil, err
	}
	return map[string]string{"shell": "enabled", "user": alias}, nil
}

// RemoveShell is SetShell(disable) for a site being deleted: what is gone
// already is fine.
func (o *Ops) RemoveShell(ctx context.Context, s state.Site) error {
	if o.shellReady() != nil {
		return nil
	}
	return o.disableShell(ctx, s)
}

// disableShell ends the site's shells and removes its shell login and the
// user-manager mask. Every step runs; the errors are joined.
func (o *Ops) disableShell(ctx context.Context, s state.Site) error {
	alias := site.ShellUser(s.SystemUser)
	var errs []error
	errs = append(errs, o.stopShells(ctx, s.SystemUser))
	uid, found, err := o.passwdUID(alias)
	if err != nil {
		errs = append(errs, err)
	}
	if !found {
		uid, found, err = o.passwdUID(s.SystemUser)
		if err != nil {
			errs = append(errs, err)
		}
	}
	exists, err := o.Accounts.UserExists(ctx, alias)
	switch {
	case err != nil:
		errs = append(errs, err)
	case exists:
		if err := o.forceDelete(ctx, alias); err != nil && !errors.Is(err, host.ErrNoSuchUser) {
			errs = append(errs, fmt.Errorf("remove the shell login %s: %w", alias, err))
		}
	}
	if found && uid > 0 {
		errs = append(errs, o.maskUserManager(ctx, uid, false))
	}
	return errors.Join(errs...)
}

func (o *Ops) forceDelete(ctx context.Context, name string) error {
	if f, ok := o.Accounts.(interface {
		ForceDeleteUser(ctx context.Context, name string) error
	}); ok {
		return f.ForceDeleteUser(ctx, name)
	}
	return o.Accounts.DeleteUser(ctx, name, false)
}

// stopShells stops every open shell session of the site (its scopes).
func (o *Ops) stopShells(ctx context.Context, systemUser string) error {
	alias := site.ShellUser(systemUser)
	if alias == "" || o.Systemd == nil {
		return nil
	}
	units, err := o.Systemd.ListUnits(ctx, alias+"-*.service")
	if err != nil {
		return fmt.Errorf("list the open shells: %w", err)
	}
	var errs []error
	for _, u := range units {
		if err := o.Systemd.Stop(ctx, u.Name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (o *Ops) gatePath() string { return filepath.Join(o.ShellPaths.BinDir, GateName) }

// ensureShellHost puts in place what every site shell shares: the gate
// link, the shell group and the sshd drop-in (SPEC §5.5).
func (o *Ops) ensureShellHost(ctx context.Context) error {
	o.shellMu.Lock()
	defer o.shellMu.Unlock()
	if err := o.ensureGateLink(); err != nil {
		return err
	}
	// sshd -t needs its privilege separation directory, which a
	// socket-activated sshd makes only on its first connection.
	if d := o.ShellPaths.SSHDRunDir; d != "" {
		if err := rootfile.MkdirMode(d, 0o755); err != nil {
			return fmt.Errorf("%s: %w", d, err)
		}
	}
	if err := o.Accounts.EnsureGroup(ctx, ShellGroup, true); err != nil {
		return fmt.Errorf("the shell group: %w", err)
	}
	if _, err := system.ApplySSHDDropIn(ctx, o.Exec, o.Systemd, o.ShellPaths.DropIn, []byte(shellDropIn)); err != nil {
		return err
	}
	return nil
}

// ensureGateLink makes sure <BinDir>/cg-site-shell runs the gate client:
// the cg-site-shell binary the installer puts there (a regular file, kept
// as it is), or, on a server upgraded by a self-update that has not had it
// yet, a relative link to panel-agent, which answers to that name (SPEC
// §5.4). Anything else at the name is replaced by the link.
func (o *Ops) ensureGateLink() error {
	if fi, err := os.Lstat(o.gatePath()); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
		return nil
	}
	agent := filepath.Join(o.ShellPaths.BinDir, AgentBinary)
	if fi, err := os.Stat(agent); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("the gate needs %s: %v", agent, err)
	}
	link := o.gatePath()
	if t, err := os.Readlink(link); err == nil && t == AgentBinary {
		return nil
	}
	tmp := link + ".cg-new"
	_ = os.Remove(tmp)
	if err := os.Symlink(AgentBinary, tmp); err != nil {
		return fmt.Errorf("the gate link: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("the gate link: %w", err)
	}
	return nil
}

// maskPath is the unit file name that masks a uid's user manager.
func (o *Ops) maskPath(uid int) string {
	return filepath.Join(o.ShellPaths.SystemdDir, "user@"+strconv.Itoa(uid)+".service")
}

// maskUserManager masks (on) or unmasks a uid's user@.service (SPEC
// §5.4). Only a link to /dev/null is ever removed; a unit file an operator
// put at that name is left alone.
func (o *Ops) maskUserManager(ctx context.Context, uid int, on bool) error {
	p := o.maskPath(uid)
	t, err := os.Readlink(p)
	masked := err == nil && t == os.DevNull
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("read %s: %w", p, err)
	}
	other := !masked && !errors.Is(err, fs.ErrNotExist)
	switch {
	case on && !masked:
		if other {
			o.log().Warn("not masking the user manager: a unit file is there", "path", p)
			return nil
		}
		if err := os.Symlink(os.DevNull, p); err != nil {
			return fmt.Errorf("mask the user manager: %w", err)
		}
	case !on && masked:
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("unmask the user manager: %w", err)
		}
	default:
		return nil
	}
	if err := o.Systemd.DaemonReload(ctx); err != nil {
		return err
	}
	if on {
		// One started by a session before the mask stops now.
		unit := "user@" + strconv.Itoa(uid) + ".service"
		if err := o.Systemd.Stop(ctx, unit); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			o.log().Warn("stopping the user manager", "unit", unit, "err", err)
		}
	}
	return nil
}
