package agent

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/agent/ops/migration"
	"github.com/cloudground-it/cloudground/internal/state"
)

// migrationHandlers returns the agent's ops/migration handler, wired to the
// release, database and WordPress handlers it builds on. It keeps no state.
func (a *Agent) migrationHandlers() *migration.Ops {
	return &migration.Ops{
		WorkDir:   a.Paths.WorkDir,
		Open:      a.openSite,
		Releases:  a.releaseHandlers(),
		WordPress: a.wpHandlers(),
		// The dump goes into a scratch database and is renamed into place:
		// the live database is never without the tables it serves from
		// (ops/migration SPEC §4.3).
		StageDB: func(ctx context.Context, s state.Site, database, dump string) (migration.Staged, error) {
			name, err := a.dbHandlers().SiteDatabase(ctx, s, database)
			if err != nil {
				return nil, err
			}
			st, err := a.dbHandlers().Stage(ctx, name, dump)
			if err != nil {
				return nil, err
			}
			return st, nil
		},
		Log: a.log(),
	}
}

// SweepMigrations takes out of WordPress maintenance any site a migration
// cut short by the agent's death left in it. Run it at start, before
// serving.
func (a *Agent) SweepMigrations() error {
	return a.migrationHandlers().Sweep()
}
