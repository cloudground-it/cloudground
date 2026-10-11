package rpc

import (
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// CertificateParams asks Let's Encrypt for a certificate covering the
// site's domain and aliases (issue or renew).
type CertificateParams struct {
	Site  state.Site `json:"site"`
	Email string     `json:"email"`
	// Staging uses the ACME staging directory, for tests.
	Staging bool `json:"staging"`
	// Wildcard adds *.<domain>, proven with a DNS-01 challenge at
	// Cloudflare through CloudflareToken (a token scoped to Zone:DNS:Edit).
	Wildcard        bool   `json:"wildcard,omitempty"`
	CloudflareToken string `json:"cloudflare_token,omitempty"`
}

// Sites lists the site to certify.
func (p *CertificateParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// CustomCertParams installs a certificate the operator brings (a commercial
// one, a Cloudflare Origin certificate), or with Remove takes it away and
// goes back to Let's Encrypt.
type CustomCertParams struct {
	Site      state.Site `json:"site"`
	Fullchain string     `json:"fullchain"`
	Key       string     `json:"key"`
	Remove    bool       `json:"remove,omitempty"`
}

// Sites lists the site whose certificate changes.
func (p *CustomCertParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// CertStatusParams names the certificate lineages to read: a site's domain,
// or "cloudground-panel" for the panel's own.
type CertStatusParams struct {
	Names []string `json:"names"`
}

// CertStatusResult maps every name asked for to its live certificate.
type CertStatusResult struct {
	Certs map[string]CertInfo `json:"certs"`
}

// CertInfo is one lineage's live certificate.
type CertInfo struct {
	Exists   bool      `json:"exists"`
	NotAfter time.Time `json:"not_after,omitempty"`
	Issuer   string    `json:"issuer,omitempty"`
	// Custom is a certificate the operator brought. It is served in place
	// of Let's Encrypt's, and certbot does not renew it.
	Custom bool `json:"custom,omitempty"`
}

// PanelCertParams asks for a Let's Encrypt certificate for the panel's own
// domain, installed as the certificate nginx serves the panel with.
type PanelCertParams struct {
	Domain string `json:"domain"`
	Email  string `json:"email"`
}
