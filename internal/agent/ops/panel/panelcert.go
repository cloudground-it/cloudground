package panel

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/certs"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// PanelCertificate gives the panel's domain a vhost and a Let's Encrypt
// certificate, and has nginx serve it. See SPEC.md §1.
func (o *Ops) PanelCertificate(ctx context.Context, p rpc.PanelCertParams) (map[string]string, error) {
	if err := site.ValidateDomain(p.Domain); err != nil {
		return nil, agentrpc.Invalid("%s", err.Error())
	}
	if err := certs.CheckEmail(p.Email); err != nil {
		return nil, err
	}
	o.CertMu.Lock()
	defer o.CertMu.Unlock()

	if err := o.installVhost(ctx, p.Domain); err != nil {
		return nil, err
	}
	if err := o.Preflight(ctx, []string{p.Domain}); err != nil {
		return nil, err
	}
	if err := o.ACME.CertOnly(ctx, host.CertRequest{
		Name: certs.PanelLineage, Domains: []string{p.Domain}, Email: p.Email, Webroot: o.Paths.ACMEWebroot,
	}); err != nil {
		return nil, fmt.Errorf("panel certificate issuance: %w", err)
	}

	live := filepath.Join(o.Paths.CertLiveDir, certs.PanelLineage)
	chain, err := os.ReadFile(filepath.Join(live, "fullchain.pem"))
	if err != nil {
		return nil, fmt.Errorf("read the issued panel certificate: %w", err)
	}
	key, err := os.ReadFile(filepath.Join(live, "privkey.pem"))
	if err != nil {
		return nil, fmt.Errorf("read the issued panel key: %w", err)
	}
	if _, err := tls.X509KeyPair(chain, key); err != nil {
		return nil, fmt.Errorf("the issued panel certificate and key do not pair: %w", err)
	}
	// certbot does nothing for a certificate that is not due, and the
	// scheduler asks every 12 hours: nothing to install then.
	if cur, err := rootfile.ReadNoFollow(o.Paths.PanelCert); err == nil && bytes.Equal(cur, chain) {
		return map[string]string{"domain": p.Domain, "status": "unchanged"}, nil
	}
	if err := o.installPanelCert(ctx, chain, key); err != nil {
		return nil, err
	}
	return map[string]string{"domain": p.Domain, "status": "issued"}, nil
}

// installVhost writes the panel vhost and has nginx load it. A vhost nginx
// refuses is taken back out: left in place it would fail every later reload
// on the server.
func (o *Ops) installVhost(ctx context.Context, domain string) error {
	info, err := o.Nginx.BuildInfo(ctx)
	if err != nil {
		o.log().Warn("nginx -V failed; rendering the panel vhost for an older nginx", "err", err)
	}
	vhost, err := nginx.PanelVhost(nginx.PanelInput{
		Domain:         domain,
		ACMEWebroot:    o.Paths.ACMEWebroot,
		CertFile:       o.Paths.PanelCert,
		KeyFile:        o.Paths.PanelKey,
		AccessInclude:  o.Paths.PanelAccess,
		APISocket:      o.Paths.APISocket,
		HTTP2Directive: nginx.HasHTTP2Directive(info),
	})
	if err != nil {
		return err
	}
	path := filepath.Join(o.Paths.NginxSites, nginx.PanelVhostName)

	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	prev, prevErr := rootfile.ReadNoFollow(path)
	if prevErr == nil && bytes.Equal(prev, []byte(vhost)) {
		return nil
	}
	if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
		return prevErr
	}
	restore := func() {
		var err error
		if prevErr == nil {
			err = rootfile.Replace(path, prev, 0o644)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			o.log().Error("could not put the previous panel vhost back", "path", path, "err", err)
		}
	}
	if err := rootfile.Replace(path, []byte(vhost), 0o644); err != nil {
		return err
	}
	if err := o.Nginx.Test(ctx); err != nil {
		restore()
		return fmt.Errorf("nginx refused the panel vhost: %w", err)
	}
	if err := o.Nginx.Reload(ctx); err != nil {
		restore()
		if rerr := o.Nginx.Reload(ctx); rerr != nil {
			o.log().Error("nginx reload after restoring the panel vhost failed", "err", rerr)
		}
		return fmt.Errorf("reload nginx with the panel vhost: %w", err)
	}
	return nil
}

// installPanelCert replaces the panel's key and certificate, root:PanelGroup
// 0640 (the API reads them through its group), and reloads nginx so it
// serves them. If nginx refuses the pair, the previous files come back.
func (o *Ops) installPanelCert(ctx context.Context, chain, key []byte) error {
	gid, err := o.groupID(o.Paths.PanelGroup)
	if err != nil {
		return err
	}
	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	type saved struct {
		path string
		data []byte
		err  error
	}
	var prev []saved
	for _, p := range []string{o.Paths.PanelKey, o.Paths.PanelCert} {
		b, err := rootfile.ReadNoFollow(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		prev = append(prev, saved{p, b, err})
	}
	restore := func() {
		for _, s := range prev {
			var err error
			if s.err == nil {
				err = rootfile.ReplaceOwned(s.path, s.data, 0o640, rootfile.Keep, gid)
			} else {
				err = os.Remove(s.path)
			}
			if err != nil {
				o.log().Error("could not put the previous panel certificate back", "path", s.path, "err", err)
			}
		}
	}
	// Key first: until the certificate follows, nginx still holds the old
	// pair in memory, and nothing reloads it under this lock.
	if err := rootfile.ReplaceOwned(o.Paths.PanelKey, key, 0o640, rootfile.Keep, gid); err != nil {
		return err
	}
	if err := rootfile.ReplaceOwned(o.Paths.PanelCert, chain, 0o640, rootfile.Keep, gid); err != nil {
		restore()
		return err
	}
	if err := o.Nginx.Test(ctx); err != nil {
		restore()
		return fmt.Errorf("nginx refused the new panel certificate: %w", err)
	}
	// nginx reads certificates at reload: until then it serves the old one.
	if err := o.Nginx.Reload(ctx); err != nil {
		return fmt.Errorf("reload nginx with the new panel certificate: %w", err)
	}
	return nil
}
