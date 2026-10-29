package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/backup"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cache"
	"github.com/cloudground-it/cloudground/internal/backuprepo"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// ResticVersion is the restic the agent runs (host SPEC, restic: Version),
// for Capabilities to report.
func (a *Agent) ResticVersion(ctx context.Context) (host.ResticVer, error) {
	return host.ResticVersion(ctx, a.exec())
}

// CheckRestic logs, at start, a restic without typed exit codes (Ubuntu
// 24.04's 0.16 on a box installed before the installer pinned 0.19.1, and
// kept by a self-update, which does not install packages): backups work,
// but a locked repository or a wrong password reads as a generic failure.
func (a *Agent) CheckRestic(ctx context.Context) {
	v, err := a.ResticVersion(ctx)
	switch {
	case err != nil:
		a.log().Warn("cannot read restic's version; backups will fail if restic is missing", "err", err)
	case !v.TypedExitCodes():
		a.log().Warn("restic is older than 0.17 (no typed exit codes); run the installer again to get the pinned release", "version", v.Line)
	}
}

// reconfigureRestored gives a WordPress site restored from another
// server's backup this panel's connector token in wp-config.php, and its
// database settings when the panel could not keep the backup's
// (Credentials; ops/backup SPEC §5), as provisioning wrote them. Secrets go
// on stdin, never in argv. Other site types keep their configuration: the
// panel gave them the backup's names, accounts and passwords.
func (a *Agent) reconfigureRestored(ctx context.Context, s state.Site, f rpc.ForeignRestore) error {
	if s.Type != state.SiteWordPress && s.Type != state.SiteWooCommerce {
		return nil
	}
	type step struct {
		what, secret string
		args         []string
	}
	var steps []step
	if d := s.Config.Database; f.Credentials && d.Enabled && !d.External {
		steps = append(steps,
			step{"database name", "", []string{"config", "set", "DB_NAME", d.Name}},
			step{"database user", "", []string{"config", "set", "DB_USER", d.User}},
			step{"database host", "", []string{"config", "set", "DB_HOST", d.Host + ":" + strconv.Itoa(d.Port)}},
			step{"database password", f.DBPassword, []string{"config", "set", "DB_PASSWORD", "--prompt"}})
	}
	if f.ConnectorToken != "" {
		steps = append(steps, step{"connector token", f.ConnectorToken, []string{"config", "set", "CLOUDGROUND_SITE_TOKEN", "--prompt"}})
	}
	for _, st := range steps {
		if _, err := a.wpcli().Run(ctx, host.Invocation{
			User: s.SystemUser, Path: path.Join(s.RootPath, "current"), Args: st.args, Stdin: st.secret,
			Slice: site.Slice(s.SystemUser), Sandbox: phpfpm.RunSandbox(s),
		}); err != nil {
			return fmt.Errorf("wp-config.php (%s): %w", st.what, err)
		}
	}
	return nil
}

// backupHandlers returns the agent's ops/backup handlers (backups, restores,
// restore tests, retention), built once: they keep the repository locks.
func (a *Agent) backupHandlers() *backup.Ops {
	a.backupOnce.Do(func() {
		r := a.Restic
		if r == nil {
			// restic's bounds come from the handlers; a cancelled request
			// stops its unit (host SPEC: helpers).
			cli := host.NewRestic(helpers(0))
			cli.Transient = true
			r = cli
		}
		purger := cache.Purger{Root: a.Paths.CacheRoot}
		a.backupOps = &backup.Ops{
			Restic: r,
			DB:     a.dbHandlers(),
			Owner:  a.siteOwner,
			Relink: relinkSite,
			RestartPHP: func(ctx context.Context, s state.Site) error {
				return a.sitesHandlers().RestartPHP(ctx, s)
			},
			RefreshPHP: a.refreshSiteState,
			ReloadNginx: func() error {
				return a.sitesHandlers().ReloadNginx(context.Background())
			},
			PurgeCache: func(s state.Site) error {
				_, err := purger.Purge(context.Background(), rpc.PurgeParams{Site: s})
				return err
			},
			Reconfigure: a.reconfigureRestored,
			WorkDir:     a.Paths.WorkDir,
			CacheDir:    os.Getenv("RESTIC_CACHE_DIR"),
			RepoRules: backuprepo.Options{
				AllowPrivate: a.AllowPrivateRepo,
				Rclone:       func() bool { _, err := exec.LookPath("rclone"); return err == nil },
			},
			Log: a.log(),
		}
	})
	return a.backupOps
}
