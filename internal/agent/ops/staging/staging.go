// Package staging clones a production site into its staging copy, compares
// the two, and pushes selected database tables back. SPEC.md in this
// directory is the contract.
package staging

import (
	"context"
	"log/slog"
	"path"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Ops holds what the handlers use. The site lifecycle (provision, teardown,
// render, verify) is the agent's; these are its unlocked inner functions,
// called while this package holds the sites' locks.
type Ops struct {
	// Open opens a site's tree; what it creates belongs to the site user.
	Open func(state.Site) (*sitefs.Root, error)
	// Lock takes a site's lock whatever the site's state; LockLive takes it
	// and fails, holding nothing, when the site no longer exists.
	Lock     func(domain string) func()
	LockLive func(state.Site) (func(), error)
	// Provision creates a site without taking its lock, and removes what
	// it made when it fails.
	Provision func(context.Context, rpc.CreateSiteParams) (rpc.CreateSiteResult, error)
	// Teardown removes a site completely without taking its lock.
	Teardown func(state.Site) error
	// Render writes a site's configuration and reloads what serves it.
	Render func(context.Context, state.Site) ([]rpc.ManagedFile, error)
	// Verify checks a provisioned site is whole.
	Verify func(context.Context, state.Site) error
	// Refresh makes a database change visible to a live site (object
	// cache and OPcache, ops/wp).
	Refresh func(context.Context, state.Site) error
	DB      *db.Ops
	WP      host.WPCLI
	// WorkDir is root's own scratch directory, for the dumps.
	WorkDir string
	Log     *slog.Logger
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.CreateStaging, o.Create)
	agentrpc.Handle(s, agentrpc.StagingDiff, o.Diff)
	agentrpc.Handle(s, agentrpc.StagingTables, o.Tables)
	agentrpc.Handle(s, agentrpc.SyncStagingDB, o.Push)
}

// AuthFile is the nginx password file of a password-protected site, in the
// directory dir: written when the site is rendered, removed with the site.
func AuthFile(dir, domain string) string {
	return filepath.Join(dir, domain+".htpasswd")
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// pair refuses a staging site that is not production's own copy, and one
// site named twice (its second lock would wait on the first forever).
func pair(prod, stg state.Site) error {
	if prod.Domain == stg.Domain || prod.ID == stg.ID {
		return agentrpc.Invalid("staging and production are the same site")
	}
	if stg.StagingOf == 0 || stg.StagingOf != prod.ID {
		return agentrpc.Invalid("%s is not a staging copy of %s", stg.Domain, prod.Domain)
	}
	return nil
}

// lockPair takes production's lock, then staging's, both sites required to
// exist. The returned release must be called; there is none on error.
func (o *Ops) lockPair(prod, stg state.Site) (func(), error) {
	unlockProd, err := o.LockLive(prod)
	if err != nil {
		return nil, err
	}
	unlockStg, err := o.LockLive(stg)
	if err != nil {
		unlockProd()
		return nil, err
	}
	return func() { unlockStg(); unlockProd() }, nil
}

func isWordPress(s state.Site) bool {
	return s.Type == state.SiteWordPress || s.Type == state.SiteWooCommerce
}

func localDB(s state.Site) bool {
	return s.Config.Database.Enabled && !s.Config.Database.External
}

// wp runs one wp-cli command as the site's user on its live release, with
// secret, when not empty, on stdin.
func (o *Ops) wp(ctx context.Context, s state.Site, secret string, args ...string) error {
	_, err := o.WP.Run(ctx, host.Invocation{
		User:    s.SystemUser,
		Path:    path.Join(s.RootPath, "current"),
		Args:    args,
		Stdin:   secret,
		Slice:   site.Slice(s.SystemUser),
		Sandbox: phpfpm.RunSandbox(s),
	})
	return err
}

// rewriteURLs replaces the site address from with to (SPEC §4): the plain
// form and the JSON-escaped one, each its own pass. extra limits the
// tables and columns.
func (o *Ops) rewriteURLs(ctx context.Context, s state.Site, from, to string, extra ...string) error {
	for _, form := range [][2]string{
		{"//" + from, "//" + to},
		{`\/\/` + from, `\/\/` + to},
	} {
		args := append([]string{"search-replace", form[0], form[1]}, extra...)
		if err := o.wp(ctx, s, "", args...); err != nil {
			return err
		}
	}
	return nil
}
