package sites

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/cfips"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Cloudflare's files (#22, SPEC §10): the ranges nginx takes
// CF-Connecting-IP from, and the server's block list as a geo, both shared
// by every site behind Cloudflare. They belong to no site, so they are
// never among a render's managed files: SetBanned changes the list on its
// own, and drift would call that a change behind the panel's back.

func (o *Ops) cloudflareFiles() (include, banned string) {
	r := nginx.Renderer{ConfDir: o.Paths.NginxConfDir}
	return r.CloudflareIncludeFile(), r.BannedFile()
}

// ensureCloudflare writes the files a site behind Cloudflare needs when
// they are missing: the shipped ranges, and nothing banned (the block list
// fills it at its next apply). The caller holds NginxMu.
func (o *Ops) ensureCloudflare() error {
	include, banned := o.cloudflareFiles()
	for path, content := range map[string]string{
		include: nginx.CloudflareInclude(cfips.Shipped()),
		banned:  nginx.Banned(nil, nil),
	} {
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := writeFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SetCloudflareRanges replaces the ranges nginx trusts CF-Connecting-IP
// from (SPEC §10).
func (o *Ops) SetCloudflareRanges(ctx context.Context, p rpc.CloudflareRangesParams) (rpc.CloudflareRangesResult, error) {
	r, err := cfips.Parse(p.V4, p.V6)
	if err != nil {
		return rpc.CloudflareRangesResult{}, agentrpc.Invalid("%s", err.Error())
	}
	include, _ := o.cloudflareFiles()
	changed, err := o.replaceShared(ctx, include, nginx.CloudflareInclude(r), false)
	return rpc.CloudflareRangesResult{Changed: changed}, err
}

// SetBanned gives the sites behind Cloudflare the server's block list
// (SPEC §10): only once one of them made the file, so a server without
// any never reloads nginx for a ban.
func (o *Ops) SetBanned(ctx context.Context, block, allow []netip.Prefix) error {
	_, banned := o.cloudflareFiles()
	_, err := o.replaceShared(ctx, banned, nginx.Banned(block, allow), true)
	return err
}

// replaceShared writes one of the shared files when its content differs,
// then tests and reloads nginx; a failure puts the old file back. With
// onlyIfPresent, a missing file is left missing.
func (o *Ops) replaceShared(ctx context.Context, path, content string, onlyIfPresent bool) (bool, error) {
	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if onlyIfPresent {
			return false, nil
		}
	case err != nil:
		return false, err
	case string(old) == content:
		return false, nil
	}
	tx := &configTx{prior: map[string]*priorFile{}}
	if _, err := tx.write(path, content, 0o644); err != nil {
		return false, errors.Join(err, tx.restore())
	}
	if err := o.reloadNginx(ctx); err != nil {
		rctx := context.WithoutCancel(ctx)
		if rerr := errors.Join(tx.restore(), o.reloadNginx(rctx)); rerr != nil {
			return false, fmt.Errorf("%w; putting %s back also failed: %v", err, path, rerr)
		}
		return false, err
	}
	return true, nil
}
