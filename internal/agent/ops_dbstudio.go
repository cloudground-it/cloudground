package agent

import (
	"github.com/cloudground-it/cloudground/internal/agent/ops/dbstudio"
)

// dbStudioHandlers returns Quarry's handlers, over the database domain's
// sessions and the WordPress domain's wp-cli and refresh.
func (a *Agent) dbStudioHandlers() *dbstudio.Ops {
	a.dbStudioOnce.Do(func() {
		a.dbStudio = &dbstudio.Ops{
			DB:      a.dbHandlers(),
			Open:    a.openSite,
			WP:      a.wpHandlers().Run,
			Refresh: a.refreshSiteState,
			Log:     a.log(),
			LogRoot: a.Paths.LogRoot,
		}
	})
	return a.dbStudio
}
