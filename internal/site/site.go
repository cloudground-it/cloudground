// Package site is the one place a site record from the API is checked and
// confined before root acts on it.
//
// The agent does not trust the API's copy of a site: every field that reaches
// a rendered file, a command line or a path is checked here, on the root side.
// The rules are the ones the agent and the API applied separately before
// (checkSite, validateSite and validateSiteRoot in internal/agent;
// ValidateDomain and NormalizeProxyUpstream in internal/engine/nginx), moved
// here unchanged in what they accept and refuse.
//
// Root confinement is lexical: the root must be SitesRoot/<domain> once
// cleaned. Whether a component of that path is a symlink is not decided here.
// Every file operation on a site tree confines itself at open time: the agent
// opens the root once (refusing a symlink anywhere on it) and resolves every
// path beneath it with internal/agent/sitefs.
package site

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// Error is a validation failure. Error() is safe to show a user; Detail may
// name host paths and is for logs only.
type Error struct {
	Msg    string
	Detail string
}

func (e *Error) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// Valid is a site that passed Validate. Only Validate makes one, so a
// function that takes a Valid needs no checks of its own.
type Valid struct {
	s state.Site
}

// Site returns a copy of the validated record. Its RootPath is exactly
// SitesRoot/<domain>, whatever spelling of that path the API sent.
func (v Valid) Site() state.Site { return v.s }

// Domain is the site's primary domain.
func (v Valid) Domain() string { return v.s.Domain }

// Root is the site's confined root directory.
func (v Valid) Root() string { return v.s.RootPath }

// SystemUser is the site's Unix user (cg-site-<id>).
func (v Valid) SystemUser() string { return v.s.SystemUser }

var (
	domainRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	systemUserRe = regexp.MustCompile(`^cg-site-[0-9]+$`)
	phpVersionRe = regexp.MustCompile(`^8\.[0-9]$`)
	// publicRootRe is a plain relative path inside a release (e.g. "public"
	// for Laravel): no "..", no leading slash, nothing nginx or php.ini would
	// misread.
	publicRootRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	upstreamHostRe = regexp.MustCompile(`^(localhost|[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*)$`)
	upstreamPathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]*)*$`)
)

// Validate checks every field of s that root acts on and confines its root to
// sitesRoot/<domain>. Checks run in a fixed order and the first failure is
// returned.
func Validate(sitesRoot string, s state.Site) (Valid, error) {
	if err := ValidateDomain(s.Domain); err != nil {
		return Valid{}, err
	}
	switch s.Type {
	case state.SiteWordPress, state.SiteWooCommerce, state.SiteStatic, state.SitePHP, state.SiteLaravel, state.SiteProxy:
	default:
		return Valid{}, invalid("invalid site type %q", s.Type)
	}
	switch s.Profile {
	case state.ProfileBalanced, state.ProfileCommerce, state.ProfileMaximum, "":
	default:
		return Valid{}, invalid("invalid profile %q", s.Profile)
	}
	for _, alias := range s.Aliases {
		if err := ValidateDomain(alias); err != nil {
			return Valid{}, invalid("invalid alias: %s", err)
		}
	}
	if strings.ContainsAny(s.RootPath, "\x00\r\n") || strings.ContainsAny(string(s.Engine), "\x00\r\n") {
		return Valid{}, invalid("invalid site record")
	}
	if s.Config.PublicRoot != "" && !publicRootRe.MatchString(s.Config.PublicRoot) {
		return Valid{}, invalid("invalid public root %q", s.Config.PublicRoot)
	}
	if !ValidSystemUser(s.SystemUser) {
		return Valid{}, invalid("invalid system user %q", s.SystemUser)
	}
	if len(s.Config.BypassCookies) > velocity.MaxBypassCookies {
		return Valid{}, invalid("at most %d bypass cookies", velocity.MaxBypassCookies)
	}
	for _, c := range s.Config.BypassCookies {
		if !velocity.ValidBypassCookie(c) {
			return Valid{}, invalid("invalid bypass cookie %q", c)
		}
	}
	if s.PHPVersion != "" && !ValidPHPVersion(s.PHPVersion) {
		return Valid{}, invalid("unsupported php version %q", s.PHPVersion)
	}
	if s.Type == state.SiteProxy {
		if _, err := NormalizeProxyUpstream(s.Config.ProxyUpstream); err != nil {
			return Valid{}, err
		}
	}
	if err := checkDatabase(s); err != nil {
		return Valid{}, err
	}
	if err := CheckRoot(sitesRoot, s); err != nil {
		return Valid{}, err
	}
	s.RootPath = Root(sitesRoot, s.Domain)
	return Valid{s: s}, nil
}

// Root is where a site with this domain lives.
func Root(sitesRoot, domain string) string {
	return filepath.Join(sitesRoot, domain)
}

// CheckRoot is the confinement alone: s.RootPath must be sitesRoot/<domain>
// once cleaned. Teardown uses it on its own, for a site that may be half gone.
func CheckRoot(sitesRoot string, s state.Site) error {
	expected := Root(sitesRoot, s.Domain)
	if filepath.Clean(s.RootPath) != filepath.Clean(expected) {
		return &Error{Msg: "invalid site root", Detail: fmt.Sprintf("invalid site root %q: expected %q", s.RootPath, expected)}
	}
	return nil
}

// ValidateDomain accepts only a plain lowercase host name. Domains end up
// inside rendered configuration files, so this check guards them.
func ValidateDomain(d string) error {
	if len(d) > 253 || !domainRe.MatchString(d) {
		return invalid("invalid domain %q", d)
	}
	return nil
}

// ValidSystemUser reports whether u is a site user name (cg-site-<id>).
func ValidSystemUser(u string) bool { return systemUserRe.MatchString(u) }

// Slice is the systemd slice a site's processes run in (cg-site-<id>.slice,
// named after its user): its PHP master, its cron jobs run on demand and its
// wp-cli runs. Stopping it stops all of them. The dashes make it a child of
// cg-site.slice, so every site sits under one node of the cgroup tree.
func Slice(systemUser string) string { return systemUser + ".slice" }

// ValidPHPVersion reports whether v is a PHP version a site may ask for
// (8.x). Whether it is installed is the agent's question.
func ValidPHPVersion(v string) bool { return phpVersionRe.MatchString(v) }

// ValidPublicRoot reports whether p is a public root a site may declare
// ("" means the release root itself).
func ValidPublicRoot(p string) bool { return p == "" || publicRootRe.MatchString(p) }

// NormalizeProxyUpstream validates a proxy upstream and returns it rebuilt
// from its parts: http or https, a host name or IP literal, an optional
// numeric port and an optional plain path. No credentials, query or fragment,
// and nothing that could close an nginx directive.
func NormalizeProxyUpstream(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return "", invalid("proxy upstream must be an http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", invalid("proxy upstream may not carry credentials, a query or a fragment")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			host = "[" + ip.String() + "]"
		} else {
			host = ip.String()
		}
	} else if len(host) > 253 || !upstreamHostRe.MatchString(host) {
		return "", invalid("proxy upstream host %q is not a valid host name", host)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid("proxy upstream port %q is invalid", p)
		}
		host += ":" + strconv.Itoa(n)
	}
	path := u.EscapedPath()
	if path != "" && !upstreamPathRe.MatchString(path) {
		return "", invalid("proxy upstream path %q has characters nginx would misread", path)
	}
	return u.Scheme + "://" + host + path, nil
}
