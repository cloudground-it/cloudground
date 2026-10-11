package agent

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/state"
)

// releaseHandlers returns the agent's ops/release handlers (deploy,
// promote, rollback), built once.
func (a *Agent) releaseHandlers() *release.Ops {
	a.releaseOnce.Do(func() {
		a.releaseOps = &release.Ops{
			Open: a.openSite,
			ReloadPHP: func(ctx context.Context, s state.Site) error {
				return a.sitesHandlers().ReloadPHP(ctx, s)
			},
			Log: a.log(),
		}
	})
	return a.releaseOps
}
