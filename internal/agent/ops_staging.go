package agent

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// stagingHandlers returns the agent's ops/staging handlers (clone, diff,
// tables, database push), built once. They hold both sites' locks and call
// ops/sites's unlocked inner functions only.
func (a *Agent) stagingHandlers() *staging.Ops {
	a.stagingOnce.Do(func() {
		a.stagingOps = &staging.Ops{
			Open:     a.openSite,
			Lock:     a.lockSite,
			LockLive: a.lockLiveSite,
			Provision: func(ctx context.Context, p rpc.CreateSiteParams) (rpc.CreateSiteResult, error) {
				return a.sitesHandlers().Provision(ctx, p)
			},
			Teardown: func(s state.Site) error {
				return a.sitesHandlers().Teardown(context.Background(), s)
			},
			Render: func(ctx context.Context, s state.Site) ([]rpc.ManagedFile, error) {
				return a.sitesHandlers().Render(ctx, s)
			},
			Verify: func(ctx context.Context, s state.Site) error {
				return a.sitesHandlers().Verify(ctx, s)
			},
			Refresh: a.refreshSiteState,
			DB:      a.dbHandlers(),
			WP:      a.wpHandlers().WP,
			WorkDir: a.Paths.WorkDir,
			Log:     a.log(),
		}
	})
	return a.stagingOps
}
