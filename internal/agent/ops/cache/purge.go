// Package cache serves the agent's page-cache methods.
package cache

import (
	"context"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// Purger empties entries of a site's page cache under Root.
type Purger struct {
	Root string // /var/cache/cloudground
}

// Purge removes the cache entries of the given URLs, on the site's domain
// and its aliases, or the whole site's cache when no URL is given. The site
// was validated by the RPC server before this runs.
func (c Purger) Purge(_ context.Context, p rpc.PurgeParams) (rpc.PurgeResult, error) {
	dir := filepath.Join(c.Root, velocity.SanitizeCacheDirName(p.Site.Domain))
	var n int
	var err error
	if len(p.URLs) == 0 {
		n, err = velocity.PurgeAll(dir)
	} else {
		n, err = velocity.PurgeURLs(dir, p.URLs, append([]string{p.Site.Domain}, p.Site.Aliases...)...)
	}
	if err != nil {
		return rpc.PurgeResult{}, err
	}
	return rpc.PurgeResult{Removed: n}, nil
}
