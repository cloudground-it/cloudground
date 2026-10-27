package nginx

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's routing block (SPEC §7): the locations a user may edit, checked
// against the rules of §7.3 before it is stored and again at every render.

// MaxRoutingBytes bounds a routing block (SPEC §7.1).
const MaxRoutingBytes = 32 << 10

// RoutingError is a routing block refused by a rule; its text is safe to
// show the administrator who wrote it.
type RoutingError struct{ msg string }

func (e *RoutingError) Error() string { return e.msg }

func refuse(line int, format string, args ...any) error {
	return &RoutingError{fmt.Sprintf("line %d: ", line) + fmt.Sprintf(format, args...)}
}

// NormalizeRouting is the block as it is stored: CRLF line ends as LF.
func NormalizeRouting(text string) string {
	return strings.ReplaceAll(text, "\r\n", "\n")
}

// DefaultRouting is the routing a site of this kind has until an
// administrator edits it (SPEC §4); "" for a PHP-type site without a PHP
// socket, which has none. The proxy upstream must already be normalised.
func DefaultRouting(s state.Site, hasPHP bool) string {
	switch {
	case s.Type.IsApp():
		return proxyRouting("http://" + runtimes.UpstreamName(s.ID))
	case s.Type == state.SiteProxy:
		return proxyRouting(s.Config.ProxyUpstream)
	case s.Type == state.SiteStatic:
		return "location / {\n    try_files $uri $uri/ /index.html =404;\n}\n"
	case hasPHP:
		return "location / {\n    try_files $uri $uri/ /index.php?$args;\n}\n"
	}
	return ""
}

// proxyRouting is the default block of a site that proxies everything.
func proxyRouting(target string) string {
	return "location / {\n" +
		"    proxy_pass " + target + ";\n" +
		"    proxy_http_version 1.1;\n" +
		"    proxy_set_header Host $host;\n" +
		"    proxy_set_header X-Real-IP $remote_addr;\n" +
		"    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n" +
		"    proxy_set_header X-Forwarded-Proto $scheme;\n" +
		"    proxy_set_header Upgrade $http_upgrade;\n" +
		"    proxy_set_header Connection \"upgrade\";\n" +
		"    proxy_read_timeout 60s;\n" +
		"}\n"
}

// RebaseRouting is a routing block moved with its site from one tree to
// another (a staging copy): every path naming the old tree names the new
// one. What comes out is checked again when it is rendered.
// Only the path arguments of root and alias are rewritten, read with the
// tokenizer: the old tree's name elsewhere (a comment, a header, a longer
// word) is left alone. A block that does not parse is returned as it is.
func RebaseRouting(text, from, to string) string {
	from, to = strings.TrimSuffix(from, "/"), strings.TrimSuffix(to, "/")
	if text == "" || from == "" || from == to {
		return text
	}
	p, err := parseConf(text)
	if err != nil {
		return text
	}
	type edit struct {
		span [2]int
		with string
	}
	var edits []edit
	var walk func(ds []*directive)
	walk = func(ds []*directive) {
		for _, d := range ds {
			if n := d.name(); (n == "root" || n == "alias") && len(d.words) == 2 {
				raw := text[d.spans[1][0]:d.spans[1][1]]
				if v := d.words[1]; (v == from || strings.HasPrefix(v, from+"/")) && strings.HasPrefix(raw, from) {
					edits = append(edits, edit{d.spans[1], to + raw[len(from):]})
				}
			}
			walk(d.children)
		}
	}
	walk(p.top)
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		text = text[:e.span[0]] + e.with + text[e.span[1]:]
	}
	return text
}

// CheckRouting applies the rules of SPEC §7.1–7.3 to a routing block.
func CheckRouting(text string, rc RoutingContext) error {
	_, err := checkRouting(NormalizeRouting(text), rc)
	return err
}

func checkRouting(text string, rc RoutingContext) (*parsed, error) {
	if len(text) > MaxRoutingBytes {
		return nil, &RoutingError{fmt.Sprintf("the routing is longer than %d KiB", MaxRoutingBytes>>10)}
	}
	line := 1
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\n' {
			line++
		} else if (c < 0x20 && c != '\t') || c == 0x7f {
			return nil, refuse(line, "control character %q", c)
		}
	}
	p, err := parseConf(text)
	if err != nil {
		if se, ok := err.(*syntaxError); ok {
			return nil, &RoutingError{se.Error()}
		}
		return nil, err
	}
	if err := checkWith(p, text, rc); err != nil {
		return nil, err
	}
	return p, nil
}

// Refused directive names (SPEC §7.3), with why.
var (
	refusedNames = map[string]string{}
	refusedWhy   = map[string]string{
		"files":    "reads or loads files when nginx starts",
		"logs":     "logging is the panel's",
		"identity": "the server's names, ports and protocols are the panel's",
		"auth":     "the staging password is the panel's",
		"headers":  "the security headers and symlink protection are the panel's",
		"php":      "the PHP-FPM link and the page cache are the panel's",
		"limits":   "the site's limits are the panel's",
		"tuning":   "performance settings are measured and set by the panel",
		"scripts":  "scripting modules are not allowed",
		"server":   "exposes the server or writes to the site's files",
	}
)

func init() {
	for why, names := range map[string][]string{
		"files":    {"include", "load_module"},
		"logs":     {"log_format", "log_subrequest", "open_log_file_cache"},
		"identity": {"server", "listen", "server_name", "http", "stream", "mail", "events", "upstream", "map", "geo", "split_clients", "http2", "http3", "user", "env", "daemon", "pid", "master_process", "worker_processes"},
		"auth":     {"satisfy"},
		"headers":  {"more_set_headers", "more_clear_headers", "more_set_input_headers", "more_clear_input_headers", "add_header_inherit", "server_tokens", "disable_symlinks", "merge_slashes"},
		"php":      {"client_body_in_file_only"},
		"limits":   {"client_max_body_size", "client_body_timeout", "client_header_timeout", "client_body_buffer_size", "large_client_header_buffers"},
		"tuning":   {"tcp_nodelay", "tcp_nopush", "output_buffers", "postpone_output", "reset_timedout_connection"},
		"server":   {"stub_status", "status", "api", "dav_methods", "dav_access", "create_full_put_path", "min_delete_depth", "post_action"},
	} {
		for _, n := range names {
			refusedNames[n] = why
		}
	}
}

// refusedPrefixes are matched in order; the first that matches decides.
var refusedPrefixes = []struct{ prefix, why string }{
	{"quic_", "identity"}, {"auth_", "auth"}, {"fastcgi_", "php"},
	{"sendfile", "tuning"}, {"aio", "tuning"}, {"directio", "tuning"}, {"open_file_cache", "tuning"},
	{"gzip", "tuning"}, {"gunzip", "tuning"}, {"brotli", "tuning"}, {"zstd", "tuning"},
	{"keepalive_", "tuning"}, {"lingering_", "tuning"},
	{"perl", "scripts"}, {"js_", "scripts"}, {"njs", "scripts"}, {"lua", "scripts"},
	{"mruby", "scripts"}, {"wasm", "scripts"}, {"python", "scripts"},
	{"vhost_traffic_status", "server"}, {"dav_ext_", "server"}, {"ssi", "server"},
	{"geoip", "files"}, {"xslt", "files"},
}

// refusedReason says why a directive name is refused, "" if it is not.
func refusedReason(n string) string {
	if why, ok := refusedNames[n]; ok {
		return why
	}
	switch {
	case n == "ssl" || strings.Contains(n, "ssl_"):
		return "files"
	case strings.HasSuffix(n, "_log"):
		return "logs"
	case strings.HasSuffix(n, "_temp_path"):
		return "php"
	case strings.Contains(n, "_by_"):
		return "scripts"
	}
	for _, up := range []string{"proxy_", "uwsgi_", "scgi_"} {
		if strings.HasPrefix(n, up) && (strings.Contains(n, "cache") || strings.Contains(n, "store")) {
			return "php"
		}
	}
	for _, p := range refusedPrefixes {
		if strings.HasPrefix(n, p.prefix) {
			return p.why
		}
	}
	return ""
}

var passNames = map[string]bool{"proxy_pass": true, "grpc_pass": true, "uwsgi_pass": true, "scgi_pass": true, "memcached_pass": true}

// pathArgs says which arguments of a directive name a file or a URI that
// nginx maps to one (SPEC §7.3, "paths"); nil for a directive that has none.
func pathArgs(name string, args []string) []int {
	all := func() []int {
		out := make([]int, len(args))
		for i := range args {
			out[i] = i
		}
		return out
	}
	switch name {
	case "root", "alias", "try_files", "index":
		return all()
	case "error_page":
		if len(args) > 0 {
			return []int{len(args) - 1}
		}
	case "rewrite":
		if len(args) > 1 {
			return []int{1}
		}
	case "mirror", "add_before_body", "add_after_body":
		if len(args) > 0 {
			return []int{0}
		}
	}
	return nil
}

// pathVars are the variables a path may use before its query: nginx's
// normalised URI and the root it maps to.
var pathVars = map[string]bool{"uri": true, "document_uri": true, "document_root": true, "realpath_root": true}

func checkDirective(d *directive, parent *directive, root string) error {
	name := d.name()
	args := d.words[1:]
	if why := refusedReason(name); why != "" {
		return refuse(d.line, "%q is not allowed in a site's routing: %s", d.words[0], refusedWhy[why])
	}
	for _, w := range d.words {
		lw := strings.ToLower(w)
		if hasDotDot(w) {
			return refuse(d.line, "%q: a \"..\" path segment is not allowed", w)
		}
		for _, cap := range []string{"(?<cg_", "(?p<cg_", "(?'cg_"} {
			if strings.Contains(lw, cap) {
				return refuse(d.line, "%q: captures named cg_* are the panel's", w)
			}
		}
	}
	if name == "set" && len(args) > 0 {
		v := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(args[0], "$"), "{"), "}"))
		if strings.HasPrefix(v, "cg_") {
			return refuse(d.line, "%q: the $cg_* variables are the panel's (the cache and its bypass)", args[0])
		}
	}
	if passNames[name] {
		for _, a := range args {
			if strings.Contains(a, "$") {
				return refuse(d.line, "%s: the target cannot use variables", d.words[0])
			}
			if strings.Contains(strings.ToLower(a), "unix:") {
				return refuse(d.line, "%s: a unix: socket is not allowed (the panel's API, the agent and the sites' PHP listen on them)", d.words[0])
			}
		}
	}
	if name == "root" || name == "alias" {
		if err := checkTreePath(d, parent, root); err != nil {
			return err
		}
	}
	for _, i := range pathArgs(name, args) {
		if bad := badPathVar(args[i], name == "rewrite"); bad != "" {
			return refuse(d.line, "%s: $%s cannot be used in a path (only $uri, $document_uri, $document_root, $realpath_root, and a rewrite's own captures)", d.words[0], bad)
		}
	}
	return nil
}

// checkTreePath is the rule for root and alias: one clean absolute path,
// without variables, in the site's tree.
func checkTreePath(d *directive, parent *directive, root string) error {
	name := d.name()
	if len(d.words) != 2 {
		return refuse(d.line, "%s takes one path", d.words[0])
	}
	p := d.words[1]
	if strings.Contains(p, "$") {
		return refuse(d.line, "%s: the path cannot use variables", d.words[0])
	}
	trimmed := p
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		trimmed = strings.TrimSuffix(p, "/")
	}
	if !filepath.IsAbs(p) || filepath.Clean(trimmed) != trimmed {
		return refuse(d.line, "%s: %q is not a clean absolute path", d.words[0], p)
	}
	inside := strings.HasPrefix(trimmed, root+"/")
	if !inside && !(name == "root" && trimmed == root) {
		return refuse(d.line, "%s: %q is outside the site's tree (%s)", d.words[0], p, root)
	}
	// "location /x { alias …/dir/; }" serves /x../ from …/dir/../: the
	// prefix must end in "/" when the alias does.
	if name == "alias" && strings.HasSuffix(p, "/") && parent != nil && parent.name() == "location" {
		if prefix, ok := locationPrefix(parent); ok && !strings.HasSuffix(prefix, "/") {
			return refuse(d.line, "alias %q ends in \"/\" but location %q does not: /%s../ would leave the directory", p, prefix, strings.Trim(prefix, "/"))
		}
	}
	return nil
}

// locationPrefix is a prefix location's path ("location /x", "location ^~
// /x"); false for a regex, exact or named location.
func locationPrefix(loc *directive) (string, bool) {
	args := loc.words[1:]
	switch {
	case len(args) == 1 && !strings.HasPrefix(args[0], "@") && !strings.HasPrefix(args[0], "~") && !strings.HasPrefix(args[0], "="):
		return strings.TrimPrefix(args[0], "^~"), true
	case len(args) == 2 && args[0] == "^~":
		return args[1], true
	}
	return "", false
}

func hasDotDot(w string) bool {
	for _, seg := range strings.FieldsFunc(w, func(r rune) bool { return r == '/' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// badPathVar returns the first variable, in the path part of w (before a
// "?" or $is_args), that a path may not use; "" if there is none.
func badPathVar(w string, captures bool) string {
	for i := 0; i < len(w); i++ {
		if w[i] == '?' {
			return ""
		}
		if w[i] != '$' {
			continue
		}
		name, n := varName(w[i+1:])
		i += n
		if name == "" {
			continue
		}
		ln := strings.ToLower(name)
		if ln == "is_args" {
			return ""
		}
		if pathVars[ln] || (captures && len(ln) == 1 && ln[0] >= '1' && ln[0] <= '9') {
			continue
		}
		return name
	}
	return ""
}

// varName reads a variable name after "$": "name" or "{name}"; n is how
// many bytes it took.
func varName(s string) (string, int) {
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return s[1:], len(s)
		}
		return s[1:end], end + 1
	}
	n := 0
	for n < len(s) && (s[n] == '_' || s[n] >= '0' && s[n] <= '9' || s[n] >= 'a' && s[n] <= 'z' || s[n] >= 'A' && s[n] <= 'Z') {
		n++
	}
	return s[:n], n
}

// renderRouting is the block as it goes in the vhost (SPEC §7.1, §7.4):
// the security headers added to every location or if block that adds
// headers of its own, each line indented four spaces except the lines
// that continue a word, and a line break at the end.
func renderRouting(text string, p *parsed, sec []string) string {
	type insert struct {
		at   int
		text string
	}
	var ins []insert
	var walk func(ds []*directive)
	walk = func(ds []*directive) {
		for _, d := range ds {
			if !d.block {
				continue
			}
			walk(d.children)
			if n := d.name(); n != "location" && n != "if" {
				continue
			}
			adds := false
			for _, c := range d.children {
				adds = adds || (!c.block && c.name() == "add_header")
			}
			if !adds {
				continue
			}
			lineStart := strings.LastIndexByte(text[:d.closeAt], '\n') + 1
			if lead := text[lineStart:d.closeAt]; strings.TrimLeft(lead, " \t") == "" && !p.continued[lineStart] {
				// At the indent of the block's last directive when that one
				// starts its line, else one level in from the brace.
				indent := lead + "    "
				last := d.children[len(d.children)-1]
				ls := strings.LastIndexByte(text[:last.at], '\n') + 1
				if pre := text[ls:last.at]; strings.TrimLeft(pre, " \t") == "" && !p.continued[ls] {
					indent = pre
				}
				var b strings.Builder
				for _, h := range sec {
					b.WriteString(indent + h + "\n")
				}
				ins = append(ins, insert{lineStart, b.String()})
			} else {
				ins = append(ins, insert{d.closeAt, strings.Join(sec, " ") + " "})
			}
		}
	}
	walk(p.top)
	sort.Slice(ins, func(i, j int) bool { return ins[i].at > ins[j].at })
	continued := p.continued
	for _, in := range ins {
		text = text[:in.at] + in.text + text[in.at:]
		shifted := map[int]bool{}
		for off := range continued {
			if off >= in.at {
				off += len(in.text)
			}
			shifted[off] = true
		}
		continued = shifted
	}

	var b strings.Builder
	start := 0
	for start < len(text) {
		end := strings.IndexByte(text[start:], '\n')
		lineText := text[start:]
		if end >= 0 {
			lineText = text[start : start+end]
		}
		if lineText != "" && !continued[start] {
			b.WriteString("    ")
		}
		b.WriteString(lineText)
		b.WriteByte('\n')
		if end < 0 {
			break
		}
		start += end + 1
	}
	return b.String()
}
