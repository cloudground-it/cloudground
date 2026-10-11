// Package nginx is the nginx + PHP-FPM engine: a site's vhost with the
// Velocity Engine page cache, its cache zone, and the http-level file every
// site shares. internal/engine/SPEC.md is the specification; the render
// matrix in testdata/matrix pins every byte.
package nginx

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Where the files go by default (SPEC §2).
const (
	DefaultSitesDir = "/etc/nginx/sites-enabled"
	DefaultConfDir  = "/etc/nginx/conf.d"
)

// Renderer renders into SitesDir and ConfDir; empty is the default.
type Renderer struct {
	SitesDir string
	ConfDir  string
}

// Name is the engine's name.
func (Renderer) Name() state.Engine { return state.EngineNginx }

func orDefault(dir, def string) string {
	if dir == "" {
		return def
	}
	return dir
}

// ValidateDomain refuses anything but a plain lowercase hostname: a domain
// is pasted into the configuration (internal/site).
func ValidateDomain(d string) error { return site.ValidateDomain(d) }

// NormalizeProxyUpstream is the upstream rebuilt from its validated parts
// (internal/site).
func NormalizeProxyUpstream(raw string) (string, error) { return site.NormalizeProxyUpstream(raw) }

//go:embed global.conf
var globalConf string

// GlobalFiles is the http-level file every site relies on (SPEC §3.1).
func (r Renderer) GlobalFiles() map[string]string {
	return map[string]string{filepath.Join(orDefault(r.ConfDir, DefaultConfDir), "cloudground-global.conf"): globalConf}
}

// vhost is one site's render in progress: the input with what follows from
// it.
type vhost struct {
	in       engine.Input
	php      bool // a PHP location and its routing
	routing  string
	parsed   *parsed
	sec      []string // the security headers, one directive each
	skipVar  string
	zoneName string
}

// prepare checks the input and works out what the vhost is made of
// (SPEC §2).
func prepare(in engine.Input) (*vhost, error) {
	s := in.Site
	for _, d := range append([]string{s.Domain}, s.Aliases...) {
		if err := ValidateDomain(d); err != nil {
			return nil, err
		}
	}
	if s.Type == state.SiteProxy {
		up, err := NormalizeProxyUpstream(s.Config.ProxyUpstream)
		if err != nil {
			return nil, err
		}
		in.Site.Config.ProxyUpstream = up
	}
	if in.ClientMaxBody == "" {
		in.ClientMaxBody = "128m"
	}
	v := &vhost{
		in:       in,
		php:      in.PHPSocket != "" && s.Type != state.SiteProxy && s.Type != state.SiteStatic,
		skipVar:  "$cg_skip_" + strconv.FormatInt(s.ID, 10),
		zoneName: "cg_" + strconv.FormatInt(s.ID, 10),
	}
	v.sec = []string{
		"add_header X-Content-Type-Options nosniff always;",
		"add_header X-Frame-Options SAMEORIGIN always;",
		"add_header Referrer-Policy strict-origin-when-cross-origin always;",
	}
	if in.HTTP3 {
		v.sec = append(v.sec, `add_header Alt-Svc 'h3=":443"; ma=86400' always;`)
	}
	// Over the self-signed fallback HSTS would lock browsers out for a year.
	if s.Config.HSTS && in.CertAvailable {
		v.sec = append(v.sec, `add_header Strict-Transport-Security "max-age=31536000" always;`)
	}
	v.routing = DefaultRouting(in.Site, v.php)
	if v.routing == "" {
		return v, nil
	}
	var err error
	if custom := NormalizeRouting(s.Config.Routing); custom != "" {
		// Checked at every render, not only when it was saved (SPEC §7.1).
		v.routing = custom
		v.parsed, err = checkRouting(custom, RoutingContextOf(in))
	} else {
		v.parsed, err = parseConf(v.routing)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: routing: %w", s.Domain, err)
	}
	return v, nil
}

// RoutingContextOf is what the site's routing block is checked against.
func RoutingContextOf(in engine.Input) RoutingContext {
	rc := RoutingContext{
		SiteRoot: in.Site.RootPath, Domains: append([]string{in.Site.Domain}, in.Site.Aliases...),
		SystemPorts: in.SystemPorts, LocalAddrs: in.LocalAddrs, Resolve: in.Resolve,
	}
	if in.Site.Type != state.SiteProxy {
		rc.DocRoot = in.DocRoot
	}
	return rc
}

// SiteFiles is the site's vhost and, when the policy caches, its cache
// zone (SPEC §2).
func (r Renderer) SiteFiles(in engine.Input) (map[string]string, error) {
	v, err := prepare(in)
	if err != nil {
		return nil, err
	}
	before, block, after := v.render()
	files := map[string]string{
		filepath.Join(orDefault(r.SitesDir, DefaultSitesDir), in.Site.Domain+".conf"): before + block + after,
	}
	if in.Policy.Enabled {
		files[filepath.Join(orDefault(r.ConfDir, DefaultConfDir), fmt.Sprintf("cloudground-cache-%d.conf", in.Site.ID))] = v.zone()
	}
	return files, nil
}

// Routing is the site's routing for the editor (SPEC §7.5).
func (r Renderer) Routing(in engine.Input) (engine.RoutingView, error) {
	v, err := prepare(in)
	if err != nil {
		return engine.RoutingView{}, err
	}
	before, _, after := v.render()
	view := engine.RoutingView{Default: DefaultRouting(v.in.Site, v.php), Before: before, After: after}
	if view.Default != "" {
		view.Current = NormalizeRouting(in.Site.Config.Routing)
	}
	return view, nil
}

// zone is the cache zone (SPEC §3.2).
func (v *vhost) zone() string {
	p := v.in.Policy
	inactive := p.TTLSec + p.StaleSec
	// A week at least: an entry evicted sooner costs its next visitor a full
	// render instead of a stale copy refreshed in the background.
	if week := 7 * 24 * 3600; inactive < week {
		inactive = week
	}
	return fmt.Sprintf("# Written by CloudGround: page-cache zone of %s (site %d).\n"+
		"fastcgi_cache_path %s levels=1:2 keys_zone=%s:%dm\n"+
		"                   max_size=%dm inactive=%ds use_temp_path=off;\n",
		v.in.Site.Domain, v.in.Site.ID, v.in.CacheDir, v.zoneName, p.ZoneSizeMB, p.MaxSizeMB, inactive)
}

// conf writes configuration lines, four spaces per level.
type conf struct{ strings.Builder }

func (c *conf) put(level int, lines ...string) {
	for _, l := range lines {
		if l != "" {
			c.WriteString(strings.Repeat("    ", level))
			c.WriteString(l)
		}
		c.WriteByte('\n')
	}
}

// putf writes one formatted line.
func (c *conf) putf(level int, format string, args ...any) {
	c.put(level, fmt.Sprintf(format, args...))
}

// headers writes the security headers. Inside a location they keep the
// server level's indent: the matrix pins it.
func (v *vhost) headers(c *conf) { c.put(1, v.sec...) }

// render is the vhost in three parts: the panel's text before the routing
// block, the block, and the panel's text after it (SPEC §4, §7.1).
func (v *vhost) render() (before, block, after string) {
	in, s := v.in, v.in.Site
	names := strings.Join(append([]string{s.Domain}, s.Aliases...), " ")
	var c conf
	c.put(0, "# Managed by CloudGround — do not edit. Changes are detected as drift.")
	c.putf(0, "# Site: %s (id %d, type %s, profile %s)", s.Domain, s.ID, s.Type, s.Profile)
	if in.BasicAuthFile != "" {
		c.put(0, "")
		c.putf(0, "map $http_x_cloudground_internal $cg_staging_realm_%d {", s.ID)
		if in.InternalKey != "" {
			c.putf(1, "%q  off;", in.InternalKey)
		}
		c.put(1, `default  "Staging";`)
		c.put(0, "}")
	}

	c.put(0, "", "server {")
	c.put(1, "listen 80;", "listen [::]:80;", "server_name "+names+";", "")
	c.put(1, "location /.well-known/acme-challenge/ {")
	c.put(2, "root "+in.ACMEWebroot+";")
	c.put(1, "}", "location / {")
	c.put(2, "return 301 https://$host$request_uri;")
	c.put(1, "}")
	c.put(0, "}", "", "server {")

	if in.HTTP2Directive {
		c.put(1, "listen 443 ssl;", "listen [::]:443 ssl;", "http2 on;")
	} else {
		c.put(1, "listen 443 ssl http2;", "listen [::]:443 ssl http2;")
	}
	if in.HTTP3 {
		c.put(1,
			"# HTTP/3 over QUIC, present because this nginx has the v3 module.",
			`# "reuseport" is allowed once per address and this file is one site`,
			"# among many, so the default server set up at install time holds it.",
			"listen 443 quic;", "listen [::]:443 quic;")
	}
	c.put(1, "server_name "+names+";", "server_tokens off;")
	if in.BasicAuthFile != "" {
		c.put(1, "",
			"# A staging copy asks for a password. CloudGround's own requests (guard",
			"# probes, warming, connector flushes) skip it with a header key kept in",
			"# a file only root reads; 127.0.0.1 gets no pass, since PHP runs here.")
		c.putf(1, "auth_basic $cg_staging_realm_%d;", s.ID)
		c.put(1, "auth_basic_user_file "+in.BasicAuthFile+";")
	}
	c.put(1, "",
		"# Send small responses at once. With Ubuntu's tcp_nopush and no",
		"# tcp_nodelay, the last packet of a kept-alive reply waits for the peer's",
		"# delayed ACK (40 ms); measured, that cut cached req/s by about ten times.",
		"tcp_nodelay on;", "tcp_nopush off;", "")

	cert, key := in.FallbackCert, in.FallbackKey
	if in.CertAvailable {
		cert, key = in.CertFullchain, in.CertKey
	}
	c.put(1, "ssl_certificate "+cert+";", "ssl_certificate_key "+key+";",
		"ssl_protocols TLSv1.2 TLSv1.3;", "ssl_prefer_server_ciphers off;",
		"ssl_session_timeout 1d;", "ssl_session_cache shared:cg_ssl:10m;", "",
		"# Log lines go out in 64 KiB batches, at least once a second.",
		"access_log "+in.LogDir+"/access.log cloudground buffer=64k flush=1s;",
		"error_log "+in.LogDir+"/error.log warn;", "",
		"client_max_body_size "+in.ClientMaxBody+";")
	if in.HTTP3 {
		c.put(1, "http3 on;")
	}
	v.headers(&c)

	if s.Type != state.SiteProxy {
		c.put(1, "",
			"root "+in.DocRoot+";",
			"index index.php index.html index.htm;",
			"disable_symlinks if_not_owner from="+symlinkFrom(in)+";", "",
			`location ~ /\.(?!well-known) {`)
		c.put(2, "deny all;")
		c.put(1, "}", "",
			"# Leftovers are never served: backup and editor copies, SQL dumps, copies",
			"# of wp-config.php and archives at the top of the root (SPEC §4.4).",
			"location ~* "+leftoverRegex+" {")
		c.put(2, "return 404;")
		c.put(1, "}")
		if s.Type == state.SiteWordPress || s.Type == state.SiteWooCommerce {
			// Before every other regex location (the routing block's and
			// the PHP one): the first that matches wins (SPEC §4.4).
			c.put(1, "",
				"# Uploads are data: a .php a plugin let through is never run (SR-T9).",
				`location ~* ^/wp-content/uploads/.*\.php$ {`)
			c.put(2, "deny all;")
			c.put(1, "}")
		}
		c.put(1, "",
			`location ~* \.(css|js|mjs|jpg|jpeg|png|gif|webp|avif|svg|ico|woff|woff2|ttf|eot|otf|mp4|webm|pdf)$ {`)
		c.put(2, "expires 30d;", `add_header Cache-Control "public, immutable";`)
		v.headers(&c)
		c.put(2, "access_log off;",
			"# A deploy writes .gz copies; send those rather than gzip each time.",
			"gzip_static on;", "try_files $uri =404;")
		c.put(1, "}")
	}
	if v.routing == "" {
		c.put(0, "}")
		return c.String(), "", ""
	}
	c.put(0, "")
	before = c.String()
	block = renderRouting(v.routing, v.parsed, v.sec)

	var t conf
	if v.php {
		v.php_(&t)
	}
	t.put(0, "}")
	return before, block, t.String()
}

// leftoverRegex matches, without regard to case, the paths a site never
// serves (SPEC §4.4): a name ending in a backup or editor suffix (.bak,
// .old, .orig, .save, .swp, .swo, ~), an SQL dump (.sql, compressed or
// not), a copy of wp-config.php under any longer name, and an archive at
// the top of the document root, where dumps are left (an archive deeper
// down, a plugin's download, is served).
const leftoverRegex = `(?:\.(?:bak|old|orig|save|swp|swo|sql|sql\.(?:gz|bz2|xz|zst|zip))|~|/wp-config\.php.+|^/[^/]+\.(?:tar|tar\.(?:gz|bz2|xz|zst)|tgz|tbz2|txz|zip|7z|rar))$`

// php_ is what follows a PHP site's routing: the cache bypass and the PHP
// location (SPEC §4.2, §5.1).
func (v *vhost) php_(c *conf) {
	in, p := v.in, v.in.Policy
	if p.Enabled {
		c.put(1, "",
			"# Who skips the page cache: POSTs, query strings, the session cookies",
			"# and the paths below (logins, carts, the admin) are always rendered.")
		skip := func(cond string) {
			c.putf(1, "if (%s) {", cond)
			c.putf(2, "set %s 1;", v.skipVar)
			c.put(1, "}")
		}
		c.putf(1, "set %s 0;", v.skipVar)
		skip("$request_method = POST")
		if p.IgnoreTrackingArgs {
			skip("$cg_args_bypass")
		} else {
			skip(`$query_string != ""`)
		}
		if len(p.BypassCookies) > 0 {
			quoted := make([]string, len(p.BypassCookies))
			for i, ck := range p.BypassCookies {
				quoted[i] = regexp.QuoteMeta(ck)
			}
			skip(`$http_cookie ~* "` + strings.Join(quoted, "|") + `"`)
		}
		if len(p.BypassURIPrefixes) > 0 {
			res := make([]string, len(p.BypassURIPrefixes))
			for i, pre := range p.BypassURIPrefixes {
				res[i] = "^" + regexp.QuoteMeta(pre)
				// End of a path segment, so /feed is not /feedback.
				if !strings.HasSuffix(pre, "/") && !strings.HasSuffix(pre, "=") {
					res[i] += "([/?]|$)"
				}
			}
			skip(`$request_uri ~* "` + strings.Join(res, "|") + `"`)
		}
	}
	c.put(1, "", `location ~ \.php$ {`)
	c.put(2, "try_files $uri =404;", "include fastcgi_params;",
		"fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;",
		"fastcgi_param HTTPS on;",
		"fastcgi_pass unix:"+in.PHPSocket+";")
	c.putf(2, "fastcgi_read_timeout %ds;", in.Site.Config.PHP.RequestTimeoutSeconds())
	c.put(2, "fastcgi_buffers 16 32k;", "fastcgi_buffer_size 64k;")
	if p.Enabled {
		key := "$cg_key"
		if !p.IgnoreTrackingArgs {
			key = "$request_uri"
		}
		c.put(2, "",
			"# Page cache. PHP is always asked for gzip, so each page is stored once,",
			"# compressed; gunzip serves the few clients that do not accept it.",
			`fastcgi_param HTTP_ACCEPT_ENCODING "gzip";`,
			"gunzip on;",
			"# After a purge an open descriptor may serve the old file for 1s at most.",
			"open_file_cache_valid 1s;",
			"fastcgi_cache "+v.zoneName+";",
			`fastcgi_cache_key "$scheme$request_method$host`+key+`";`)
		c.putf(2, "fastcgi_cache_valid 200 301 %ds;", p.TTLSec)
		c.put(2, "fastcgi_cache_valid 404 60s;", "fastcgi_cache_methods GET HEAD;",
			"fastcgi_cache_bypass "+v.skipVar+" $cg_private_req;",
			"fastcgi_no_cache "+v.skipVar+" $cg_no_store $cg_private_req;",
			"# One request renders a missing page; the others wait for it.",
			"fastcgi_cache_lock on;")
		c.putf(2, "fastcgi_cache_lock_timeout %ds;", p.LockTimeoutSec)
		c.put(2, "# Expired or failing: answer from the old copy, refresh in background.",
			"fastcgi_cache_use_stale updating error timeout invalid_header http_500 http_503;",
			"fastcgi_cache_background_update on;",
			"# The key fixes the encoding, so Vary is not needed. Set-Cookie still",
			"# counts: a reply that sets a cookie stays out of the cache, or every",
			"# later visitor would receive that cookie.",
			"fastcgi_ignore_headers Cache-Control Expires Vary;",
			"add_header X-CloudGround-Cache $upstream_cache_status always;")
	}
	v.headers(c)
	c.put(1, "}")
}

// symlinkFrom is where nginx starts checking links (SPEC §4): the release
// of a site with a public root, else the document root.
func symlinkFrom(in engine.Input) string {
	if in.SymlinkFrom != "" {
		return in.SymlinkFrom
	}
	return "$document_root"
}
