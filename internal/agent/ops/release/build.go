package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The links a release gets into the site's shared/ (SPEC §1). Relative, so
// a staging copy's links name the copy's own shared/.
const (
	configLink    = "wp-config.php"
	configTarget  = "../../shared/wp-config.php"
	sharedConfig  = "shared/wp-config.php"
	uploadsLink   = "wp-content/uploads"
	uploadsTarget = "../../../shared/uploads"
)

// Deploy builds a release from a directory of the site's own tree or, for a
// safe push, of the tree of its own staging site (SPEC §2). The copy runs as
// root, so anything else (the agent's token, the panel database, another
// tenant) would be readable back through the file manager. The server has
// validated both sites and holds the target's lock (LockLiveSite).
func (o *Ops) Deploy(ctx context.Context, p rpc.DeployParams) (rpc.DeployResult, error) {
	roots := []string{p.Site.RootPath}
	if p.SourceSite != nil {
		src := *p.SourceSite
		if src.StagingOf == 0 || src.StagingOf != p.Site.ID {
			return rpc.DeployResult{}, agentrpc.Invalid("source site %s is not a staging copy of %s", src.Domain, p.Site.Domain)
		}
		roots = append(roots, src.RootPath)
	}
	return o.Build(ctx, p, roots)
}

// Build makes releases/<p.ReleaseID> from p.SourceDir, which must lie
// inside one of roots. Migration passes its own scratch directory; the
// caller holds the site's lock.
func (o *Ops) Build(ctx context.Context, p rpc.DeployParams, roots []string) (rpc.DeployResult, error) {
	s := p.Site
	if !site.ValidReleaseID(p.ReleaseID) {
		return rpc.DeployResult{}, agentrpc.Invalid("invalid release id %q", p.ReleaseID)
	}
	if len(p.Overlay) > 0 && p.SourceSite == nil {
		return rpc.DeployResult{}, agentrpc.Invalid("a selective push needs the staging site")
	}
	src, srcRel, err := openSource(p.SourceDir, roots)
	if err != nil {
		return rpc.DeployResult{}, err
	}
	defer src.Close()
	r, err := o.Open(s)
	if err != nil {
		return rpc.DeployResult{}, err
	}
	defer r.Close()
	if filepath.Clean(src.Path()) == filepath.Clean(s.RootPath) &&
		(srcRel == "." || srcRel == Dir || strings.HasPrefix(srcRel, Dir+"/"+buildPrefix)) {
		return rpc.DeployResult{}, agentrpc.Invalid("the source directory contains the release being built")
	}
	if err := LockSkeleton(r); err != nil {
		return rpc.DeployResult{}, err
	}
	final := Dir + "/" + p.ReleaseID
	if _, err := r.Lstat(final); err == nil {
		return rpc.DeployResult{}, agentrpc.Conflict("release %s already exists", p.ReleaseID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return rpc.DeployResult{}, err
	}
	if err := clearBuilds(r); err != nil {
		return rpc.DeployResult{}, err
	}

	// The release is built in releases/.build-<id>/<id>: a directory of
	// root's, mode 0700, so the site user cannot reach anything in it while
	// root works there, whoever owns the files. Everything is created as the
	// site user's from the start, and one rename publishes the finished
	// tree. A failure, or a crash, leaves only the build directory, which
	// is not a release and is removed by the next deploy or prune.
	box := Dir + "/" + buildPrefix + p.ReleaseID
	if err := asRoot(r).Mkdir(box, 0o700); err != nil {
		return rpc.DeployResult{}, err
	}
	defer r.RemoveAll(box)
	tree := box + "/" + p.ReleaseID

	// The copy never follows a symlink: a source that is one (rather than a
	// directory reached through the site's own relative links) is refused.
	if err := sitefs.CopyTree(src, srcRel, r, tree); err != nil {
		if errors.Is(err, sitefs.ErrSymlink) || errors.Is(err, sitefs.ErrNotRegular) || errors.Is(err, fs.ErrNotExist) {
			return rpc.DeployResult{}, agentrpc.Wrap(agentrpc.CodeInvalid, "the source directory is not available", err)
		}
		return rpc.DeployResult{}, fmt.Errorf("copy release: %w", err)
	}
	if fi, err := r.Lstat(tree); err != nil || !fi.IsDir() {
		return rpc.DeployResult{}, agentrpc.Invalid("the source is not a directory")
	}
	if err := linkShared(r, tree); err != nil {
		return rpc.DeployResult{}, err
	}

	// A deployed tree is exactly what was uploaded, and a CI build has no
	// CloudGround mu-plugin in it. Promoted like that, the site answers 200
	// and nginx keeps caching it, but nothing would ever purge it again. The object-cache drop-in is a
	// link into shared/ that a new tree does not have either: without it
	// the first deploy quietly turns the object cache off.
	var managed []rpc.ManagedFile
	if s.Type == state.SiteWordPress || s.Type == state.SiteWooCommerce {
		if err := wp.InstallConnector(r, tree); err != nil {
			return rpc.DeployResult{}, fmt.Errorf("install cache connector: %w", err)
		}
		if err := wp.LinkDropin(r, tree, s.Config.ObjectCache); err != nil {
			return rpc.DeployResult{}, fmt.Errorf("link object-cache drop-in: %w", err)
		}
		sum := sha256.Sum256([]byte(connector.PHP))
		managed = append(managed, rpc.ManagedFile{
			Path:   filepath.Join(s.RootPath, CurrentLink, "wp-content", "mu-plugins", "cloudground-connector.php"),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}

	// A selective push: the release is production's own, with only the
	// chosen paths taken from the staging copy.
	if len(p.Overlay) > 0 {
		if err := o.overlay(r, tree, *p.SourceSite, p.Overlay); err != nil {
			return rpc.DeployResult{}, err
		}
	}

	sum, err := scan(ctx, r, tree, o.workers(), true)
	if err != nil {
		return rpc.DeployResult{}, fmt.Errorf("precompress and hash the release: %w", err)
	}
	// The copy keeps the source directory's own mode (a migration work dir
	// is 0700); nginx, through the site group, must traverse the release.
	if err := r.Chmod(tree, 0o750); err != nil {
		return rpc.DeployResult{}, err
	}
	if err := r.Rename(tree, final, sitefs.NoReplace); err != nil {
		return rpc.DeployResult{}, fmt.Errorf("publish release: %w", err)
	}
	if s.Type.IsApp() && o.PrepareApp != nil {
		// As the site user, who cannot reach the build directory (SPEC §3
		// step 8); a failure leaves no release.
		if _, err := o.PrepareApp(ctx, s, filepath.Join(s.RootPath, final)); err != nil {
			if rerr := asRoot(r).RemoveAll(final); rerr != nil {
				o.log().Warn("could not remove a release whose dependencies failed", "release", p.ReleaseID, "err", rerr)
			}
			return rpc.DeployResult{}, err
		}
	}
	return rpc.DeployResult{ReleaseID: p.ReleaseID, Path: filepath.Join(s.RootPath, final), Checksum: sum.checksum, Managed: managed}, nil
}

// linkShared points the release's persistent paths at the site's shared/:
// uploads when it has wp-content, and wp-config.php whenever the site has
// one. Configuration always comes from THIS site's shared/, never from the
// deployed tree: a release cloned from staging must not carry staging
// credentials into production.
func linkShared(r *sitefs.Root, tree string) error {
	if fi, err := r.Lstat(tree + "/wp-content"); err == nil && fi.IsDir() {
		if err := r.RemoveAll(tree + "/" + uploadsLink); err != nil {
			return err
		}
		if err := r.Symlink(uploadsTarget, tree+"/"+uploadsLink); err != nil {
			return err
		}
	}
	if _, err := r.Lstat(sharedConfig); err == nil {
		if err := r.RemoveAll(tree + "/" + configLink); err != nil {
			return err
		}
		if err := r.Symlink(configTarget, tree+"/"+configLink); err != nil {
			return err
		}
	}
	return nil
}

// openSource opens the one of roots that dir lies in, and returns dir
// relative to it with every symlink on it followed beneath that root: a
// deploy from a site's "current" copies the release current points at.
func openSource(dir string, roots []string) (*sitefs.Root, string, error) {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return nil, "", agentrpc.Invalid("the source directory must be an absolute path")
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, clean)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		src, err := sitefs.Open(root, sitefs.Owner{})
		if err != nil {
			return nil, "", agentrpc.Wrap(agentrpc.CodeInvalid, "the source directory is not available", err)
		}
		resolved, err := src.Resolve(rel)
		if err != nil {
			src.Close()
			return nil, "", agentrpc.Wrap(agentrpc.CodeInvalid, "the source directory is not available", err)
		}
		return src, resolved, nil
	}
	return nil, "", agentrpc.Invalid("the source directory is outside the site's own tree")
}

// overlayPathRe is a plain relative path inside a release.
var overlayPathRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*$`)

// overlay replaces each of paths in the release tree (in r) with the same
// path from the staging site's live release. Both sides are confined to
// their release: a link on staging that leads out of it, or a component of
// the new release that does, is refused by the kernel.
func (o *Ops) overlay(r *sitefs.Root, tree string, stg state.Site, paths []string) error {
	sr, err := o.Open(stg)
	if err != nil {
		return err
	}
	defer sr.Close()
	cur, err := Live(sr)
	if err != nil {
		return err
	}
	from, err := sr.Sub(cur)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := r.Sub(tree)
	if err != nil {
		return err
	}
	defer to.Close()
	for _, p := range paths {
		p = strings.Trim(p, "/")
		if !overlayPathRe.MatchString(p) || strings.Contains("/"+p+"/", "/../") ||
			strings.HasPrefix(p, uploadsLink) || p == configLink {
			return agentrpc.Invalid("cannot push %q selectively", p)
		}
		resolved, err := from.Resolve(p)
		if errors.Is(err, sitefs.ErrEscape) {
			return agentrpc.Invalid("%s on staging points outside its release", p)
		}
		if err != nil {
			return err
		}
		if _, err := from.Lstat(resolved); err != nil {
			return agentrpc.Invalid("%s does not exist on staging", p)
		}
		if err := to.MkdirAll(path.Dir(p), 0o755); err != nil {
			return fmt.Errorf("prepare %s in the release: %w", p, confined(err))
		}
		if err := to.RemoveAll(p); err != nil {
			return confined(err)
		}
		if err := sitefs.CopyTree(from, resolved, to, p); err != nil {
			return fmt.Errorf("copy %s from staging: %w", p, confined(err))
		}
	}
	return nil
}

// confined turns a confinement refusal into an error the panel can show.
func confined(err error) error {
	switch {
	case errors.Is(err, sitefs.ErrEscape):
		return agentrpc.Invalid("path escapes the site root")
	case errors.Is(err, sitefs.ErrSymlink):
		return agentrpc.Invalid("the path is a symlink; refusing to follow it")
	}
	return err
}
