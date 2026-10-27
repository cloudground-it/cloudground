package sites

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// Delete tears a site down (SPEC §6). The server holds the site's lock.
func (o *Ops) Delete(ctx context.Context, p rpc.SiteRef) (map[string]any, error) {
	if err := o.Teardown(ctx, p.Site); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": p.Site.Domain}, nil
}

// Teardown is Delete for a caller that holds the site's lock (staging's
// rollback). Every part is removed if it is there, so it can be repeated.
func (o *Ops) Teardown(ctx context.Context, s state.Site) error {
	if err := site.CheckRoot(o.Paths.SitesRoot, s); err != nil {
		return err
	}
	if !site.ValidSystemUser(s.SystemUser) {
		return fmt.Errorf("invalid system user %q", s.SystemUser)
	}
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	keep(o.removeConfig(ctx, s))
	keep(o.Cron.RemoveSite(ctx, s.SystemUser))
	// Every database and account the agent records for the site, the
	// primary and any made since.
	keep(o.DB.DropSite(ctx, s.ID))
	keep(o.Certs.RemoveSite(ctx, s.Domain))
	// The shell login shares the user's uid and has its group as its
	// primary one: it goes first (R-4.5).
	keep(o.SFTP.RemoveShell(ctx, s))
	// The user goes before its files: nothing of it can be running while
	// they are removed.
	keep(o.deleteUser(ctx, s.SystemUser))
	keep(o.removeSiteDirs(s))
	if o.Paths.Quarantine != "" {
		if err := os.RemoveAll(filepath.Join(o.Paths.Quarantine, strconv.FormatInt(s.ID, 10))); err != nil {
			keep(fmt.Errorf("remove the site's quarantined files: %w", err))
		}
	}
	if err := os.RemoveAll(s.RootPath); err != nil {
		keep(fmt.Errorf("remove the site files: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("site cleanup incomplete: %w", err)
	}
	return nil
}

// removeSiteDirs removes the site's page cache and logs, which a render
// creates outside its tree.
func (o *Ops) removeSiteDirs(s state.Site) error {
	var errs []error
	for what, dir := range map[string]string{
		"page cache": filepath.Join(o.Paths.CacheRoot, velocity.SanitizeCacheDirName(s.Domain)),
		"logs":       filepath.Join(o.Paths.LogRoot, s.Domain),
	} {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove the %s: %w", what, err))
		}
	}
	return errors.Join(errs...)
}

// removeConfig takes the site off nginx, stops its slice and removes its
// PHP master (SPEC §6.1-6.3).
func (o *Ops) removeConfig(ctx context.Context, s state.Site) error {
	var errs []error
	remove := func(path string) bool {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
		return err == nil
	}

	o.NginxMu.Lock()
	remove(o.vhostPath(s))
	remove(o.cacheZonePath(s))
	remove(o.blockingPath(s))
	if o.Paths.NginxAuthDir != "" {
		remove(staging.AuthFile(o.Paths.NginxAuthDir, s.Domain))
		paths, _ := filepath.Glob(filepath.Join(o.Paths.NginxAuthDir, s.Domain+".path-*.htpasswd"))
		for _, p := range paths {
			remove(p)
		}
	}
	if err := o.reloadNginx(ctx); err != nil {
		errs = append(errs, err)
	}
	o.NginxMu.Unlock()

	if isApp(s) {
		if err := o.removeApp(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	if o.RemoveFunctions != nil {
		if err := o.RemoveFunctions(ctx, s.SystemUser); err != nil {
			errs = append(errs, err)
		}
	}
	// Everything the site runs is in its slice: one stop ends it all.
	if err := o.Systemd.Stop(ctx, site.Slice(s.SystemUser)); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
		errs = append(errs, err)
	}
	unit := phpfpm.UnitName(s.SystemUser)
	if _, err := os.Stat(o.unitPath(s)); err == nil {
		// A master started before it had a slice is stopped by name.
		if err := o.Systemd.Stop(ctx, unit); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
		if err := o.Systemd.Disable(ctx, unit); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
	}
	removed := remove(o.unitPath(s))
	if err := os.RemoveAll(o.dropinDir(s)); err != nil {
		errs = append(errs, err)
	}
	remove(o.poolPath(s))
	if remove(o.slicePath(s)) {
		removed = true
	}
	if removed {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// A master that had failed stays listed as failed after its file went.
	if err := o.Systemd.ResetFailed(ctx, unit); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
		o.log().Warn("resetting the PHP master's failed state", "unit", unit, "err", err)
	}
	return errors.Join(errs...)
}

// deleteUser removes the site's user and its group, if they exist (SPEC
// §6.8), after ending the user's processes outside the slice (a scheduled
// cron job, an SFTP session).
func (o *Ops) deleteUser(ctx context.Context, name string) error {
	if err := o.deleteUserOnly(ctx, name); err != nil {
		return err
	}
	// userdel keeps a user's own group while it has another member, and
	// www-data is one in every site's group: without this, each deleted
	// site left a system group behind, until groupadd ran out of system
	// GIDs (R-3.14).
	if err := o.Accounts.DeleteGroup(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchGroup) {
		return fmt.Errorf("remove the group %s: %w", name, err)
	}
	return nil
}

func (o *Ops) deleteUserOnly(ctx context.Context, name string) error {
	exists, err := o.Accounts.UserExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	// No new login of the user from here on (an SFTP session, crond).
	if err := o.Accounts.ExpireUser(ctx, name, true); err != nil && !errors.Is(err, host.ErrNoSuchUser) {
		return err
	}
	// The processes left outside the slice end before userdel, whatever it
	// would say: its own check cannot see them from the agent's sandbox
	// (reading /proc/<pid>/root of another uid needs CAP_SYS_PTRACE, and it
	// skips what it cannot read), so it deleted a user whose processes ran
	// on under a uid the next site is given (R-3.15).
	kill := o.KillUser
	if kill == nil {
		kill = killUser
	}
	if err := kill(ctx, name); err != nil {
		return fmt.Errorf("end the processes of %s: %w", name, err)
	}
	err = o.Accounts.DeleteUser(ctx, name, false)
	if errors.Is(err, host.ErrUserBusy) {
		// One started between the kill and userdel.
		o.log().Info("the site user still has processes; ending them", "user", name)
		if err := kill(ctx, name); err != nil {
			return fmt.Errorf("end the processes of %s: %w", name, err)
		}
		err = o.Accounts.DeleteUser(ctx, name, false)
	}
	if errors.Is(err, host.ErrNoSuchUser) {
		return nil
	}
	return err
}

// killWait bounds the wait for a killed user's processes to exit.
const killWait = 10 * time.Second

// KillUser is killUser, for the other users the agent removes (a
// function's, ops/functions SPEC §3).
func KillUser(ctx context.Context, name string) error { return killUser(ctx, name) }

// killUser sends SIGKILL to every process whose uid is the user's, and
// waits until none is left (zombies, already dead, do not count).
func killUser(ctx context.Context, name string) error {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return nil // no such account here: nothing runs as it
	}
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil || uid == 0 || !(site.ValidSystemUser(name) || functions.ValidUser(name)) {
		return fmt.Errorf("refusing to kill the processes of %s (uid %s)", name, u.Uid)
	}
	ctx, cancel := context.WithTimeout(ctx, killWait)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		left := 0
		for _, pid := range processesOf(uid) {
			left++
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if left == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d processes still running: %w", left, ctx.Err())
		case <-tick.C:
		}
	}
}

// processesOf lists the live (not zombie) processes with uid as their
// real, effective, saved or filesystem uid.
func processesOf(uid int) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		f, err := os.Open("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		match, zombie := false, false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "State:"); ok {
				zombie = strings.HasPrefix(strings.TrimSpace(v), "Z")
			}
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				for _, id := range strings.Fields(v) {
					if id == strconv.Itoa(uid) {
						match = true
					}
				}
			}
		}
		f.Close()
		if match && !zombie {
			out = append(out, pid)
		}
	}
	return out
}
