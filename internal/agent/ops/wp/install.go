package wp

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Where a release's configuration and uploads live: in shared/, reached by
// links relative to the release (ops/release SPEC §1).
const (
	ConfigLink    = "wp-config.php"
	ConfigTarget  = "../../shared/wp-config.php"
	UploadsLink   = "wp-content/uploads"
	UploadsTarget = "../../../shared/uploads"
	sharedConfig  = "shared/wp-config.php"
)

// ConnectorEndpoint is where the connector reaches the panel.
const ConnectorEndpoint = "http://127.0.0.1:9080"

// Install is what a new WordPress is installed with (SPEC §11). The secrets
// reach wp-cli on stdin only.
type Install struct {
	DBPassword     string
	AdminUser      string
	AdminPassword  string
	AdminEmail     string
	Title          string // "" means the domain
	ConnectorToken string // "" leaves the connector unconfigured
}

// step is one wp-cli command of an installation, with the secret it reads
// on stdin ("" for none) and what it does, for the error.
type step struct {
	what   string
	secret string
	args   []string
}

// InstallWordPress installs WordPress into release, a fresh directory of
// r (the site's tree, whose live link already points at it): core, its
// configuration in shared/, uploads in shared/, the connector (SPEC §11).
func (o *Ops) InstallWordPress(ctx context.Context, r *sitefs.Root, s state.Site, release string, in Install) error {
	run := func(st step) error {
		_, err := o.WP.Run(ctx, host.Invocation{
			User:    s.SystemUser,
			Path:    path.Join(s.RootPath, release),
			Args:    st.args,
			Stdin:   st.secret,
			Slice:   site.Slice(s.SystemUser),
			Sandbox: phpfpm.RunSandbox(s),
		})
		if err != nil {
			return fmt.Errorf("%s: %w", st.what, err)
		}
		return nil
	}
	db := s.Config.Database
	steps := []step{
		{"download WordPress", "", []string{"core", "download", "--locale=en_US"}},
		{"write wp-config.php", in.DBPassword, []string{"config", "create",
			"--dbname=" + db.Name, "--dbuser=" + db.User, "--prompt=dbpass",
			"--dbhost=" + db.Host + ":" + strconv.Itoa(db.Port), "--skip-check"}},
	}
	if in.ConnectorToken != "" {
		steps = append(steps,
			// A bare --prompt reads the positional value of `config set`.
			step{"set the connector token", in.ConnectorToken, []string{"config", "set", "CLOUDGROUND_SITE_TOKEN", "--prompt"}},
			step{"set the connector endpoint", "", []string{"config", "set", "CLOUDGROUND_ENDPOINT", ConnectorEndpoint}},
			// Scheduled tasks run from the site's crontab (ops/cron), not
			// inside visitors' requests.
			step{"turn wp-cron off", "", []string{"config", "set", "DISABLE_WP_CRON", "true", "--raw"}},
		)
	}
	title := in.Title
	if title == "" {
		title = s.Domain
	}
	steps = append(steps, step{"install WordPress", in.AdminPassword, []string{"core", "install",
		"--url=https://" + s.Domain, "--title=" + title,
		"--admin_user=" + in.AdminUser, "--prompt=admin_password",
		"--admin_email=" + in.AdminEmail, "--skip-email"}})
	for _, st := range steps {
		if err := run(st); err != nil {
			return err
		}
	}
	// Pretty permalinks: the core sitemap the warmer reads needs them. A
	// site works without, so a failure is reported, not fatal.
	if err := run(step{"set pretty permalinks", "", []string{"rewrite", "structure", "/%postname%/", "--hard"}}); err != nil {
		o.log().Warn("setting pretty permalinks failed", "site", s.Domain, "err", err)
	}
	return linkShared(r, release)
}

// linkShared moves the release's wp-config.php into shared/ (readable by
// the site user alone), points the release's config and uploads at shared/
// with relative links, and adds the connector.
func linkShared(r *sitefs.Root, release string) error {
	cfg := path.Join(release, ConfigLink)
	if fi, err := r.Lstat(cfg); err == nil && fi.Mode().IsRegular() {
		if err := r.Rename(cfg, sharedConfig, 0); err != nil {
			return fmt.Errorf("move wp-config.php to shared/: %w", err)
		}
		if err := r.Chmod(sharedConfig, 0o600); err != nil {
			return fmt.Errorf("restrict wp-config.php: %w", err)
		}
		if err := r.Symlink(ConfigTarget, cfg); err != nil {
			return err
		}
	}
	uploads := path.Join(release, UploadsLink)
	if err := r.MkdirAll(path.Dir(uploads), 0o750); err != nil {
		return err
	}
	if err := r.RemoveAll(uploads); err != nil {
		return err
	}
	if err := r.Symlink(UploadsTarget, uploads); err != nil {
		return err
	}
	return InstallConnector(r, release)
}

// InstallWooCommerce installs and activates WooCommerce on the live
// release, persists the rewrite rules for its pages (shop, cart, checkout,
// account), and turns off the cart-fragments request that bypasses the page
// cache on every page (SPEC §11).
func (o *Ops) InstallWooCommerce(ctx context.Context, s state.Site) error {
	for _, st := range []step{
		{"install WooCommerce", "", []string{"plugin", "install", "woocommerce", "--activate"}},
		{"flush the rewrite rules", "", []string{"rewrite", "flush"}},
		{"turn cart fragments off", "", []string{"config", "set", "CLOUDGROUND_DISABLE_CART_FRAGMENTS", "true", "--raw"}},
	} {
		if _, err := o.Run(ctx, s, st.args...); err != nil {
			return fmt.Errorf("%s: %w", st.what, err)
		}
	}
	return nil
}
