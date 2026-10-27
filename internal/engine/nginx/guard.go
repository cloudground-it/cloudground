package nginx

import (
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The routing block's allowlist (SPEC §7.3): what a directive may be, and
// what it may reach. The denylist of routing.go runs after it, as a second
// check on the same tree.

// RoutingContext is what a routing block is checked against: the site and
// what of this machine it must not reach.
type RoutingContext struct {
	// SiteRoot is <SitesRoot>/<domain>; DocRoot what the vhost serves
	// ("" for a proxy site).
	SiteRoot string
	DocRoot  string
	// Domains are the site's names (domain and aliases).
	Domains []string
	// SystemPorts are TCP ports system services listen on here, beyond
	// the fixed ones (reservedPorts); LocalAddrs this machine's own
	// addresses. Both are filled in by the agent at every render.
	SystemPorts []int
	LocalAddrs  []netip.Addr
	// Resolve looks a proxy host name up; nil leaves names unchecked (the
	// panel's quick check: the agent's is the one that counts).
	Resolve func(host string) ([]netip.Addr, error)
	// AppUpstream is a managed app's own upstream name (cg_app_<id>, SPEC
	// §4.11): the one host name proxy_pass may take, as this machine's.
	AppUpstream string
}

// reservedPorts are the ports a local proxy target may never use: nginx
// itself (80, 443, the panel's 8443: through them any other tenant, by
// Host), MariaDB, Redis, the panel's API (5252) and its local listener
// (9080, the connector's).
var reservedPorts = []int{80, 443, 8443, 3306, 6379, 5252, 9080}

// checker checks a directive's own arguments; anc are the enclosing blocks,
// innermost last.
type checker func(g *guard, d *directive, anc []*directive) error

// allowed is every directive a routing block may hold, at any depth.
var allowed map[string]checker

func init() {
	none := func(*guard, *directive, []*directive) error { return nil }
	allowed = map[string]checker{
		"location":              checkLocation,
		"if":                    checkIf,
		"try_files":             checkTryFiles,
		"rewrite":               checkRewrite,
		"return":                checkReturn,
		"error_page":            checkErrorPage,
		"root":                  checkTree,
		"alias":                 checkTree,
		"index":                 none,
		"autoindex":             checkAutoindex,
		"default_type":          none,
		"types":                 checkTypes,
		"expires":               none,
		"add_header":            none,
		"charset":               none,
		"internal":              none,
		"set":                   none, // $cg_* is refused by the second check
		"break":                 none, // the rewrite module's own
		"allow":                 none, // access control, read-only
		"deny":                  none,
		"limit_except":          checkBlock,
		"proxy_pass":            checkProxyPass,
		"proxy_set_header":      checkProxyHeader,
		"proxy_http_version":    none,
		"proxy_read_timeout":    none,
		"proxy_connect_timeout": none,
		"proxy_send_timeout":    none,
		"proxy_buffering":       none,
		"proxy_redirect":        none,
		"proxy_hide_header":     none, // removes an upstream header
	}
}

// guard is one check of a block.
type guard struct {
	RoutingContext
	text  string
	local bool // some proxy_pass of the block goes to this machine or a private network
}

// allowPass applies the allowlist to the whole block.
func (g *guard) allowPass(top []*directive) error {
	// The proxy targets first: whether one is local decides the Host rule.
	var targets func(ds []*directive) error
	targets = func(ds []*directive) error {
		for _, d := range ds {
			if d.name() == "proxy_pass" && len(d.words) == 2 {
				local, err := g.target(d)
				if err != nil {
					return err
				}
				g.local = g.local || local
			}
			if err := targets(d.children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := targets(top); err != nil {
		return err
	}
	var walk func(ds []*directive, anc []*directive) error
	walk = func(ds []*directive, anc []*directive) error {
		for _, d := range ds {
			if len(anc) > 0 && anc[len(anc)-1].name() == "types" {
				if err := checkMIME(d); err != nil {
					return err
				}
				continue
			}
			check, ok := allowed[d.name()]
			if !ok {
				return refuse(d.line, "%q is not one of the directives a site's routing may use", d.words[0])
			}
			if d.block != blockDirective(d.name()) {
				if d.block {
					return refuse(d.line, "%s does not take a block", d.words[0])
				}
				return refuse(d.line, "%s needs a block", d.words[0])
			}
			if err := check(g, d, anc); err != nil {
				return err
			}
			if err := walk(d.children, append(anc[:len(anc):len(anc)], d)); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(top, nil)
}

func blockDirective(n string) bool {
	return n == "location" || n == "if" || n == "types" || n == "limit_except"
}

func checkBlock(*guard, *directive, []*directive) error { return nil }

func checkLocation(_ *guard, d *directive, _ []*directive) error {
	if n := len(d.words) - 1; n < 1 || n > 2 {
		return refuse(d.line, "location takes a path, or a modifier and a path")
	}
	if len(d.words) == 3 && !slices.Contains([]string{"=", "~", "~*", "^~"}, d.words[1]) {
		return refuse(d.line, "location modifier %q", d.words[1])
	}
	return nil
}

var (
	varRe      = `\$[A-Za-z_][A-Za-z0-9_]*`
	condVarRe  = regexp.MustCompile(`^` + varRe + `$`)
	condCmpRe  = regexp.MustCompile(`^(` + varRe + `)\s+(=|!=|~|~\*|!~|!~\*)\s+(.+)$`)
	condFileRe = regexp.MustCompile(`^!?-[fdex]\s+(\S+)$`)
)

// checkIf admits a variable's truth, a comparison or regex match of a
// variable, and a file test of $request_filename only: a file test of any
// other path would tell what exists outside the site.
func checkIf(_ *guard, d *directive, _ []*directive) error {
	cond := strings.Join(d.words[1:], " ")
	if !strings.HasPrefix(cond, "(") || !strings.HasSuffix(cond, ")") {
		return refuse(d.line, "if: the condition must be in parentheses")
	}
	cond = strings.TrimSpace(cond[1 : len(cond)-1])
	switch {
	case condVarRe.MatchString(cond), condCmpRe.MatchString(cond):
		return nil
	case condFileRe.MatchString(cond):
		if m := condFileRe.FindStringSubmatch(cond); m[1] == "$request_filename" {
			return nil
		}
		return refuse(d.line, "if: a file test may only test $request_filename")
	}
	return refuse(d.line, "if: %q is not a condition a site's routing may use", cond)
}

// localURI is a URI of this site: a path, or a named location.
func localURI(u string) bool {
	return strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") || strings.HasPrefix(u, "@")
}

func checkTryFiles(_ *guard, d *directive, _ []*directive) error {
	if len(d.words) < 3 {
		return refuse(d.line, "try_files takes files and a fallback")
	}
	last := d.words[len(d.words)-1]
	if !localURI(last) && !regexp.MustCompile(`^=[1-5][0-9][0-9]$`).MatchString(last) {
		return refuse(d.line, "try_files: the fallback must be a path, a named location or =<code>")
	}
	return nil
}

func checkRewrite(_ *guard, d *directive, _ []*directive) error {
	if n := len(d.words); n < 3 || n > 4 {
		return refuse(d.line, "rewrite takes a regex, a replacement and a flag")
	}
	if len(d.words) == 4 && !slices.Contains([]string{"last", "break", "redirect", "permanent"}, d.words[3]) {
		return refuse(d.line, "rewrite flag %q", d.words[3])
	}
	return nil
}

func checkReturn(_ *guard, d *directive, _ []*directive) error {
	if n := len(d.words); n < 2 || n > 3 {
		return refuse(d.line, "return takes a code and a text or URL")
	}
	if len(d.words) == 3 {
		if c, err := strconv.Atoi(d.words[1]); err != nil || c < 100 || c > 599 {
			return refuse(d.line, "return: %q is not a status code", d.words[1])
		}
	}
	return nil
}

// checkErrorPage: codes, an optional =<code> or =, and a local URI (no
// redirect to another host, no page from another site).
func checkErrorPage(_ *guard, d *directive, _ []*directive) error {
	args := d.words[1:]
	if len(args) < 2 {
		return refuse(d.line, "error_page takes codes and a page")
	}
	for _, a := range args[:len(args)-1] {
		// The codes caught are errors (300–599); "=<code>" answers with any.
		low := 300
		if strings.HasPrefix(a, "=") {
			low = 100
		}
		if c, err := strconv.Atoi(strings.TrimPrefix(a, "=")); (err != nil || c < low || c > 599) && a != "=" {
			return refuse(d.line, "error_page: %q is not a status code", a)
		}
	}
	if !localURI(args[len(args)-1]) {
		return refuse(d.line, "error_page: the page must be a path of this site or a named location")
	}
	return nil
}

var (
	mimeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$`)
	extRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

func checkTypes(*guard, *directive, []*directive) error { return nil }

// checkMIME is one line of a types block: a media type and its extensions.
func checkMIME(d *directive) error {
	if d.block || !mimeRe.MatchString(strings.ToLower(d.words[0])) || len(d.words) < 2 {
		return refuse(d.line, "types: %q is not a media type and its extensions", strings.Join(d.words, " "))
	}
	for _, e := range d.words[1:] {
		if !extRe.MatchString(e) {
			return refuse(d.line, "types: %q is not an extension", e)
		}
	}
	return nil
}

// releaseTree says whether p is in the site's release tree:
// <root>/current or <root>/releases/<name>, or beneath, with no hidden
// segment below. Never the root itself, shared/, logs/, tmp/, src/, .ssh/.
func releaseTree(p, root string) bool {
	rest, ok := strings.CutPrefix(p, root+"/")
	if !ok {
		return false
	}
	segs := strings.Split(rest, "/")
	switch {
	case segs[0] == "current":
		segs = segs[1:]
	case segs[0] == "releases" && len(segs) >= 2 && segs[1] != "":
		segs = segs[2:]
	default:
		return false
	}
	for _, s := range segs {
		if s == "" || strings.HasPrefix(s, ".") {
			return false
		}
	}
	return true
}

// cleanPath is the rule both root and alias start from: one clean absolute
// path, without variables; a trailing "/" is kept apart.
func cleanPath(d *directive) (string, error) {
	if len(d.words) != 2 {
		return "", refuse(d.line, "%s takes one path", d.words[0])
	}
	p := d.words[1]
	if strings.Contains(p, "$") {
		return "", refuse(d.line, "%s: the path cannot use variables", d.words[0])
	}
	trimmed := p
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		trimmed = strings.TrimSuffix(p, "/")
	}
	if !filepath.IsAbs(p) || filepath.Clean(trimmed) != trimmed {
		return "", refuse(d.line, "%s: %q is not a clean absolute path", d.words[0], p)
	}
	return trimmed, nil
}

// checkTree: root and alias only in the site's release tree.
func checkTree(g *guard, d *directive, anc []*directive) error {
	p, err := cleanPath(d)
	if err != nil {
		return err
	}
	if !releaseTree(p, g.SiteRoot) {
		return refuse(d.line, "%s: %q is outside the site's release tree (%s/current/…)", d.words[0], d.words[1], g.SiteRoot)
	}
	// "location /x { alias …/dir/; }" serves /x../ from …/dir/../: the
	// prefix must end in "/" when the alias does.
	if d.name() == "alias" && strings.HasSuffix(d.words[1], "/") && len(anc) > 0 && anc[len(anc)-1].name() == "location" {
		if prefix, ok := locationPrefix(anc[len(anc)-1]); ok && !strings.HasSuffix(prefix, "/") {
			return refuse(d.line, "alias %q ends in \"/\" but location %q does not: /%s../ would leave the directory", d.words[1], prefix, strings.Trim(prefix, "/"))
		}
	}
	return nil
}

// checkAutoindex: off only (R-10.5). nginx's listing stats every entry
// following symlinks (ngx_de_info is stat(2) on Linux; disable_symlinks
// applies to the files a request opens, not to a listing), so a link the
// tenant plants in a listed folder shows the size and time of whatever it
// names, another site's file included.
func checkAutoindex(_ *guard, d *directive, _ []*directive) error {
	if len(d.words) != 2 || (d.words[1] != "on" && d.words[1] != "off") {
		return refuse(d.line, "autoindex takes on or off")
	}
	if d.words[1] == "on" {
		return refuse(d.line, "autoindex on is not allowed: nginx's listing reads the size and date of whatever a symlink in the folder points to, another site's files included")
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none: this site serves no files"
	}
	return s
}

// pathIn is the root or alias a block sets for itself, "" for none.
func pathIn(b *directive) string {
	for _, c := range b.children {
		if n := c.name(); (n == "root" || n == "alias") && len(c.words) == 2 {
			return strings.TrimSuffix(c.words[1], "/")
		}
	}
	return ""
}

// topPath is a root set at the top of the block, "" for none.
func topPath(g *guard) string {
	p, err := parseConf(g.text)
	if err != nil {
		return ""
	}
	return pathIn(&directive{children: p.top})
}

// target checks a proxy_pass target and says whether it is on this machine
// or a private network.
func (g *guard) target(d *directive) (bool, error) {
	raw := d.words[1]
	if strings.Contains(raw, "$") {
		return false, refuse(d.line, "proxy_pass: the target cannot use variables")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false, refuse(d.line, "proxy_pass: %q is not an http:// or https:// URL with a host", raw)
	}
	host := u.Hostname()
	if g.AppUpstream != "" && host == g.AppUpstream && u.Port() == "" && u.Scheme == "http" {
		return true, nil // the site's own app (SPEC §4.11)
	}
	if strings.EqualFold(host, "unix") || strings.HasPrefix(strings.ToLower(u.Host), "unix:") {
		return false, refuse(d.line, "proxy_pass: a unix: socket is not allowed")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if ps := u.Port(); ps != "" {
		if port, err = strconv.Atoi(ps); err != nil || port < 1 || port > 65535 {
			return false, refuse(d.line, "proxy_pass: port %q", ps)
		}
	}
	var addrs []netip.Addr
	lh := strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case lh == "localhost" || strings.HasSuffix(lh, ".localhost"):
		addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	default:
		a, err := netip.ParseAddr(host)
		if err != nil {
			// A name is checked here but resolved again by nginx at every
			// start and reload, when it may lead elsewhere (this machine's
			// services): only an address is what it says (SPEC §7.3).
			return false, refuse(d.line, "proxy_pass: %q is a host name; use an IP address (or localhost): nginx resolves a name again at every reload, after this check", host)
		}
		addrs = []netip.Addr{a.Unmap()}
	}
	local := false
	for _, a := range addrs {
		local = local || g.localAddr(a.Unmap())
	}
	if local && (slices.Contains(reservedPorts, port) || slices.Contains(g.SystemPorts, port)) {
		return true, refuse(d.line, "proxy_pass: port %d on this machine or a private network is a system service's", port)
	}
	return local, nil
}

var privateNets = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}

func (g *guard) localAddr(a netip.Addr) bool {
	for _, p := range privateNets {
		if p.Contains(a) {
			return true
		}
	}
	return slices.Contains(g.LocalAddrs, a)
}

// checkProxyPass: one target, already checked by allowPass (target).
func checkProxyPass(_ *guard, d *directive, _ []*directive) error {
	if len(d.words) != 2 {
		return refuse(d.line, "proxy_pass takes one URL")
	}
	return nil
}

// checkProxyHeader: towards this machine, Host may only be the request's
// own ($host) or one of the site's names: any other name would reach
// whatever the local service routes by Host.
func checkProxyHeader(g *guard, d *directive, _ []*directive) error {
	if len(d.words) != 3 {
		return refuse(d.line, "proxy_set_header takes a header and a value")
	}
	if !g.local || !strings.EqualFold(d.words[1], "Host") {
		return nil
	}
	v := strings.ToLower(d.words[2])
	if v == "$host" || slices.Contains(g.Domains, v) {
		return nil
	}
	return refuse(d.line, "proxy_set_header Host %q: towards this machine Host is $host or one of the site's names", d.words[2])
}

// checkWith runs both passes over p.
func checkWith(p *parsed, text string, rc RoutingContext) error {
	root := filepath.Clean(rc.SiteRoot)
	if !filepath.IsAbs(root) || root == "/" {
		return fmt.Errorf("routing: site root %q is not a site tree", rc.SiteRoot)
	}
	rc.SiteRoot = root
	g := &guard{RoutingContext: rc, text: text}
	if err := g.allowPass(p.top); err != nil {
		return err
	}
	var walk func(ds []*directive, parent *directive) error
	walk = func(ds []*directive, parent *directive) error {
		for _, d := range ds {
			if parent == nil || parent.name() != "types" {
				if err := checkDirective(d, parent, root); err != nil {
					return err
				}
			}
			if err := walk(d.children, d); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(p.top, nil)
}
