package sites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Apply re-renders a site from the API's desired state (SPEC §7). The
// server holds the site's lock and has checked that the site exists.
func (o *Ops) Apply(ctx context.Context, p rpc.SiteRef) (rpc.ApplyResult, error) {
	if err := o.prepareTree(p.Site); err != nil {
		return rpc.ApplyResult{}, err
	}
	// The render reports the connector of a WordPress site among its files.
	files, err := o.render(ctx, p.Site, p.ReloadOnlyIfChanged)
	if err != nil {
		return rpc.ApplyResult{}, err
	}
	return rpc.ApplyResult{Files: files}, nil
}

// prepareTree relinks a tree with absolute layout links and, for
// WordPress, rewrites the live release's connector with the embedded one:
// a panel upgrade brings connector fixes to the sites it already has.
func (o *Ops) prepareTree(s state.Site) error {
	r, err := o.Open(s)
	if err != nil {
		return err
	}
	defer r.Close()
	if o.Relink != nil {
		if err := o.Relink(r, s.RootPath); err != nil {
			return fmt.Errorf("relink the site layout: %w", err)
		}
	}
	if !isWordPress(s) {
		return nil
	}
	if _, err := release.Live(r); err != nil {
		return nil // no release yet: nothing to refresh
	}
	err = r.Replace(path.Join(liveLink, wp.ConnectorPlugin), 0o644, func(f *os.File) error {
		_, err := io.WriteString(f, connector.PHP)
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil // no mu-plugins directory: not a release the panel built
	}
	if err != nil {
		return fmt.Errorf("refresh the cache connector: %w", err)
	}
	return nil
}
