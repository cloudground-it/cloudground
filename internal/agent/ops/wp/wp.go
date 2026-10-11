// Package wp serves the agent's WordPress methods (login link, plugins,
// updates, object cache, cache statistics) and the page-cache warmer, which
// works for every site type. SPEC.md in this directory is the contract.
package wp

import (
	"context"
	"log/slog"
	"path"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// PHP controls a site's own PHP-FPM master.
type PHP interface {
	// Reload makes the master pick up new code (OPcache). APCu survives it.
	Reload(ctx context.Context, s state.Site) error
	// Restart replaces the master, and with it the site's APCu and OPcache.
	Restart(ctx context.Context, s state.Site) error
}

// Ops holds what the handlers use.
type Ops struct {
	WP host.WPCLI
	// OpenSite opens a site's tree; what it creates belongs to the site user.
	OpenSite func(state.Site) (*sitefs.Root, error)
	PHP      PHP
	// LoopbackAddr is where a site's own pages are requested. Empty means
	// 127.0.0.1:443.
	LoopbackAddr string
	// WarmWorkers is the warmer's concurrency. Zero means 2 × CPUs, within
	// [2, 8].
	WarmWorkers int
	Log         *slog.Logger
	Now         func() time.Time
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.WPMagicLogin, o.MagicLogin)
	agentrpc.Handle(s, agentrpc.WPPlugins, o.Plugins)
	agentrpc.Handle(s, agentrpc.WPUpdate, o.Update)
	agentrpc.Handle(s, agentrpc.WPObjectCache, o.ObjectCache)
	agentrpc.Handle(s, agentrpc.CacheStats, o.CacheStats)
	agentrpc.Handle(s, agentrpc.WarmCache, o.WarmCache)
}

// Run runs one wp-cli command as the site's user against its live release.
func (o *Ops) Run(ctx context.Context, s state.Site, args ...string) ([]byte, error) {
	return o.WP.Run(ctx, host.Invocation{
		User:    s.SystemUser,
		Path:    path.Join(s.RootPath, liveLink),
		Args:    args,
		Slice:   site.Slice(s.SystemUser),
		Sandbox: phpfpm.RunSandbox(s),
	})
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func isWordPress(t state.SiteType) bool {
	return t == state.SiteWordPress || t == state.SiteWooCommerce
}
