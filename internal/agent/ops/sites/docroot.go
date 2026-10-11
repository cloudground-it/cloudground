package sites

// A site's public root must stay inside its release (R-4.3 review). nginx
// is in every site's group, so a public root that is, or passes through, a
// link the site user planted (releases/<n>/web → another tenant's tree)
// would have nginx serve another site's files. Two layers refuse it: this
// check, before every render and when the API validates a change, and the
// vhost's disable_symlinks, which from R-4.3 checks every component below
// the release ("current") instead of exempting the whole document root.

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// CheckDocRoot is the RPC: the public root s would have is refused when a
// component of it below the release is a symlink.
func (o *Ops) CheckDocRoot(_ context.Context, p rpc.SiteRef) (agentrpc.None, error) {
	return agentrpc.None{}, o.checkDocRoot(p.Site)
}

// checkDocRoot Lstats each component of the public root beneath the live
// release, through the site's confined root. A component that does not
// exist yet ends the check: nothing is served from it, and if it later
// appears as a link, nginx refuses it.
func (o *Ops) checkDocRoot(s state.Site) error {
	if s.Config.PublicRoot == "" || o.Open == nil {
		return nil
	}
	root, err := o.Open(s)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	release, err := root.Sub("current")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release.Close()
	var walked []string
	for _, part := range strings.Split(s.Config.PublicRoot, "/") {
		walked = append(walked, part)
		rel := strings.Join(walked, "/")
		fi, err := release.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return agentrpc.Invalid("the public root %q passes through a symbolic link (%s): a site's document root must be a real folder inside its release", s.Config.PublicRoot, rel)
		}
		if !fi.IsDir() {
			return agentrpc.Invalid("the public root %q is not a folder (%s)", s.Config.PublicRoot, rel)
		}
	}
	return nil
}
