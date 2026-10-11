package host

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrNoSuchCert is a certbot lineage that does not exist.
var ErrNoSuchCert = errors.New("no such certificate")

// certNameRe is a lineage name: a domain, as certbot names a site's lineage,
// or a single label such as the panel's own "cloudground-panel". Labels are
// lowercase letters, digits and inner hyphens, so a name can neither start
// with "-" nor hold a path separator or "..".
var certNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$`)

// domainRe also admits a leading "*." for wildcard names.
var domainRe = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)+$`)

// CertRequest is one `certbot certonly`.
type CertRequest struct {
	Name    string   // lineage (--cert-name)
	Domains []string // -d, in order; the first is usually Name
	Email   string
	// Webroot selects HTTP-01 through this directory. Empty with
	// CloudflareCredentials set selects DNS-01 through Cloudflare.
	Webroot               string
	CloudflareCredentials string
	PropagationSeconds    int // DNS-01 only; 0 means 30
	Staging               bool
}

// ACME issues and removes certificates.
type ACME interface {
	CertOnly(ctx context.Context, r CertRequest) error
	Delete(ctx context.Context, name string) error
}

// Certbot is the real ACME.
type Certbot struct {
	Exec Exec
	// ConfigDir is certbot's configuration directory; empty means
	// /etc/letsencrypt.
	ConfigDir string
}

// NewCertbot returns the real adapter.
func NewCertbot(e Exec) *Certbot { return &Certbot{Exec: e} }

func (c *Certbot) configDir() string {
	if c.ConfigDir != "" {
		return c.ConfigDir
	}
	return "/etc/letsencrypt"
}

// CertOnlyArgs is the certbot argv for r, after validation.
func CertOnlyArgs(r CertRequest) ([]string, error) {
	if !certNameRe.MatchString(r.Name) {
		return nil, fmt.Errorf("certificate name %q: %w", r.Name, ErrInvalidArgument)
	}
	if len(r.Domains) == 0 {
		return nil, fmt.Errorf("no domains: %w", ErrInvalidArgument)
	}
	for _, d := range r.Domains {
		if !domainRe.MatchString(d) {
			return nil, fmt.Errorf("domain %q: %w", d, ErrInvalidArgument)
		}
	}
	if _, err := mail.ParseAddress(r.Email); err != nil || strings.HasPrefix(r.Email, "-") || strings.ContainsAny(r.Email, " <>") {
		return nil, fmt.Errorf("email %q: %w", r.Email, ErrInvalidArgument)
	}
	args := []string{"certonly"}
	switch {
	case r.Webroot != "" && r.CloudflareCredentials == "":
		if !filepath.IsAbs(r.Webroot) {
			return nil, fmt.Errorf("webroot %q: %w", r.Webroot, ErrInvalidArgument)
		}
		args = append(args, "--webroot", "--webroot-path", r.Webroot)
	case r.Webroot == "" && r.CloudflareCredentials != "":
		if !filepath.IsAbs(r.CloudflareCredentials) {
			return nil, fmt.Errorf("credentials %q: %w", r.CloudflareCredentials, ErrInvalidArgument)
		}
		secs := r.PropagationSeconds
		if secs <= 0 {
			secs = 30
		}
		args = append(args, "--dns-cloudflare", "--dns-cloudflare-credentials", r.CloudflareCredentials,
			"--dns-cloudflare-propagation-seconds", fmt.Sprint(secs))
	default:
		return nil, fmt.Errorf("exactly one of webroot or Cloudflare credentials: %w", ErrInvalidArgument)
	}
	args = append(args, "--non-interactive", "--agree-tos", "--email", r.Email, "--cert-name", r.Name)
	for _, d := range r.Domains {
		args = append(args, "-d", d)
	}
	if r.Staging {
		args = append(args, "--staging")
	}
	return args, nil
}

func (c *Certbot) CertOnly(ctx context.Context, r CertRequest) error {
	args, err := CertOnlyArgs(r)
	if err != nil {
		return err
	}
	_, err = c.Exec.Run(ctx, Cmd{Name: "certbot", Args: args})
	return err
}

// Delete removes a lineage. One without a renewal configuration does not
// exist as far as certbot is concerned: ErrNoSuchCert, and certbot is not
// run (it would exit 1, the same code as for every other failure).
func (c *Certbot) Delete(ctx context.Context, name string) error {
	if !certNameRe.MatchString(name) {
		return fmt.Errorf("certificate name %q: %w", name, ErrInvalidArgument)
	}
	if _, err := os.Lstat(filepath.Join(c.configDir(), "renewal", name+".conf")); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("certbot delete %s: %w", name, ErrNoSuchCert)
	} else if err != nil {
		return err
	}
	_, err := c.Exec.Run(ctx, Cmd{Name: "certbot", Args: []string{"delete", "--cert-name", name, "--non-interactive"}})
	return err
}

var _ ACME = (*Certbot)(nil)
