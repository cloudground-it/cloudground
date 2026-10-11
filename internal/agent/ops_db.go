package agent

import (
	"context"
	"path"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// dbHandlers returns the agent's ops/db handlers, built once: they keep the
// tuning lock.
func (a *Agent) dbHandlers() *db.Ops {
	a.dbOnce.Do(func() {
		m := a.MariaDB
		if m == nil {
			m = host.NewMariaDB(a.Paths.MySQLSocket, a.Paths.WorkDir, helpers(time.Minute))
		}
		a.dbOps = &db.Ops{
			MariaDB:         m,
			Systemd:         a.Systemd,
			Open:            a.openSite,
			WorkDir:         a.Paths.WorkDir,
			Socket:          a.Paths.MySQLSocket,
			RegistryFile:    a.Paths.DBRegistry,
			PrimaryPassword: a.primaryDBPassword,
			Log:             a.Log,
		}
	})
	return a.dbOps
}

// primaryDBPassword writes a WordPress site's new database password into
// its wp-config.php (wp config set, the value on stdin). Other sites keep
// their credentials where their owner put them (a Laravel .env is the
// application's).
func (a *Agent) primaryDBPassword(ctx context.Context, s state.Site, password string) error {
	if s.Type != state.SiteWordPress && s.Type != state.SiteWooCommerce {
		return nil
	}
	_, err := a.wpcli().Run(ctx, host.Invocation{
		User:    s.SystemUser,
		Path:    path.Join(s.RootPath, "current"),
		Args:    []string{"config", "set", "DB_PASSWORD", "--prompt"},
		Stdin:   password,
		Slice:   site.Slice(s.SystemUser),
		Sandbox: phpfpm.RunSandbox(s),
	})
	return err
}

// LoadDatabaseRegistry reads the agent's record of each site's databases
// and accounts, rebuilding it from the server the first time (ops/db SPEC
// §2). Run it at start; a failure is retried on first use.
func (a *Agent) LoadDatabaseRegistry(ctx context.Context) error {
	return a.dbHandlers().LoadRegistry(ctx)
}

// AdoptDatabases rebuilds the record of the sites' databases from the
// panel's claims and the server (ops/db SPEC §2, "A lost record"): the
// `panel-agent adopt-databases` command, run by root with the agent
// stopped.
func (a *Agent) AdoptDatabases(ctx context.Context, claims state.DatabaseClaims, dryRun bool) (db.AdoptReport, error) {
	return a.dbHandlers().Adopt(ctx, claims, dryRun)
}

// SweepDatabase drops the scoped accounts and restore-test databases a
// previous agent left behind. Run it at start, before serving.
func (a *Agent) SweepDatabase(ctx context.Context) error {
	return a.dbHandlers().Sweep(ctx)
}

// HardenDatabase applies MariaDB's security settings (ops/db SPEC §7). Run
// it at start; a restart they need runs in the background.
func (a *Agent) HardenDatabase(ctx context.Context) error {
	restart, err := a.dbHandlers().Harden(ctx)
	if restart != nil {
		go restart()
	}
	return err
}
