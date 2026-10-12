package site

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

const root = "/srv/sites"

func good() state.Site {
	return state.Site{
		ID: 7, Domain: "example.com", Type: state.SiteWordPress, Profile: state.ProfileBalanced,
		SystemUser: "cg-site-7", RootPath: "/srv/sites/example.com", PHPVersion: "8.4",
		Engine: state.EngineNginx,
	}
}

func with(f func(*state.Site)) state.Site {
	s := good()
	f(&s)
	return s
}

// The cases pin what the agent accepted and refused before the rules moved
// here. want is "" for accept, else the exact message a user sees.
var cases = []struct {
	name string
	site state.Site
	want string
}{
	{"a plain WordPress site", good(), ""},
	{"every site type", with(func(s *state.Site) { s.Type = state.SiteLaravel }), ""},
	{"static", with(func(s *state.Site) { s.Type = state.SiteStatic; s.PHPVersion = "" }), ""},
	{"woocommerce", with(func(s *state.Site) { s.Type = state.SiteWooCommerce }), ""},
	{"php", with(func(s *state.Site) { s.Type = state.SitePHP }), ""},
	{"empty profile", with(func(s *state.Site) { s.Profile = "" }), ""},
	{"commerce profile", with(func(s *state.Site) { s.Profile = state.ProfileCommerce }), ""},
	{"maximum profile", with(func(s *state.Site) { s.Profile = state.ProfileMaximum }), ""},
	{"aliases", with(func(s *state.Site) { s.Aliases = []string{"www.example.com", "a-b.example.org"} }), ""},
	{"public root", with(func(s *state.Site) { s.Config.PublicRoot = "public/web_1" }), ""},
	{"root with a trailing slash", with(func(s *state.Site) { s.RootPath = "/srv/sites/example.com/" }), ""},
	{"root through dot-dot that cleans to itself", with(func(s *state.Site) { s.RootPath = "/srv/sites/../sites/example.com" }), ""},
	{"long label", with(func(s *state.Site) { s.Domain = strings.Repeat("a", 63) + ".com"; s.RootPath = root + "/" + s.Domain }), ""},

	{"empty domain", with(func(s *state.Site) { s.Domain = "" }), `invalid domain ""`},
	{"uppercase domain", with(func(s *state.Site) { s.Domain = "Example.com" }), `invalid domain "Example.com"`},
	{"single label", with(func(s *state.Site) { s.Domain = "localhost" }), `invalid domain "localhost"`},
	{"trailing dot", with(func(s *state.Site) { s.Domain = "example.com." }), `invalid domain "example.com."`},
	{"leading hyphen", with(func(s *state.Site) { s.Domain = "-a.com" }), `invalid domain "-a.com"`},
	{"underscore", with(func(s *state.Site) { s.Domain = "a_b.com" }), `invalid domain "a_b.com"`},
	{"wildcard", with(func(s *state.Site) { s.Domain = "*.example.com" }), `invalid domain "*.example.com"`},
	{"newline", with(func(s *state.Site) { s.Domain = "a.com\nb" }), `invalid domain "a.com\nb"`},
	{"path in domain", with(func(s *state.Site) { s.Domain = "../etc" }), `invalid domain "../etc"`},
	{"too long", with(func(s *state.Site) { s.Domain = strings.Repeat("a.", 127) + "com" }), fmt.Sprintf("invalid domain %q", strings.Repeat("a.", 127)+"com")},
	{"unknown type", with(func(s *state.Site) { s.Type = "node" }), `invalid site type "node"`},
	{"empty type", with(func(s *state.Site) { s.Type = "" }), `invalid site type ""`},
	{"unknown profile", with(func(s *state.Site) { s.Profile = "turbo" }), `invalid profile "turbo"`},
	{"bad alias", with(func(s *state.Site) { s.Aliases = []string{"www.example.com", "bad alias"} }), `invalid alias: invalid domain "bad alias"`},
	{"NUL in root", with(func(s *state.Site) { s.RootPath = "/srv/sites/example.com\x00" }), "invalid site record"},
	{"CR in engine", with(func(s *state.Site) { s.Engine = "nginx\r" }), "invalid site record"},
	{"public root with dot-dot", with(func(s *state.Site) { s.Config.PublicRoot = "../x" }), `invalid public root "../x"`},
	{"absolute public root", with(func(s *state.Site) { s.Config.PublicRoot = "/public" }), `invalid public root "/public"`},
	{"public root trailing slash", with(func(s *state.Site) { s.Config.PublicRoot = "public/" }), `invalid public root "public/"`},
	{"root as system user", with(func(s *state.Site) { s.SystemUser = "root" }), `invalid system user "root"`},
	{"empty system user", with(func(s *state.Site) { s.SystemUser = "" }), `invalid system user ""`},
	{"system user with suffix", with(func(s *state.Site) { s.SystemUser = "cg-site-7x" }), `invalid system user "cg-site-7x"`},
	{"php 7.4", with(func(s *state.Site) { s.PHPVersion = "7.4" }), `unsupported php version "7.4"`},
	{"php 8.10", with(func(s *state.Site) { s.PHPVersion = "8.10" }), `unsupported php version "8.10"`},
	{"empty root", with(func(s *state.Site) { s.RootPath = "" }), "invalid site root"},
	{"another site's root", with(func(s *state.Site) { s.RootPath = "/srv/sites/other.com" }), "invalid site root"},
	{"root outside SitesRoot", with(func(s *state.Site) { s.RootPath = "/etc" }), "invalid site root"},
	{"root below the site", with(func(s *state.Site) { s.RootPath = "/srv/sites/example.com/current" }), "invalid site root"},
	{"relative root", with(func(s *state.Site) { s.RootPath = "srv/sites/example.com" }), "invalid site root"},
	{"SitesRoot itself", with(func(s *state.Site) { s.RootPath = "/srv/sites" }), "invalid site root"},

	// A proxy site's upstream is checked like the renderer and the API check
	// it (the API stores only normalized upstreams, so every record it writes
	// passes).
	{"proxy", with(func(s *state.Site) {
		s.Type, s.PHPVersion, s.Config.ProxyUpstream = state.SiteProxy, "", "http://127.0.0.1:3000"
	}), ""},
	{"proxy with a path", with(func(s *state.Site) {
		s.Type, s.PHPVersion, s.Config.ProxyUpstream = state.SiteProxy, "", "https://app.internal/api"
	}), ""},
	{"proxy without upstream", with(func(s *state.Site) { s.Type, s.PHPVersion = state.SiteProxy, "" }), "proxy upstream must be an http(s) URL"},
	{"proxy upstream closing the directive", with(func(s *state.Site) {
		s.Type, s.Config.ProxyUpstream = state.SiteProxy, "http://a;}"
	}), "proxy upstream must be an http(s) URL"},
	{"proxy upstream with an underscore host", with(func(s *state.Site) {
		s.Type, s.Config.ProxyUpstream = state.SiteProxy, "http://a_b"
	}), `proxy upstream host "a_b" is not a valid host name`},
	{"upstream ignored for other types", with(func(s *state.Site) { s.Config.ProxyUpstream = "nonsense" }), ""},

	// First failure wins, in the old order: domain before type before user.
	{"order: domain first", with(func(s *state.Site) { s.Domain, s.Type, s.SystemUser = "X", "x", "x" }), `invalid domain "X"`},
	{"order: type before user", with(func(s *state.Site) { s.Type, s.SystemUser = "x", "x" }), `invalid site type "x"`},
}

func TestValidate(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := Validate(root, c.site)
			if c.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if v.Root() != filepath.Join(root, c.site.Domain) {
					t.Fatalf("root %q is not the canonical %q", v.Root(), filepath.Join(root, c.site.Domain))
				}
				if v.Domain() != c.site.Domain || v.SystemUser() != c.site.SystemUser {
					t.Fatal("validated value differs from the input")
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, want %q", c.want)
			}
			if err.Error() != c.want {
				t.Fatalf("message %q, want %q", err.Error(), c.want)
			}
			var se *Error
			if !errors.As(err, &se) {
				t.Fatalf("not a *site.Error: %T", err)
			}
			if strings.Contains(se.Msg, "/srv") {
				t.Fatalf("user message names a host path: %q", se.Msg)
			}
		})
	}
}

// The detail of a root refusal keeps what the agent used to say.
func TestRootDetail(t *testing.T) {
	err := CheckRoot(root, with(func(s *state.Site) { s.RootPath = "/etc" }))
	var se *Error
	if !errors.As(err, &se) || se.Detail != `invalid site root "/etc": expected "/srv/sites/example.com"` {
		t.Fatalf("detail: %#v", err)
	}
}

// Confinement is lexical, as it was: a root that is a symlink (to anywhere)
// still validates, and so does a SitesRoot reached through a symlink. What
// keeps root inside the tree is the open-time confinement of every file
// operation, not this check. Pinned so a change here is deliberate.
func TestSymlinkedRootsArePinnedLexical(t *testing.T) {
	dir := t.TempDir()
	sites := filepath.Join(dir, "sites")
	if err := os.Mkdir(sites, 0o755); err != nil {
		t.Fatal(err)
	}
	// The site root is a symlink to a directory outside SitesRoot.
	if err := os.Symlink("/etc", filepath.Join(sites, "example.com")); err != nil {
		t.Fatal(err)
	}
	s := good()
	s.RootPath = filepath.Join(sites, "example.com")
	v, err := Validate(sites, s)
	if err != nil {
		t.Fatalf("a symlinked root was refused (the check is lexical): %v", err)
	}
	if v.Root() != filepath.Join(sites, "example.com") {
		t.Fatalf("the root was resolved through the link: %q", v.Root())
	}
	// SitesRoot itself through a symlink: the root must be spelled under the
	// configured SitesRoot, not under the link's target.
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(sites, link); err != nil {
		t.Fatal(err)
	}
	s.RootPath = filepath.Join(link, "example.com")
	if _, err := Validate(sites, s); err == nil || err.Error() != "invalid site root" {
		t.Fatalf("a root spelled through another path was accepted: %v", err)
	}
	if _, err := Validate(link, s); err != nil {
		t.Fatalf("a SitesRoot that is itself a link: %v", err)
	}
}

// legacyCheckSite is internal/agent's checkSite as it was before this
// package (validateSite + validateSiteRoot, nginx.ValidateDomain inlined),
// kept verbatim to prove the port accepts and refuses the same records.
func legacyCheckSite(sitesRoot string, site state.Site) error {
	domainRe := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	validateDomain := func(d string) error {
		if len(d) > 253 || !domainRe.MatchString(d) {
			return fmt.Errorf("invalid domain %q", d)
		}
		return nil
	}
	systemUserRe := regexp.MustCompile(`^cg-site-[0-9]+$`)
	phpVersionRe := regexp.MustCompile(`^8\.[0-9]$`)
	publicRootRe := regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	if err := validateDomain(site.Domain); err != nil {
		return err
	}
	switch site.Type {
	case state.SiteWordPress, state.SiteWooCommerce, state.SiteStatic, state.SitePHP, state.SiteLaravel, state.SiteProxy:
	default:
		return fmt.Errorf("invalid site type %q", site.Type)
	}
	switch site.Profile {
	case state.ProfileBalanced, state.ProfileCommerce, state.ProfileMaximum, "":
	default:
		return fmt.Errorf("invalid profile %q", site.Profile)
	}
	for _, alias := range site.Aliases {
		if err := validateDomain(alias); err != nil {
			return fmt.Errorf("invalid alias: %w", err)
		}
	}
	if strings.ContainsAny(site.RootPath, "\x00\r\n") || strings.ContainsAny(string(site.Engine), "\x00\r\n") {
		return fmt.Errorf("invalid site record")
	}
	if site.Config.PublicRoot != "" && !publicRootRe.MatchString(site.Config.PublicRoot) {
		return fmt.Errorf("invalid public root %q", site.Config.PublicRoot)
	}
	if !systemUserRe.MatchString(site.SystemUser) {
		return fmt.Errorf("invalid system user %q", site.SystemUser)
	}
	if site.PHPVersion != "" && !phpVersionRe.MatchString(site.PHPVersion) {
		return fmt.Errorf("unsupported php version %q", site.PHPVersion)
	}
	expected := filepath.Join(sitesRoot, site.Domain)
	if filepath.Clean(site.RootPath) != filepath.Clean(expected) {
		return fmt.Errorf("invalid site root %q: expected %q", site.RootPath, expected)
	}
	return nil
}

// sameVerdict compares the new validator with the old one. The only
// intended difference is the proxy upstream check, which the old agent left
// to the renderer.
func sameVerdict(t *testing.T, s state.Site) {
	t.Helper()
	_, got := Validate(root, s)
	old := legacyCheckSite(root, s)
	if s.Type == state.SiteProxy {
		if _, uerr := NormalizeProxyUpstream(s.Config.ProxyUpstream); uerr != nil {
			if old == nil && got == nil {
				t.Fatalf("a proxy site with an invalid upstream was accepted: %+v", s)
			}
			return
		}
	}
	if (got == nil) != (old == nil) {
		t.Fatalf("verdicts differ for %+v: new %v, old %v", s, got, old)
	}
	if got != nil && old != nil && !strings.HasPrefix(old.Error(), got.Error()) {
		t.Fatalf("messages differ for %+v: new %q, old %q", s, got, old)
	}
}

func TestMatchesLegacyOnTheTable(t *testing.T) {
	for _, c := range cases {
		sameVerdict(t, c.site)
	}
}

func FuzzMatchesLegacy(f *testing.F) {
	for _, c := range cases {
		s := c.site
		alias := ""
		if len(s.Aliases) > 0 {
			alias = s.Aliases[len(s.Aliases)-1]
		}
		f.Add(s.Domain, string(s.Type), string(s.Profile), alias, s.RootPath, s.Config.PublicRoot, s.SystemUser, s.PHPVersion, string(s.Engine), s.Config.ProxyUpstream)
	}
	f.Fuzz(func(t *testing.T, domain, typ, profile, alias, rootPath, publicRoot, user, php, engine, upstream string) {
		s := state.Site{Domain: domain, Type: state.SiteType(typ), Profile: state.Profile(profile), RootPath: rootPath,
			SystemUser: user, PHPVersion: php, Engine: state.Engine(engine)}
		if alias != "" {
			s.Aliases = []string{alias}
		}
		s.Config.PublicRoot, s.Config.ProxyUpstream = publicRoot, upstream
		sameVerdict(t, s)
	})
}

// #26: the paths a site may protect are pasted into nginx's configuration.
func TestValidProtectedPath(t *testing.T) {
	for _, p := range []string{"/wp-admin", "/private/files", "/a.b~c_d-e", "/x"} {
		if !ValidProtectedPath(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range []string{"", "/", "wp-admin", "/wp-admin/", "/a//b", "/a/../b", "/./a", "/a b", "/a;b", "/a{1}", `/a"b`, "/a$b", "/a\nb", "/" + strings.Repeat("a", 200)} {
		if ValidProtectedPath(p) {
			t.Errorf("%q accepted", p)
		}
	}
	for u, want := range map[string]bool{"admin": true, "a.b@c-d_e": true, "": false, "a:b": false, "a b": false, strings.Repeat("u", 65): false} {
		if ValidBasicAuthUser(u) != want {
			t.Errorf("ValidBasicAuthUser(%q) = %v", u, !want)
		}
	}
}

// #75: the login path and the XML-RPC exceptions are pasted into nginx's
// configuration and PHP's environment; only what renders as it is passes.
func TestHardeningInput(t *testing.T) {
	for p, ok := range map[string]bool{"my-door": true, "a1b": true, "ab": false, "My-Door": false, "-x-y": false,
		"a/b": false, "wp-admin": false, "login": false, "my-account": false, "x y": false, "door;": false} {
		if ValidLoginPath(p) != ok {
			t.Errorf("ValidLoginPath(%q) = %v", p, !ok)
		}
	}
	for in, want := range map[string]string{"192.0.2.1": "192.0.2.1", "192.0.2.7/24": "192.0.2.0/24", "2001:db8::1": "2001:db8::1",
		"::ffff:192.0.2.1": "192.0.2.1", "2001:db8::/32": "2001:db8::/32", "x": "", "192.0.2.1;": "", "fe80::1%eth0": "", "": ""} {
		got, err := NormalizeXMLRPCAllow(in)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("NormalizeXMLRPCAllow(%q) = %q, %v", in, got, err)
		}
	}
	s := state.Site{}
	s.Config.WPHardening = state.WPHardening{XMLRPCAllow: []string{"192.0.2.7/24"}}
	if checkHardening(s) == nil {
		t.Error("a network not in its canonical form passed")
	}
	s.Config.WPHardening = state.WPHardening{XMLRPCAllow: []string{"192.0.2.0/24"}, LoginPath: "my-door"}
	if err := checkHardening(s); err != nil {
		t.Error(err)
	}
}
