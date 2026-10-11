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
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// DenyGroup is named in the installer's sshd DenyGroups: a member cannot
// sign in at all, by password or by key.
const DenyGroup = "cg-sftp-off"

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

// ensureGroup creates the deny group the first time it is needed in this
// process; a failure is retried on the next call.
func (o *Ops) ensureGroup(ctx context.Context) error {
	o.groupMu.Lock()
	defer o.groupMu.Unlock()
	if o.groupReady {
		return nil
	}
	if err := o.Accounts.EnsureGroup(ctx, DenyGroup, true); err != nil {
		return fmt.Errorf("SFTP access group: %w", err)
	}
	o.groupReady = true
	return nil
}

// SetAccess takes user out of the deny group (allow) or puts it in. Both
// directions are idempotent; any other accounts error is returned.
func (o *Ops) SetAccess(ctx context.Context, user string, allow bool) error {
	err := o.setAccess(ctx, user, allow)
	if errors.Is(err, host.ErrNoSuchGroup) {
		// The group went away since it was ensured: make it again, once.
		o.groupMu.Lock()
		o.groupReady = false
		o.groupMu.Unlock()
		err = o.setAccess(ctx, user, allow)
	}
	return err
}

func (o *Ops) setAccess(ctx context.Context, user string, allow bool) error {
	if err := o.ensureGroup(ctx); err != nil {
		return err
	}
	if allow {
		if err := o.Accounts.RemoveFromGroup(ctx, user, DenyGroup); err != nil && !errors.Is(err, host.ErrNotMember) {
			return fmt.Errorf("allow SFTP access: %w", err)
		}
		return nil
	}
	if err := o.Accounts.AddToGroup(ctx, user, DenyGroup); err != nil {
		return fmt.Errorf("block SFTP access: %w", err)
	}
	return nil
}

// SetSFTP turns the site user's SFTP login on, with a password, or off.
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
	if len(p.Password) < minPassword {
		return nil, agentrpc.Invalid("sftp password too short")
	}
	if err := o.Accounts.SetPassword(ctx, user, p.Password); err != nil {
		if errors.Is(err, host.ErrInvalidPassword) {
			return nil, agentrpc.Invalid("the SFTP password cannot contain a line break or a NUL character")
		}
		return nil, fmt.Errorf("set the SFTP password: %w", err)
	}
	if err := o.SetAccess(ctx, user, true); err != nil {
		return nil, err
	}
	if err := o.shellAccess(ctx, user, true); err != nil {
		return nil, err
	}
	return map[string]string{"sftp": "enabled", "user": user}, nil
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
