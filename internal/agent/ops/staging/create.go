package staging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The staging copy's release and the layout links in it (ops/release SPEC
// §1), each with the relative target that resolves into the copy's own
// shared/.
const (
	cloneRelease  = "releases/" + "initial"
	sharedConfig  = "shared/wp-config.php"
	sharedUploads = "shared/uploads"
	sharedDropin  = "shared/object-cache.php"
	uploadsDir    = "wp-content/uploads"
)

var layoutLinks = []struct{ link, target string }{
	{"wp-config.php", "../../" + sharedConfig},
	{uploadsDir, "../../../" + sharedUploads},
	{wp.DropinLink, wp.DropinTarget},
}

// configLimit bounds the wp-config.php read from production.
const configLimit = 16 << 20

// Create clones production into its staging site (SPEC §2).
func (o *Ops) Create(ctx context.Context, p rpc.StagingParams) (rpc.CreateSiteResult, error) {
	prod, stg := p.Production, p.Staging
	if err := pair(prod, stg); err != nil {
		return rpc.CreateSiteResult{}, err
	}
	if isWordPress(prod) && prod.Config.Database.External {
		return rpc.CreateSiteResult{}, agentrpc.Invalid("a WordPress site on an external database cannot be staged")
	}
	if localDB(prod) {
		// Only the primary database is staged (SPEC §2.7): production's must
		// be recorded as production's, the copy's a name it may take.
		if _, err := o.DB.SiteDatabase(ctx, prod, ""); err != nil {
			return rpc.CreateSiteResult{}, err
		}
		if !localDB(stg) {
			return rpc.CreateSiteResult{}, agentrpc.Invalid("the staging site needs a database of its own")
		}
		if err := stagingDB(stg); err != nil {
			return rpc.CreateSiteResult{}, err
		}
	}
	if isWordPress(prod) {
		if err := stagingDB(stg); err != nil {
			return rpc.CreateSiteResult{}, err
		}
	}
	unlockProd, err := o.LockLive(prod)
	if err != nil {
		return rpc.CreateSiteResult{}, err
	}
	defer unlockProd()
	defer o.Lock(stg.Domain)()

	skeleton := stg
	if isWordPress(prod) {
		skeleton.Type = state.SitePHP // the clone replaces WordPress's bootstrap
	}
	res, err := o.Provision(ctx, rpc.CreateSiteParams{Site: skeleton, DBPassword: p.DBPassword})
	if err != nil {
		return res, err
	}
	done := false
	defer func() {
		if done {
			return
		}
		if err := o.Teardown(stg); err != nil {
			o.log().Error("removing the half-made staging site failed", "site", stg.Domain, "err", err)
		}
	}()

	if err := o.cloneFiles(prod, stg); err != nil {
		return res, err
	}
	if localDB(prod) {
		if err := o.cloneDatabase(ctx, prod, stg); err != nil {
			return res, err
		}
	}
	if isWordPress(prod) {
		if err := o.configureWordPress(ctx, prod, stg, p); err != nil {
			return res, err
		}
	}
	sr, err := o.Open(stg)
	if err != nil {
		return res, err
	}
	err = release.LockSkeleton(sr)
	sr.Close()
	if err != nil {
		return res, err
	}
	if res.Files, err = o.Render(ctx, stg); err != nil {
		return res, err
	}
	if err := o.Verify(ctx, stg); err != nil {
		return res, fmt.Errorf("verify the staging site: %w", err)
	}
	done = true
	return res, nil
}

// cloneFiles copies production's live release and uploads into staging,
// points the copy's layout links at staging's shared/, and gives staging
// its own configuration (SPEC §2.4-2.6).
func (o *Ops) cloneFiles(prod, stg state.Site) error {
	pr, err := o.Open(prod)
	if err != nil {
		return err
	}
	defer pr.Close()
	sr, err := o.Open(stg)
	if err != nil {
		return err
	}
	defer sr.Close()

	live, err := release.Live(pr)
	if err != nil {
		return fmt.Errorf("production's live release: %w", err)
	}
	if err := sr.RemoveAll(cloneRelease); err != nil {
		return fmt.Errorf("clone files: %w", err)
	}
	if err := sitefs.CopyTree(pr, live, sr, cloneRelease); err != nil {
		return fmt.Errorf("clone files: %w", err)
	}
	if err := relink(sr, cloneRelease); err != nil {
		return err
	}
	if _, err := pr.Lstat(sharedUploads); err == nil {
		if err := sr.RemoveAll(sharedUploads); err != nil {
			return fmt.Errorf("clone uploads: %w", err)
		}
		if err := sitefs.CopyTree(pr, sharedUploads, sr, sharedUploads); err != nil {
			return fmt.Errorf("clone uploads: %w", err)
		}
	}

	cfg, err := pr.ReadFile(sharedConfig, configLimit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read production's wp-config.php: %w", err)
	default:
		if err := sr.Replace(sharedConfig, 0o600, func(f *os.File) error {
			_, err := f.Write(cfg)
			return err
		}); err != nil {
			return fmt.Errorf("write staging's wp-config.php: %w", err)
		}
	}
	if fi, err := sr.Lstat(cloneRelease + "/" + wp.DropinLink); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if err := sr.Replace(sharedDropin, 0o640, func(f *os.File) error {
			_, err := io.WriteString(f, connector.ObjectCacheAPCu)
			return err
		}); err != nil {
			return fmt.Errorf("write staging's object-cache drop-in: %w", err)
		}
	}
	return nil
}

// relink makes every layout link in the release rel the relative one,
// whatever it pointed at (SPEC §2.5). A path that is not a link is left
// alone.
func relink(r *sitefs.Root, rel string) error {
	for _, l := range layoutLinks {
		p := rel + "/" + l.link
		t, err := r.Readlink(p)
		if err != nil || t == l.target {
			continue
		}
		if err := r.Symlink(l.target, p); err != nil {
			return fmt.Errorf("relink %s: %w", l.link, err)
		}
	}
	return nil
}

// cloneDatabase copies production's database into staging's through a
// root-only dump file (SPEC §2.7).
func (o *Ops) cloneDatabase(ctx context.Context, prod, stg state.Site) error {
	if err := os.MkdirAll(o.WorkDir, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(o.WorkDir, "staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	dump := filepath.Join(dir, "production.sql")
	if err := o.DB.DumpFile(ctx, prod.Config.Database.Name, dump); err != nil {
		return fmt.Errorf("dump production's database: %w", err)
	}
	if err := o.DB.ImportFile(ctx, stg.Config.Database.Name, dump); err != nil {
		return fmt.Errorf("import staging's database: %w", err)
	}
	return nil
}

// wpStep is one wp-cli command of the copy's configuration.
type wpStep struct {
	what   string
	secret string
	args   []string
}

// stagingDB requires the copy's database and account to be names the copy
// may take (internal/site ClaimableBy: a lowercase letter first, never
// reserved, never another site's default): anything else never reaches
// wp-cli's argv (SPEC §2.8).
func stagingDB(stg state.Site) error {
	d := stg.Config.Database
	if stg.ID <= 0 || site.ClaimableBy(d.Name, stg.ID) != nil || site.ClaimableBy(d.User, stg.ID) != nil {
		return agentrpc.Invalid("the staging site's database and user must be names of its own")
	}
	return nil
}

// configureWordPress gives the copy its own credentials, token and salts,
// its own address and no indexing (SPEC §2.8).
func (o *Ops) configureWordPress(ctx context.Context, prod, stg state.Site, p rpc.StagingParams) error {
	d := stg.Config.Database
	if err := stagingDB(stg); err != nil {
		return err
	}
	steps := []wpStep{
		{"database name", "", []string{"config", "set", "DB_NAME", d.Name}},
		{"database user", "", []string{"config", "set", "DB_USER", d.User}},
		// Bare --prompt: the positional value of `config set` is read from
		// stdin (--prompt=value is a usage error).
		{"database password", p.DBPassword, []string{"config", "set", "DB_PASSWORD", "--prompt"}},
		{"environment", "", []string{"config", "set", "WP_ENVIRONMENT_TYPE", "staging"}},
		{"indexing", "", []string{"config", "set", "DISALLOW_INDEXING", "true", "--raw"}},
	}
	switch {
	case p.ConnectorToken != "":
		steps = append(steps, wpStep{"connector token", p.ConnectorToken, []string{"config", "set", "CLOUDGROUND_SITE_TOKEN", "--prompt"}})
	case o.wp(ctx, stg, "", "config", "has", "CLOUDGROUND_SITE_TOKEN") == nil:
		// No token of its own: it must not keep production's.
		steps = append(steps, wpStep{"connector token", "", []string{"config", "delete", "CLOUDGROUND_SITE_TOKEN"}})
	}
	for _, s := range steps {
		if err := o.wp(ctx, stg, s.secret, s.args...); err != nil {
			return fmt.Errorf("staging wp-config (%s): %w", s.what, err)
		}
	}
	if err := o.wp(ctx, stg, "", "config", "shuffle-salts"); err != nil {
		return fmt.Errorf("staging salts: %w", err)
	}
	if err := o.rewriteURLs(ctx, stg, prod.Domain, stg.Domain, "--all-tables", "--precise"); err != nil {
		return fmt.Errorf("staging address: %w", err)
	}
	if err := o.wp(ctx, stg, "", "option", "update", "blog_public", "0"); err != nil {
		return fmt.Errorf("staging noindex: %w", err)
	}
	return nil
}
