package release

import (
	"context"
	"errors"
	"io/fs"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// CurrentRelease reads which release a site serves (SPEC §6): the release
// its current link names, read through sitefs, when that is a release
// directory; "" otherwise. It changes nothing.
func (o *Ops) CurrentRelease(ctx context.Context, p rpc.CurrentReleaseParams) (rpc.CurrentReleaseResult, error) {
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.CurrentReleaseResult{}, err
	}
	defer r.Close()
	rel, err := Live(r)
	if errors.Is(err, fs.ErrNotExist) {
		return rpc.CurrentReleaseResult{}, nil
	}
	if err != nil {
		// A link that leads nowhere a release can be is no release.
		o.log().Warn("the current link names no release", "site", p.Site.Domain, "err", err)
		return rpc.CurrentReleaseResult{}, nil
	}
	id, _ := ParseLink(rel, r.Path())
	if !validName(id) {
		return rpc.CurrentReleaseResult{}, nil
	}
	return rpc.CurrentReleaseResult{ReleaseID: id}, nil
}

// RemoveReleases removes releases the panel knows were never made live
// (SPEC §7): each named one that exists, is a real directory and is not
// the live release. The server holds the site's lock.
func (o *Ops) RemoveReleases(ctx context.Context, p rpc.RemoveReleasesParams) (rpc.RemoveReleasesResult, error) {
	res := rpc.RemoveReleasesResult{Removed: []string{}}
	for _, id := range p.ReleaseIDs {
		if !site.ValidReleaseID(id) {
			return res, agentrpc.Invalid("invalid release id %q", id)
		}
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return res, err
	}
	defer r.Close()
	if err := LockSkeleton(r); err != nil {
		return res, err
	}
	_, cur, err := List(r)
	if err != nil {
		return res, err
	}
	for _, id := range p.ReleaseIDs {
		if id == cur {
			continue
		}
		fi, err := r.Lstat(Dir + "/" + id)
		if err != nil || !fi.IsDir() {
			continue
		}
		if err := asRoot(r).RemoveAll(Dir + "/" + id); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, id)
	}
	return res, nil
}
