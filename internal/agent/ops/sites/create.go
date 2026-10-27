package sites

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The first release of every site and the link that serves it.
const (
	firstRelease = "releases/" + site.InitialRelease
	liveLink     = "current"
)

// undoTimeout bounds the clean-up of a failed creation, which runs even
// when the request that asked for it is gone.
const undoTimeout = 5 * time.Minute

// Create provisions a site (SPEC §5). The server holds the site's lock.
func (o *Ops) Create(ctx context.Context, p rpc.CreateSiteParams) (rpc.CreateSiteResult, error) {
	return o.Provision(ctx, p)
}

// Provision is Create for a caller that holds the site's lock (staging).
// On failure, everything it made is undone, in reverse order, and nothing
// else: the checks first make sure none of it was there before.
func (o *Ops) Provision(ctx context.Context, p rpc.CreateSiteParams) (res rpc.CreateSiteResult, err error) {
	s := p.Site
	if err := o.refuseExisting(ctx, s); err != nil {
		return res, err
	}
	var undo []func(context.Context) error
	defer func() {
		if err == nil {
			return
		}
		o.log().Warn("provisioning failed, undoing it", "site", s.Domain, "err", err)
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
		defer cancel()
		for i := len(undo) - 1; i >= 0; i-- {
			if uerr := undo[i](uctx); uerr != nil {
				o.log().Error("undoing a provisioning step failed", "site", s.Domain, "err", uerr)
			}
		}
	}()

	// The user, with no SFTP until the operator turns it on.
	if err := o.Accounts.CreateUser(ctx, host.UserSpec{
		Name: s.SystemUser, System: true, Home: s.RootPath, Shell: "/usr/sbin/nologin",
	}); err != nil {
		return res, fmt.Errorf("create the site user: %w", err)
	}
	undo = append(undo, func(ctx context.Context) error { return o.deleteUser(ctx, s.SystemUser) })
	if err := o.SFTP.SetAccess(ctx, s.SystemUser, false); err != nil {
		return res, err
	}

	// The tree.
	// The sites' root is traversed by nginx's workers and every site's user:
	// 0755 if the agent has to make it, whatever its umask.
	if err := rootfile.MkdirAllMode(filepath.Dir(s.RootPath), 0o755); err != nil {
		return res, err
	}
	if err := os.Mkdir(s.RootPath, 0o750); err != nil {
		return res, err
	}
	undo = append(undo, func(context.Context) error { return os.RemoveAll(s.RootPath) })
	r, err := o.Open(s)
	if err != nil {
		return res, err
	}
	defer r.Close()
	if err := o.makeTree(ctx, r, s); err != nil {
		return res, err
	}
	// The sandbox wp-cli and cron jobs run in binds the site's log
	// directory, which the first render would otherwise create only after
	// WordPress is downloaded: systemd then refuses to start the unit
	// (226/NAMESPACE, measured on the test VPS).
	undo = append(undo, func(context.Context) error { return o.removeSiteDirs(s) })
	if err := o.prepareDirs(s, o.input(ctx, s)); err != nil {
		return res, err
	}
	res.SystemUser, res.RootPath = s.SystemUser, s.RootPath

	// The database.
	if localDB(s) {
		undo = append(undo, func(ctx context.Context) error { return o.DB.DropSite(ctx, s.ID) })
		if err := o.DB.ProvisionSite(ctx, s, p.DBPassword); err != nil {
			return res, err
		}
		res.DatabaseName, res.DatabaseUser = s.Config.Database.Name, s.Config.Database.User
	}

	// The first release, live, with the application in it.
	if err := r.Mkdir(firstRelease, 0o750); err != nil {
		return res, err
	}
	if err := rootOwned(r).Symlink(firstRelease, liveLink); err != nil {
		return res, err
	}
	if err := o.bootstrap(ctx, r, s, p); err != nil {
		return res, err
	}
	if _, err := release.Precompress(ctx, r, firstRelease, 0); err != nil {
		o.log().Warn("precompressing static assets failed", "site", s.Domain, "err", err)
	}
	if err := r.ChownTree(firstRelease); err != nil {
		return res, err
	}

	// The configuration, loaded.
	undo = append(undo, func(ctx context.Context) error {
		return errors.Join(o.removeConfig(ctx, s), o.removeSiteDirs(s))
	})
	if res.Files, err = o.Render(ctx, s); err != nil {
		return res, err
	}
	if isWordPress(s) {
		// WordPress's own events, which no longer run on page views.
		undo = append(undo, func(ctx context.Context) error { return o.Cron.RemoveSite(ctx, s.SystemUser) })
		if _, err := o.Cron.Apply(ctx, rpc.CronParams{Site: s}); err != nil {
			return res, err
		}
	}
	if err := o.Verify(ctx, s); err != nil {
		return res, fmt.Errorf("verify the provisioned site: %w", err)
	}
	return res, nil
}

// refuseExisting refuses a site whose PHP version is not installed, or any
// of whose parts already exists: the undo of a failed creation removes them
// by name, and must never remove another site's.
func (o *Ops) refuseExisting(ctx context.Context, s state.Site) error {
	if err := o.checkPHPInstalled(s.PHPVersion); err != nil {
		return err
	}
	if exists, err := o.Accounts.UserExists(ctx, s.SystemUser); err != nil {
		return err
	} else if exists {
		return agentrpc.Conflict("system user %s already exists; refusing to provision over it", s.SystemUser)
	}
	if _, err := os.Lstat(s.RootPath); err == nil {
		return agentrpc.Wrap(agentrpc.CodeConflict, "the site's directory already exists; refusing to provision over it",
			fmt.Errorf("%s already exists", s.RootPath))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if localDB(s) {
		exists, err := o.DB.Exists(ctx, s.Config.Database.Name)
		if err != nil {
			return err
		}
		if exists {
			return agentrpc.Conflict("database %s already exists; refusing to provision over it", s.Config.Database.Name)
		}
	}
	return nil
}

// makeTree creates the skeleton: shared data, logs and sessions are the
// site's; the root and releases/ are root's with the site's group, as
// OpenSSH's chroot requires; nginx joins the site's group to read it.
func (o *Ops) makeTree(ctx context.Context, r *sitefs.Root, s state.Site) error {
	for _, d := range []string{"shared/uploads", "logs", "tmp/sessions"} {
		if err := r.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	if err := release.LockSkeleton(r); err != nil {
		return err
	}
	return o.Accounts.AddToGroup(ctx, "www-data", s.SystemUser)
}

// bootstrap puts the application into the first release (SPEC §5).
func (o *Ops) bootstrap(ctx context.Context, r *sitefs.Root, s state.Site, p rpc.CreateSiteParams) error {
	switch s.Type {
	case state.SiteWordPress, state.SiteWooCommerce:
		if err := o.WordPress.InstallWordPress(ctx, r, s, firstRelease, wp.Install{
			DBPassword: p.DBPassword, AdminUser: p.AdminUser, AdminPassword: p.AdminPassword,
			AdminEmail: p.AdminEmail, Title: p.SiteTitle, ConnectorToken: p.ConnectorToken,
		}); err != nil {
			return err
		}
		// The object cache is on by default: logged-in and cold pages gain
		// most. A site works without it.
		if err := o.WordPress.InstallObjectCache(ctx, s); err != nil {
			o.log().Warn("object cache setup failed", "site", s.Domain, "err", err)
		}
		if s.Type == state.SiteWooCommerce || s.Profile == state.ProfileCommerce {
			if err := o.WordPress.InstallWooCommerce(ctx, s); err != nil {
				return fmt.Errorf("woocommerce: %w", err)
			}
		}
	case state.SiteStatic:
		return r.WriteFile(firstRelease+"/index.html", []byte(welcomePage(s.Domain, false)), 0o644)
	case state.SiteNode, state.SitePython:
		return o.bootstrapApp(ctx, r, s)
	case state.SitePHP, state.SiteLaravel:
		dir := firstRelease
		if s.Config.PublicRoot != "" {
			dir = path.Join(dir, s.Config.PublicRoot)
			if err := r.MkdirAll(dir, 0o750); err != nil {
				return err
			}
		}
		return r.WriteFile(dir+"/index.php", []byte(welcomePage(s.Domain, true)), 0o644)
	}
	return nil
}

// welcomePage is what a new site serves until something is deployed. The
// PHP one prints the version PHP itself reports, so it shows PHP runs.
func welcomePage(domain string, php bool) string {
	name := html.EscapeString(domain)
	running := ""
	if php {
		running = ` It runs PHP <?= htmlspecialchars(PHP_VERSION, ENT_QUOTES) ?>.`
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>` + name + `</title></head>
<body style="margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:system-ui,sans-serif">
<main style="text-align:center;padding:1rem"><h1>` + name + `</h1>
<p>This site is ready on CloudGround.` + running + ` Deploy your code to replace this page.</p></main>
</body></html>
`
}

// rootOwned is r creating entries for root with the site's group.
func rootOwned(r *sitefs.Root) *sitefs.Root {
	return r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID})
}

// Verify checks that a provisioned site has every part (SPEC §5).
func (o *Ops) Verify(ctx context.Context, s state.Site) error {
	if ok, err := o.Accounts.UserExists(ctx, s.SystemUser); err != nil {
		return fmt.Errorf("look up the site user: %w", err)
	} else if !ok {
		return fmt.Errorf("the site user %s is missing", s.SystemUser)
	}
	r, err := o.Open(s)
	if err != nil {
		return fmt.Errorf("site tree unavailable: %w", err)
	}
	_, err = release.Live(r)
	r.Close()
	if err != nil {
		return fmt.Errorf("current release unavailable: %w", err)
	}
	if _, err := os.Stat(o.vhostPath(s)); err != nil {
		return fmt.Errorf("nginx site configuration unavailable: %w", err)
	}
	if isApp(s) {
		if _, err := os.Stat(o.appUnitPath(s)); err != nil {
			return fmt.Errorf("the app's unit unavailable: %w", err)
		}
	}
	if s.PHPVersion != "" {
		if _, err := os.Stat(o.poolPath(s)); err != nil {
			return fmt.Errorf("PHP pool configuration unavailable: %w", err)
		}
	}
	if localDB(s) {
		ok, err := o.DB.Exists(ctx, s.Config.Database.Name)
		if err != nil {
			return fmt.Errorf("database unavailable: %w", err)
		}
		if !ok {
			return errors.New("database unavailable: the database is missing")
		}
	}
	return nil
}
