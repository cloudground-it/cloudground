package nginx

import (
	"flag"
	"fmt"
	"github.com/cloudground-it/cloudground/internal/functions"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/cfips"
	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// The render matrix (SPEC.md §6): every site type × profile × cache switch ×
// nginx build, plus every option the templates branch on, rendered and
// compared byte for byte with testdata/matrix. The goldens were taken from
// the templates before the Phase 5 rewrite (R-5.1); a difference is a change
// to the request path and must be deliberate (regenerate with
// -update-matrix and say why in the commit).
//
// CG_NGINX_DUMP=<dir> also writes every case's files under
// <dir>/<build>/{sites-enabled,conf.d}, for scripts/nginx-matrix-test.sh to
// run `nginx -t` on them in a distro container.

var updateMatrix = flag.Bool("update-matrix", false, "rewrite testdata/matrix")

const matrixDir = "testdata/matrix"

// matrixBuild is what the agent reads from `nginx -V`.
type matrixBuild struct {
	name           string
	http2Directive bool
	http3          bool
}

var matrixBuilds = []matrixBuild{
	{"nginx124", false, false}, // Ubuntu 24.04: listen … http2, no QUIC
	{"nginx126", true, false},  // >= 1.25.1 built without the v3 module
	{"nginx128", true, true},   // Ubuntu 26.04: http2 on, QUIC
}

type matrixCase struct {
	name  string
	build matrixBuild
	in    engine.Input
}

// matrixRoot is where the dumped renders expect their trees; the nginx
// script creates it.
const matrixRoot = "/srv/cgt"

func matrixInput(id int64, typ state.SiteType, prof state.Profile, cache bool) engine.Input {
	// Short names: the distro's server_names_hash_bucket_size is 64.
	domain := fmt.Sprintf("c%d.example.test", id)
	s := state.Site{
		ID: id, Domain: domain, Type: typ, Profile: prof, Engine: state.EngineNginx,
		SystemUser: fmt.Sprintf("cg-site-%d", id), RootPath: matrixRoot + "/sites/" + domain,
		Status: state.SiteActive,
	}
	s.Config.CacheEnabled = cache
	switch typ {
	case state.SiteStatic:
	case state.SiteProxy:
		s.Config.ProxyUpstream = "http://127.0.0.1:3000"
	default:
		s.PHPVersion = "8.4"
	}
	in := engine.Input{
		Site:          s,
		Policy:        velocity.PolicyFor(s),
		DocRoot:       s.RootPath + "/current",
		CacheDir:      matrixRoot + "/cache/" + domain,
		LogDir:        matrixRoot + "/log/" + domain,
		CertFullchain: matrixRoot + "/certs/" + domain + "/fullchain.pem",
		CertKey:       matrixRoot + "/certs/" + domain + "/privkey.pem",
		FallbackCert:  matrixRoot + "/fallback/cert.pem",
		FallbackKey:   matrixRoot + "/fallback/key.pem",
		ACMEWebroot:   matrixRoot + "/acme",
		AuthLogSocket: "/run/cloudground/auth-log.sock",
	}
	if typ != state.SiteStatic && typ != state.SiteProxy {
		in.PHPSocket = matrixRoot + "/php/" + s.SystemUser + "/php.sock"
	}
	return in
}

// matrixCases is the whole matrix. Case IDs and names are stable: new cases
// go at the end.
func matrixCases() []matrixCase {
	var cases []matrixCase
	id := int64(0)
	add := func(name string, b matrixBuild, in engine.Input) {
		in.HTTP2Directive, in.HTTP3 = b.http2Directive, b.http3
		cases = append(cases, matrixCase{name: name, build: b, in: in})
	}
	types := []state.SiteType{state.SiteWordPress, state.SiteWooCommerce, state.SiteStatic, state.SitePHP, state.SiteLaravel, state.SiteProxy}
	profiles := []state.Profile{state.ProfileBalanced, state.ProfileCommerce, state.ProfileMaximum}
	for _, b := range matrixBuilds {
		for _, typ := range types {
			for _, prof := range profiles {
				for _, cache := range []bool{false, true} {
					id++
					c := "nocache"
					if cache {
						c = "cache"
					}
					name := fmt.Sprintf("%s-%s-%s-%s", b.name, typ, prof, c)
					add(name, b, matrixInput(id, typ, prof, cache))
				}
			}
		}
	}

	// Options, each on its own, on every build.
	type option struct {
		name string
		typ  state.SiteType
		set  func(*engine.Input)
	}
	hash := "$2a$10$" + strings.Repeat("b", 53)
	options := []option{
		{"hsts-cert", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Config.HSTS, in.CertAvailable = true, true
		}},
		{"hsts-fallback", state.SiteWordPress, func(in *engine.Input) { in.Site.Config.HSTS = true }},
		{"cert", state.SitePHP, func(in *engine.Input) { in.CertAvailable = true }},
		{"aliases", state.SiteWooCommerce, func(in *engine.Input) {
			in.Site.Aliases = []string{"www." + in.Site.Domain, "shop2.example.test"}
		}},
		{"staging", state.SiteWordPress, func(in *engine.Input) {
			in.Site.StagingOf = 1
			in.Site.Config.StagingAuthHash = hash
			in.BasicAuthFile = matrixRoot + "/auth/" + in.Site.Domain + ".htpasswd"
			in.InternalKey = in.Site.InternalKey()
		}},
		{"basicauth-nokey", state.SiteStatic, func(in *engine.Input) {
			in.BasicAuthFile = matrixRoot + "/auth/" + in.Site.Domain + ".htpasswd"
		}},
		{"staging-proxy", state.SiteProxy, func(in *engine.Input) {
			in.Site.Config.StagingAuthHash = hash
			in.BasicAuthFile = matrixRoot + "/auth/" + in.Site.Domain + ".htpasswd"
			in.InternalKey = in.Site.InternalKey()
			in.Site.Config.HSTS, in.CertAvailable = true, true
		}},
		{"public-root", state.SiteLaravel, func(in *engine.Input) {
			in.Site.Config.PublicRoot = "public"
			in.SymlinkFrom = in.DocRoot
			in.DocRoot += "/public"
		}},
		{"php-timeout", state.SitePHP, func(in *engine.Input) {
			in.Site.Config.PHP.MaxExecutionSeconds = 300
		}},
		{"body-limit", state.SiteWordPress, func(in *engine.Input) { in.ClientMaxBody = "512m" }},
		{"ttl-override", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Config.CacheTTLSec = 45
			in.Policy = velocity.PolicyFor(in.Site)
		}},
		{"long-ttl", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Config.CacheTTLSec = 8 * 24 * 3600
			in.Policy = velocity.PolicyFor(in.Site)
		}},
		{"query-bypass", state.SitePHP, func(in *engine.Input) { in.Policy.IgnoreTrackingArgs = false }},
		{"no-bypass-lists", state.SitePHP, func(in *engine.Input) {
			in.Policy.BypassCookies, in.Policy.BypassURIPrefixes = nil, nil
		}},
		{"php-without-socket", state.SitePHP, func(in *engine.Input) { in.PHPSocket = "" }},
		// A site's own bypass cookies (R-10.5): dots and dashes reach the
		// regex quoted.
		{"site-bypass-cookies", state.SiteLaravel, func(in *engine.Input) {
			in.Site.Config.BypassCookies = []string{"myapp.sid", "Tenant-Session_2"}
			in.Policy = velocity.PolicyFor(in.Site)
		}},
		{"proxy-https-upstream", state.SiteProxy, func(in *engine.Input) {
			in.Site.Config.ProxyUpstream = "https://127.0.0.1:8443/"
		}},
		{"max-profile-commerce-type", state.SiteWooCommerce, func(in *engine.Input) {
			in.Site.Profile = state.ProfileMaximum
			in.Policy = velocity.PolicyFor(in.Site)
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range options {
			id++
			name := fmt.Sprintf("%s-opt-%s", b.name, o.name)
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic && o.typ != state.SiteProxy)
			o.set(&in)
			add(name, b, in)
		}
	}

	// Custom routing blocks (SPEC §7, R-5.4), on every build.
	routings := []option{
		{"routing-spa", state.SiteStatic, func(in *engine.Input) { in.Site.Config.Routing = routingSPA }},
		{"routing-static-404", state.SiteStatic, func(in *engine.Input) { in.Site.Config.Routing = routingStatic404 }},
		{"routing-slim", state.SitePHP, func(in *engine.Input) { in.Site.Config.Routing = routingSlim }},
		{"routing-api-proxy", state.SiteLaravel, func(in *engine.Input) { in.Site.Config.Routing = routingAPIProxy }},
		{"routing-redirects", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Config.Routing = routingRedirects
			in.Site.Config.HSTS, in.CertAvailable = true, true
		}},
		{"routing-proxy", state.SiteProxy, func(in *engine.Input) { in.Site.Config.Routing = routingProxy }},
		{"routing-quoted", state.SitePHP, func(in *engine.Input) { in.Site.Config.Routing = routingQuoted }},
	}
	for _, b := range matrixBuilds {
		for _, o := range routings {
			id++
			name := fmt.Sprintf("%s-%s", b.name, o.name)
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic && o.typ != state.SiteProxy)
			o.set(&in)
			add(name, b, in)
		}
	}
	// The allowlist's directives (SPEC §7.3, after the R-5.4 review), on
	// every build.
	allowlisted := []option{
		{"routing-allowlist", state.SiteStatic, func(in *engine.Input) {
			in.Site.Config.Routing = "location / {\n    try_files $uri $uri/ =404;\n}\n" +
				"location /files/ {\n    autoindex off;\n}\n" +
				"location /media/ {\n    alias " + in.Site.RootPath + "/current/media/;\n}\n" +
				"location = /data.map {\n    types { application/json map; }\n    default_type application/json;\n}\n" +
				"location /admin/ {\n    limit_except GET HEAD { deny all; }\n    if (!-f $request_filename) { return 404; }\n" +
				"    expires 1h;\n    charset utf-8;\n    index index.html;\n}\n" +
				"error_page 404 =200 /index.html;\n"
		}},
		{"routing-proxy-headers", state.SiteLaravel, func(in *engine.Input) {
			in.Site.Config.Routing = "location / {\n    try_files $uri $uri/ /index.php?$args;\n}\n" +
				"location /api/ {\n    proxy_pass http://127.0.0.1:9000/;\n    proxy_http_version 1.1;\n" +
				"    proxy_set_header Host " + in.Site.Domain + ";\n    proxy_set_header X-Forwarded-Proto $scheme;\n" +
				"    proxy_connect_timeout 5s;\n    proxy_send_timeout 30s;\n    proxy_read_timeout 30s;\n" +
				"    proxy_buffering off;\n    proxy_redirect off;\n    proxy_hide_header X-Powered-By;\n}\n" +
				"rewrite ^/old/(.*)$ /new/$1 permanent;\n"
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range allowlisted {
			id++
			name := fmt.Sprintf("%s-%s", b.name, o.name)
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic && o.typ != state.SiteProxy)
			o.set(&in)
			add(name, b, in)
		}
	}
	// Protected paths (#26, SPEC §4.5), on every build: a WordPress site
	// with a path inside another (the longer one first, admin-ajax.php
	// left open) and a proxy site.
	protected := []option{
		{"protected-paths", state.SiteWordPress, func(in *engine.Input) {
			in.ProtectedPaths = []engine.ProtectedPath{
				{Path: "/wp-admin", File: matrixRoot + "/auth/" + in.Site.Domain + ".path-a.htpasswd"},
				{Path: "/wp-admin/tools.d", File: matrixRoot + "/auth/" + in.Site.Domain + ".path-b.htpasswd"},
			}
		}},
		{"protected-paths-proxy", state.SiteProxy, func(in *engine.Input) {
			in.ProtectedPaths = []engine.ProtectedPath{{Path: "/private~files", File: matrixRoot + "/auth/" + in.Site.Domain + ".path-c.htpasswd"}}
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range protected {
			id++
			name := fmt.Sprintf("%s-opt-%s", b.name, o.name)
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic && o.typ != state.SiteProxy)
			o.set(&in)
			add(name, b, in)
		}
	}
	// Blocking (#25, SPEC §4.7), on every build: a WordPress site with
	// addresses, user agents and countries refused, and a static site that
	// serves two countries only.
	blocking := []option{
		{"blocking", state.SiteWordPress, func(in *engine.Input) {
			in.Blocking = &engine.Blocking{
				Exempt:          []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")},
				Addresses:       []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("198.51.100.7/32"), netip.MustParsePrefix("2001:db8:bad::/48")},
				UserAgents:      []string{"GPTBot", "Ahrefs.Bot", "x(y)"},
				Countries:       []string{"RU", "CN"},
				CountryPrefixes: []netip.Prefix{netip.MustParsePrefix("1.0.1.0/24"), netip.MustParsePrefix("1.0.2.0/23"), netip.MustParsePrefix("2a00:1::/32")},
			}
		}},
		{"blocking-countries-only", state.SiteStatic, func(in *engine.Input) {
			in.Blocking = &engine.Blocking{
				Exempt:             []netip.Addr{netip.MustParseAddr("192.0.2.10")},
				Countries:          []string{"IT", "SM"},
				CountryPrefixes:    []netip.Prefix{netip.MustParsePrefix("2.32.0.0/11"), netip.MustParsePrefix("2001:b07::/32")},
				AllowCountriesOnly: true,
			}
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range blocking {
			id++
			name := fmt.Sprintf("%s-opt-%s", b.name, o.name)
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic && o.typ != state.SiteProxy)
			o.set(&in)
			add(name, b, in)
		}
	}
	// WordPress hardening (#75, SPEC §4.8), after the others so their
	// site ids stay: every item, with exceptions of both families; and
	// XML-RPC off alone, with none.
	hardening := []option{
		{"wp-hardening", state.SiteWooCommerce, func(in *engine.Input) {
			in.Site.Config.WPHardening = state.WPHardening{XMLRPCOff: true, XMLRPCAllow: []string{"192.0.2.10", "198.51.100.0/24", "2001:db8::/32"},
				FileEditOff: true, HideVersion: true, BlockReadme: true, LoginPath: "my-door"}
		}},
		{"wp-hardening-xmlrpc", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Config.WPHardening = state.WPHardening{XMLRPCOff: true}
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range hardening {
			id++
			in := matrixInput(id, o.typ, state.ProfileBalanced, true)
			o.set(&in)
			add(fmt.Sprintf("%s-opt-%s", b.name, o.name), b, in)
		}
	}
	// A maintenance page (#79, SPEC §4.9), last for the same reason: a
	// WordPress site with a text full of characters nginx or HTML would
	// read, and a static site with the defaults.
	maintenance := []option{
		{"maintenance", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Config.Maintenance = state.SiteMaintenance{
				Enabled: true, Key: "0123456789abcdef0123456789abcdef", RetryAfterSec: 1800,
				Title:   `Back at "noon" <b>&</b>`,
				Message: "We are moving servers.\nNo orders until 14:00 (CET) ${host} 'quoted' \\ {x};\nMerci, à bientôt",
				Allow:   []string{"198.51.100.0/24", "2001:db8::1"},
			}
			in.MaintenanceExempt = []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")}
		}},
		{"maintenance-static", state.SiteStatic, func(in *engine.Input) {
			in.Site.Config.Maintenance = state.SiteMaintenance{Enabled: true, Key: "fedcba9876543210fedcba9876543210"}
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range maintenance {
			id++
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic)
			o.set(&in)
			add(fmt.Sprintf("%s-opt-%s", b.name, o.name), b, in)
		}
	}
	// Redirects and a canonical host (#83, SPEC §4.10), last again: a
	// WordPress site with the www name canonical and every kind of rule, and
	// a static site with one rule and no canonical host.
	redirects := []option{
		{"redirects", state.SiteWordPress, func(in *engine.Input) {
			in.Site.Aliases = []string{"www." + in.Site.Domain}
			in.Site.Config.Redirects = state.SiteRedirects{
				Canonical: "www." + in.Site.Domain,
				Rules: []state.RedirectRule{
					{Match: state.RedirectExact, From: "/old-page", To: "/new-page", Status: 301},
					{Match: state.RedirectExact, From: "/shop/", To: "/store/?src=old", Status: 302},
					{Match: state.RedirectPrefix, From: "/blog", To: "/news", Status: 301},
					{Match: state.RedirectPrefix, From: "/docs/v1", To: "https://docs.example.org/v1", Status: 302},
					{Match: state.RedirectPrefix, From: "/a.b+c(d)", To: "/", Status: 301},
				},
			}
		}},
		{"redirects-static", state.SiteStatic, func(in *engine.Input) {
			in.Site.Config.Redirects = state.SiteRedirects{
				Rules: []state.RedirectRule{{Match: state.RedirectExact, From: "/", To: "/home.html", Status: 302}},
			}
		}},
	}
	for _, b := range matrixBuilds {
		for _, o := range redirects {
			id++
			in := matrixInput(id, o.typ, state.ProfileBalanced, o.typ != state.SiteStatic)
			o.set(&in)
			add(fmt.Sprintf("%s-opt-%s", b.name, o.name), b, in)
		}
	}
	// Behind Cloudflare (#22, SPEC §4.11), last for the same reason: a
	// WordPress site that also refuses visitors, so the order of the two
	// is pinned.
	for _, b := range matrixBuilds {
		id++
		in := matrixInput(id, state.SiteWordPress, state.ProfileBalanced, true)
		in.CloudflareInclude = Renderer{}.CloudflareIncludeFile()
		in.Site.Config.Blocking = state.SiteBlocking{Addresses: []string{"203.0.113.0/24"}}
		in.Blocking = &engine.Blocking{Addresses: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}
		add(fmt.Sprintf("%s-opt-cloudflare", b.name), b, in)
	}
	// Managed apps (#28, SPEC §4.12), last for the same reason: a Node.js
	// app on its socket, a Python app on a loopback port.
	for _, b := range matrixBuilds {
		for _, typ := range []state.SiteType{state.SiteNode, state.SitePython} {
			id++
			in := matrixInput(id, typ, state.ProfileBalanced, false)
			in.Site.PHPVersion, in.PHPSocket = "", ""
			in.Site.Config.App = state.SiteApp{Version: "22", Start: "node server.js"}
			in.AppUpstream = "unix:/run/cloudground/app/" + in.Site.SystemUser + "/app.sock"
			if typ == state.SitePython {
				in.Site.Config.App = state.SiteApp{Version: "3.12", Start: "python -m gunicorn --bind 127.0.0.1:8001 app:app", Port: 8001}
				in.AppUpstream = "127.0.0.1:8001"
			}
			in.Policy = velocity.PolicyFor(in.Site)
			add(fmt.Sprintf("%s-opt-%s", b.name, typ), b, in)
		}
	}
	return cases
}

// matrixText is one case's files, sorted by path.
func matrixText(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "## %s\n%s", p, files[p])
	}
	return b.String()
}

func TestRenderMatrix(t *testing.T) {
	dump := os.Getenv("CG_NGINX_DUMP")
	r := Renderer{}
	cases := matrixCases()
	seen := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files, err := r.SiteFiles(c.in)
			if err != nil {
				t.Fatal(err)
			}
			seen[c.name+".golden"] = true
			checkMatrix(t, c.name, matrixText(files))
			if dump != "" {
				if c.in.CloudflareInclude != "" {
					// The agent's shared files, which nginx -t needs (ops/sites SPEC §10).
					files[r.CloudflareIncludeFile()] = CloudflareInclude(cfips.Shipped())
					files[r.BannedFile()] = Banned([]netip.Prefix{netip.MustParsePrefix("198.51.100.7/32")}, nil)
				}
				dumpCase(t, filepath.Join(dump, c.build.name), files)
			}
		})
	}
	// The standalone functions' files (#143 v0.9.0, SPEC §8), per build: a
	// functions domain and three functions, one with its own domains and a
	// list of origins, one behind a token, one open to every origin.
	for _, b := range matrixBuilds {
		name := b.name + "-functions"
		t.Run(name, func(t *testing.T) {
			files, err := r.FunctionsFiles(matrixFunctions(b))
			if err != nil {
				t.Fatal(err)
			}
			seen[name+".golden"] = true
			checkMatrix(t, name, matrixText(files))
			if dump != "" {
				dumpCase(t, filepath.Join(dump, b.name), files)
			}
		})
	}
	t.Run("global", func(t *testing.T) {
		seen["global.golden"] = true
		checkMatrix(t, "global", matrixText(r.GlobalFiles()))
		if dump != "" {
			for _, b := range matrixBuilds {
				dumpCase(t, filepath.Join(dump, b.name), r.GlobalFiles())
			}
		}
	})
	// No golden is left over from a case that no longer exists.
	if !*updateMatrix {
		ents, _ := os.ReadDir(matrixDir)
		for _, e := range ents {
			if !seen[e.Name()] {
				t.Errorf("testdata/matrix/%s has no case", e.Name())
			}
		}
	}
}

func checkMatrix(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(matrixDir, name+".golden")
	if *updateMatrix {
		if err := os.MkdirAll(matrixDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden (run with -update-matrix): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s: the render changed.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

// dumpCase writes files under dir, keeping the last two path elements
// (sites-enabled/<x>.conf, conf.d/<y>.conf).
func dumpCase(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		dst := filepath.Join(dir, filepath.Base(filepath.Dir(p)), filepath.Base(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// matrixFunctions is the functions' input of a build (SPEC §8).
func matrixFunctions(b matrixBuild) FunctionsInput {
	cert, key := matrixRoot+"/fallback/cert.pem", matrixRoot+"/fallback/key.pem"
	fn := func(id int64, name string) FunctionServer {
		return FunctionServer{ID: id, Name: name, Socket: functions.SocketPath(id), TimeoutSec: 10, AccessLog: functions.AccessLog(name)}
	}
	hello := fn(1, "hello")
	hello.CORSOrigins = []string{"https://app.example.test", "http://localhost:3000"}
	hello.Domains, hello.CertFile, hello.KeyFile = []string{"api.example.test", "api2.example.test"}, cert, key
	hook := fn(2, "stripe-hook")
	hook.Token = "cgf_" + strings.Repeat("A", 43)
	hook.TimeoutSec = 30
	open := fn(3, "open")
	open.CORSOrigins = []string{"*"}
	return FunctionsInput{
		Domain: "fn.example.test", DomainCert: cert, DomainKey: key, ACMEWebroot: matrixRoot + "/acme",
		InternalSocket: "/var/lib/cloudground-agent/nginx-functions.sock", HTTP2Directive: b.http2Directive,
		Functions: []FunctionServer{hook, hello, open},
	}
}
