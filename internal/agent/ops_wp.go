package agent

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/state"
)

// wpHandlers returns the agent's ops/wp handlers (WordPress toolkit, object
// cache, cache warmer, installation), built once from its adapters.
func (a *Agent) wpHandlers() *wp.Ops {
	a.wpOnce.Do(func() {
		a.wpOps = &wp.Ops{
			WP:       a.wpcli(),
			OpenSite: a.openSite,
			PHP:      sitePHP{a},
			Log:      a.log(),
		}
	})
	return a.wpOps
}

// sitePHP is a site's own PHP-FPM master (ops/sites), for ops/wp.
type sitePHP struct{ a *Agent }

func (p sitePHP) Reload(ctx context.Context, s state.Site) error {
	return p.a.sitesHandlers().ReloadPHP(ctx, s)
}

func (p sitePHP) Restart(ctx context.Context, s state.Site) error {
	return p.a.sitesHandlers().RestartPHP(ctx, s)
}

// refreshSiteState lets the running site see a change made behind its back
// to its database or files, touching only that site (ops/wp SPEC §7).
func (a *Agent) refreshSiteState(ctx context.Context, s state.Site) error {
	return a.wpHandlers().Refresh(ctx, s)
}
