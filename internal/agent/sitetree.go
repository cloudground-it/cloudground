package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
)

// Every root-side operation on a site's files goes through a sitefs.Root
// opened on the site's validated root (see sitefs/SPEC.md). This file holds
// the migration of trees made before R-2.3, whose layout links were
// absolute, to the relative layout.

// currentLink is the live release's link: current -> releases/<id>.
const currentLink = "current"

// layoutLinks maps each link inside a release to its relative target.
var layoutLinks = map[string]string{
	wp.ConfigLink:  wp.ConfigTarget,
	wp.UploadsLink: wp.UploadsTarget,
	wp.DropinLink:  wp.DropinTarget,
}

// relinkRelease rewrites the release's layout links that are absolute (the
// layout before R-2.3, or a release cloned from one) as the relative ones.
// An absolute link is never followed by sitefs, and in a staging clone it
// would point at production's shared/.
func relinkRelease(r *sitefs.Root, release string) error {
	for link, target := range layoutLinks {
		p := release + "/" + link
		t, err := r.Readlink(p)
		if err != nil || !path.IsAbs(t) {
			continue
		}
		if err := r.Symlink(target, p); err != nil {
			return fmt.Errorf("relink %s: %w", link, err)
		}
	}
	return nil
}

// relinkSite migrates a tree built with absolute links (siteRoot is the path
// they were made against) to the relative layout. Applying a site's
// configuration does it, so existing sites need no reprovisioning.
func relinkSite(r *sitefs.Root, siteRoot string) error {
	if t, err := r.Readlink(currentLink); err == nil && path.IsAbs(t) {
		if id, ok := release.ParseLink(t, siteRoot); ok {
			root := r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID})
			if err := root.Symlink("releases/"+id, currentLink); err != nil {
				return fmt.Errorf("relink current: %w", err)
			}
		}
	}
	ents, err := r.ReadDir("releases")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() {
			if err := relinkRelease(r, "releases/"+e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}
