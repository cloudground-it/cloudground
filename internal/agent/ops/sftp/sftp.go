// Package sftp turns a site's SFTP login on and off and keeps the public
// keys that may sign in as the site user. SPEC.md in this directory is the
// contract.
package sftp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agent/ops/system"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// DenyGroup is named in the installer's sshd DenyGroups: a member cannot
// sign in at all, by password or by key.
const DenyGroup = "cg-sftp-off"

// KeysGroup is named in the installer's sshd drop-in: a member signs in
// with a key only, sshd does not offer it a password (SPEC §3.1). Its
// password is locked as well, which refuses one on its own.
const KeysGroup = "cg-sftp-keys"

// minPassword is the shortest SFTP password accepted (the API checks too).
const minPassword = 12

// Ops holds what the handlers use.
type Ops struct {
	Accounts host.Accounts
	// Open opens a site's tree, creating entries for the site user.
	Open func(s state.Site) (*sitefs.Root, error)
	// Port is sshd's listening port. Nil: 22.
	Port func(ctx context.Context) int
	// The site shell (SPEC §5): systemd, the runner of sshd -t and the
	// paths. A nil Systemd or Exec turns it off.
	Systemd    ShellSystemd
	Exec       host.Exec
	ShellPaths ShellPaths
	// LookupGID resolves the shell group for the gate socket; nil is the
	// group database.
	LookupGID func(group string) (int, error)
	// Spawn starts a site shell's systemd-run with the given stdio (nil:
	// /dev/null) and returns its wait; nil is the real one (§5.4).
	Spawn func(argv []string, stdio [3]*os.File) (wait func() int, err error)
	Log   *slog.Logger

	groupMu    sync.Mutex
	groupReady bool
	keyLocks   sync.Map // site root -> *sync.Mutex
	shellMu    sync.Mutex
	gate       gateLimits
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.SetSFTP, o.SetSFTP)
	agentrpc.Handle(s, agentrpc.SSHKeys, o.SSHKeys)
	agentrpc.Handle(s, agentrpc.SetShell, o.SetShell)
}

// ensureGroup creates the deny group and the keys-only group the first
// time they are needed in this process; a failure is retried on the next
// call.
func (o *Ops) ensureGroup(ctx context.Context) error {
	o.groupMu.Lock()
	defer o.groupMu.Unlock()
	if o.groupReady {
		return nil
	}
	for _, g := range []string{DenyGroup, KeysGroup} {
		if err := o.Accounts.EnsureGroup(ctx, g, true); err != nil {
			return fmt.Errorf("SFTP access group %s: %w", g, err)
		}
	}
	o.groupReady = true
	return nil
}

// SetAccess takes user out of the deny group (allow) or puts it in. Both
// directions are idempotent; any other accounts error is returned.
func (o *Ops) SetAccess(ctx context.Context, user string, allow bool) error {
	if err := o.setMember(ctx, user, DenyGroup, !allow); err != nil {
		if allow {
			return fmt.Errorf("allow SFTP access: %w", err)
		}
		return fmt.Errorf("block SFTP access: %w", err)
	}
	return nil
}

// keysDropIn is the sshd configuration of the keys-only group (SPEC §3.1).
const keysDropIn = `# Managed by CloudGround: SFTP logins set to keys only. Do not edit.
# Read before 10-cloudground-sftp.conf, which allows passwords for every
# cg-site-*: sshd keeps the first value it reads for a setting.
Match Group ` + KeysGroup + `
    PasswordAuthentication no
    KbdInteractiveAuthentication no
`

// ensureKeysHost puts the keys-only drop-in in place (checked with sshd -t,
// then sshd reloaded). Without systemd or a runner (tests) there is none,
// and the locked password alone refuses passwords.
func (o *Ops) ensureKeysHost(ctx context.Context) error {
	if o.Systemd == nil || o.Exec == nil || o.ShellPaths.KeysDropIn == "" {
		return nil
	}
	o.shellMu.Lock()
	defer o.shellMu.Unlock()
	if d := o.ShellPaths.SSHDRunDir; d != "" {
		if err := rootfile.MkdirMode(d, 0o755); err != nil {
			return fmt.Errorf("%s: %w", d, err)
		}
	}
	if _, err := system.ApplySSHDDropIn(ctx, o.Exec, o.Systemd, o.ShellPaths.KeysDropIn, []byte(keysDropIn)); err != nil {
		return err
	}
	return nil
}

// setKeysOnly puts user in the keys-only group or takes it out.
func (o *Ops) setKeysOnly(ctx context.Context, user string, on bool) error {
	if err := o.setMember(ctx, user, KeysGroup, on); err != nil {
		return fmt.Errorf("SFTP keys-only: %w", err)
	}
	return nil
}

// setMember adds user to group or removes it, idempotently.
func (o *Ops) setMember(ctx context.Context, user, group string, in bool) error {
	err := o.setMemberOnce(ctx, user, group, in)
	if errors.Is(err, host.ErrNoSuchGroup) {
		// The group went away since it was ensured: make it again, once.
		o.groupMu.Lock()
		o.groupReady = false
		o.groupMu.Unlock()
		err = o.setMemberOnce(ctx, user, group, in)
	}
	return err
}

func (o *Ops) setMemberOnce(ctx context.Context, user, group string, in bool) error {
	if err := o.ensureGroup(ctx); err != nil {
		return err
	}
	if in {
		return o.Accounts.AddToGroup(ctx, user, group)
	}
	if err := o.Accounts.RemoveFromGroup(ctx, user, group); err != nil && !errors.Is(err, host.ErrNotMember) {
		return err
	}
	return nil
}

// SetSFTP turns the site user's SFTP login on, with a password or for keys
// only, or off (SPEC §3).
func (o *Ops) SetSFTP(ctx context.Context, p rpc.SFTPParams) (map[string]string, error) {
	user := p.Site.SystemUser
	if !p.Enable {
		if err := o.SetAccess(ctx, user, false); err != nil {
			return nil, err
		}
		if err := o.shellAccess(ctx, user, false); err != nil {
			return nil, err
		}
		if err := o.Accounts.LockUser(ctx, user); err != nil {
			return nil, fmt.Errorf("lock the SFTP password: %w", err)
		}
		return map[string]string{"sftp": "disabled"}, nil
	}
	if p.KeysOnly {
		if p.Password != "" {
			return nil, agentrpc.Invalid("a keys-only SFTP login takes no password")
		}
		// sshd told and the password locked before access opens; a
		// drop-in sshd refuses changes nothing.
		if err := o.ensureKeysHost(ctx); err != nil {
			return nil, err
		}
		if err := o.Accounts.LockUser(ctx, user); err != nil {
			return nil, fmt.Errorf("lock the SFTP password: %w", err)
		}
		if err := o.setKeysOnly(ctx, user, true); err != nil {
			return nil, err
		}
	} else {
		if len(p.Password) < minPassword {
			return nil, agentrpc.Invalid("sftp password too short")
		}
		if err := o.Accounts.SetPassword(ctx, user, p.Password); err != nil {
			if errors.Is(err, host.ErrInvalidPassword) {
				return nil, agentrpc.Invalid("the SFTP password cannot contain a line break or a NUL character")
			}
			return nil, fmt.Errorf("set the SFTP password: %w", err)
		}
		if err := o.setKeysOnly(ctx, user, false); err != nil {
			return nil, err
		}
	}
	if err := o.SetAccess(ctx, user, true); err != nil {
		return nil, err
	}
	if err := o.shellAccess(ctx, user, true); err != nil {
		return nil, err
	}
	auth := "password"
	if p.KeysOnly {
		auth = "keys"
	}
	return map[string]string{"sftp": "enabled", "user": user, "auth": auth}, nil
}

// shellAccess gives the site's shell login, when it has one, the access the
// site user has (SPEC §5.5); turning access off ends the open shells.
func (o *Ops) shellAccess(ctx context.Context, user string, allow bool) error {
	alias := site.ShellUser(user)
	exists, err := o.Accounts.UserExists(ctx, alias)
	if err != nil || !exists {
		return err
	}
	if err := o.SetAccess(ctx, alias, allow); err != nil {
		return err
	}
	if !allow {
		return o.stopShells(ctx, user)
	}
	return nil
}
