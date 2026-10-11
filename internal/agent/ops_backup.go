package agent

import (
	"context"
	"os"
	"os/exec"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/backup"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cache"
	"github.com/cloudground-it/cloudground/internal/backuprepo"
	"github.com/cloudground-it/cloudground/internal/rpc"
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

// backupHandlers returns the agent's ops/backup handlers (backups, restores,
// restore tests, retention), built once: they keep the repository locks.
func (a *Agent) backupHandlers() *backup.Ops {
	a.backupOnce.Do(func() {
		r := a.Restic
		if r == nil {
			// restic's bounds come from the handlers; OSExec kills its
			// process group when the request ends.
			cli := host.NewRestic(host.OSExec{})
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
			WorkDir:  a.Paths.WorkDir,
			CacheDir: os.Getenv("RESTIC_CACHE_DIR"),
			RepoRules: backuprepo.Options{
				AllowPrivate: a.AllowPrivateRepo,
				Rclone:       func() bool { _, err := exec.LookPath("rclone"); return err == nil },
			},
			Log: a.log(),
		}
	})
	return a.backupOps
}
