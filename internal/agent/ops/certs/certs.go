// Package certs serves a site's certificates: Let's Encrypt issuance through
// the ACME adapter, certificates the operator brings, expiry, and the cleanup
// when a site goes. SPEC.md in this directory is the contract.
package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// PanelLineage is certbot's lineage for the panel's own domain.
const PanelLineage = "cloudground-panel"

// Paths are the places this package reads and writes.
type Paths struct {
	CertLiveDir   string // certbot's live/ directory; its parent holds certbot's state
	CustomCertDir string // <dir>/<domain>/{fullchain,privkey}.pem, operator-supplied
	ACMEWebroot   string // served by nginx at /.well-known/acme-challenge/
	PanelCert     string // the certificate nginx serves for the panel domain
}

// Ops holds what the handlers use.
type Ops struct {
	Paths Paths
	ACME  host.ACME
	// Render re-renders a site's configuration and returns the files it
	// wrote. The caller holds the site's lock; Render takes the nginx lock.
	Render func(ctx context.Context, s state.Site) ([]rpc.ManagedFile, error)
	// CertMu is the agent's certbot lock, shared with the panel certificate
	// and site deletion. certbot allows one process against its state.
	CertMu *sync.Mutex
	// HTTP fetches the preflight. Nil: a 12 s client that does not follow
	// redirects.
	HTTP *http.Client
	// LookupHost resolves a name for the preflight's hint. Nil: the system
	// resolver.
	LookupHost func(ctx context.Context, host string) ([]string, error)
	Log        *slog.Logger
	// Now is the clock for expiry checks. Nil: time.Now.
	Now func() time.Time
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.IssueCert, o.IssueCertificate)
	agentrpc.Handle(s, agentrpc.CertStatus, o.CertStatus)
	agentrpc.Handle(s, agentrpc.CustomCert, o.CustomCertificate)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// CheckEmail validates an ACME account address: a bare address that cannot
// be read as an option.
func CheckEmail(email string) error {
	if email == "" {
		return agentrpc.Invalid("acme account email required")
	}
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || strings.HasPrefix(email, "-") {
		return agentrpc.Invalid("invalid acme account email")
	}
	return nil
}

// cloudflareTokenRe is the shape of a Cloudflare API token.
var cloudflareTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{30,80}$`)

// IssueCertificate obtains or renews a site's Let's Encrypt certificate and
// re-renders the site so nginx serves it. The RPC server holds the site's
// lock and has checked the site still exists.
func (o *Ops) IssueCertificate(ctx context.Context, p rpc.CertificateParams) (rpc.ApplyResult, error) {
	s := p.Site
	if err := CheckEmail(p.Email); err != nil {
		return rpc.ApplyResult{}, err
	}
	req := host.CertRequest{Name: s.Domain, Email: p.Email, Staging: p.Staging}
	if p.Wildcard {
		if !cloudflareTokenRe.MatchString(p.CloudflareToken) {
			return rpc.ApplyResult{}, agentrpc.Invalid("a Cloudflare API token is required for a wildcard certificate")
		}
		req.Domains = []string{s.Domain, "*." + s.Domain}
	} else {
		req.Domains = append([]string{s.Domain}, s.Aliases...)
		req.Webroot = o.Paths.ACMEWebroot
		if err := o.Preflight(ctx, req.Domains); err != nil {
			return rpc.ApplyResult{}, err
		}
	}
	if err := o.certbot(ctx, func() error {
		if p.Wildcard {
			creds, err := o.writeCloudflareCredentials(p.CloudflareToken)
			if err != nil {
				return err
			}
			req.CloudflareCredentials = creds
		}
		return o.ACME.CertOnly(ctx, req)
	}); err != nil {
		if errors.Is(err, host.ErrInvalidArgument) {
			return rpc.ApplyResult{}, agentrpc.Wrap(agentrpc.CodeInvalid, "the certificate request names something certbot cannot take", err)
		}
		return rpc.ApplyResult{}, fmt.Errorf("certificate issuance: %w", err)
	}
	files, err := o.Render(ctx, s)
	if err != nil {
		return rpc.ApplyResult{}, err
	}
	return rpc.ApplyResult{Files: files}, nil
}

// certbot runs fn under the certbot lock, and only fn.
func (o *Ops) certbot(ctx context.Context, fn func() error) error {
	o.CertMu.Lock()
	defer o.CertMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// writeCloudflareCredentials writes the file certbot's Cloudflare plugin
// reads, 0600 root, next to certbot's own state. certbot records its path
// in the lineage's renewal configuration and reads it on every renewal. The
// caller holds CertMu.
func (o *Ops) writeCloudflareCredentials(token string) (string, error) {
	path := filepath.Join(filepath.Dir(o.Paths.CertLiveDir), "cloudground-cloudflare.ini")
	if err := rootfile.Replace(path, []byte("dns_cloudflare_api_token = "+token+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// CertStatus reports, for each name, the certificate the server serves
// under it. See SPEC.md §3.
func (o *Ops) CertStatus(ctx context.Context, p rpc.CertStatusParams) (rpc.CertStatusResult, error) {
	res := rpc.CertStatusResult{Certs: map[string]rpc.CertInfo{}}
	for _, name := range p.Names {
		if name != PanelLineage && !functions.ValidLineage(name) && site.ValidateDomain(name) != nil {
			return rpc.CertStatusResult{}, agentrpc.Invalid("invalid certificate name %q", name)
		}
	}
	for _, name := range p.Names {
		path, custom := o.servedCertPath(name)
		info, ok := readCertInfo(path)
		if ok {
			info.Custom = custom
		}
		res.Certs[name] = info
	}
	return res, nil
}

// servedCertPath is the file nginx serves for name, and whether it is one the
// operator brought.
func (o *Ops) servedCertPath(name string) (string, bool) {
	if name == PanelLineage {
		return o.Paths.PanelCert, false
	}
	if o.Paths.CustomCertDir != "" {
		custom := filepath.Join(o.Paths.CustomCertDir, name, "fullchain.pem")
		if fi, err := os.Stat(custom); err == nil && fi.Mode().IsRegular() {
			return custom, true
		}
	}
	return filepath.Join(o.Paths.CertLiveDir, name, "fullchain.pem"), false
}

// readCertInfo parses the first certificate of a PEM file. Anything that is
// not one reads as no certificate.
func readCertInfo(path string) (rpc.CertInfo, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return rpc.CertInfo{}, false
	}
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return rpc.CertInfo{}, false
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return rpc.CertInfo{}, false
		}
		return rpc.CertInfo{Exists: true, NotAfter: cert.NotAfter, Issuer: cert.Issuer.CommonName}, true
	}
}

// CustomCertificate installs or removes the certificate the operator brings
// for a site, then re-renders the site. The RPC server holds the site's lock
// and has checked the site still exists. certbot is not involved, so the
// certbot lock is not taken.
func (o *Ops) CustomCertificate(ctx context.Context, p rpc.CustomCertParams) (rpc.ApplyResult, error) {
	s := p.Site
	if p.Remove {
		if err := o.removeCustom(s.Domain); err != nil {
			return rpc.ApplyResult{}, err
		}
	} else {
		if err := o.checkCustom(s.Domain, p.Fullchain, p.Key); err != nil {
			return rpc.ApplyResult{}, err
		}
		dir := filepath.Join(o.Paths.CustomCertDir, s.Domain)
		if err := rootfile.MkdirMode(dir, 0o700); err != nil {
			return rpc.ApplyResult{}, err
		}
		if err := rootfile.Replace(filepath.Join(dir, "privkey.pem"), []byte(p.Key), 0o600); err != nil {
			return rpc.ApplyResult{}, err
		}
		if err := rootfile.Replace(filepath.Join(dir, "fullchain.pem"), []byte(p.Fullchain), 0o644); err != nil {
			return rpc.ApplyResult{}, err
		}
	}
	files, err := o.Render(ctx, s)
	if err != nil {
		return rpc.ApplyResult{}, err
	}
	return rpc.ApplyResult{Files: files}, nil
}

// checkCustom accepts a certificate that pairs with its key, covers the
// domain and has not expired.
func (o *Ops) checkCustom(domain, fullchain, key string) error {
	pair, err := tls.X509KeyPair([]byte(fullchain), []byte(key))
	if err != nil {
		return agentrpc.Wrap(agentrpc.CodeInvalid, "the certificate and key do not form a pair", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return agentrpc.Wrap(agentrpc.CodeInvalid, "the certificate cannot be read", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return agentrpc.Invalid("the certificate does not cover %s", domain)
	}
	if o.now().After(leaf.NotAfter) {
		return agentrpc.Invalid("the certificate expired on %s", leaf.NotAfter.Format("2006-01-02"))
	}
	return nil
}

func (o *Ops) removeCustom(domain string) error {
	if o.Paths.CustomCertDir == "" {
		return nil
	}
	if err := site.ValidateDomain(domain); err != nil {
		return agentrpc.Invalid("%s", err.Error())
	}
	// The directory is root's; sitefs removes the tree without following a
	// symlink anywhere, the one at the domain's name included.
	d, err := sitefs.Open(o.Paths.CustomCertDir, sitefs.Owner{})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	return d.RemoveAll(domain)
}

// RemoveSite removes what this package keeps for a site being deleted: its
// certbot lineage (so certbot's timer stops renewing a domain that is gone)
// and the certificate the operator brought (key included, which a site
// created later under the same domain would otherwise serve). The caller
// holds the site's lock. A missing lineage is not an error; a failure to
// delete one is logged; a failure to remove the custom certificate is
// returned.
func (o *Ops) RemoveSite(ctx context.Context, domain string) error {
	if err := site.ValidateDomain(domain); err != nil {
		return agentrpc.Invalid("%s", err.Error())
	}
	o.CertMu.Lock()
	err := o.ACME.Delete(ctx, domain)
	o.CertMu.Unlock()
	if err != nil && !errors.Is(err, host.ErrNoSuchCert) {
		o.log().Warn("removing the site's certificate failed", "site", domain, "err", err)
	}
	if err := o.removeCustom(domain); err != nil {
		return fmt.Errorf("remove custom certificate: %w", err)
	}
	return nil
}
