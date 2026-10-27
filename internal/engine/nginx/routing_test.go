package nginx

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Custom routings the matrix renders (and nginx -t checks in containers).
const (
	routingSPA       = "location / {\n    try_files $uri $uri/ /index.html;\n}\n"
	routingStatic404 = "error_page 404 /404.html;\n\nlocation / {\n    try_files $uri $uri/ =404;\n}\n"
	routingSlim      = "# Slim: the front controller is app.php\nlocation / {\n    try_files $uri /app.php$is_args$args;\n}\n"
	routingAPIProxy  = `location / {
    try_files $uri $uri/ /index.php?$args;
}

location /api/ {
    proxy_pass http://127.0.0.1:9000;
    proxy_set_header Host $host;
    add_header X-Api backend;
    if ($request_method = OPTIONS) {
        add_header Access-Control-Allow-Origin "*";
        return 204;
    }
}
`
	routingRedirects = "location = /old { return 301 /new; }\n" +
		"location /promo { add_header X-Promo yes; return 302 /shop; }\n" +
		"rewrite ^/blog/(.*)$ /news/$1 permanent;\n" +
		"location / {\n\ttry_files $uri $uri/ /index.php?$args;\n}"
	routingProxy = `location / {
    proxy_pass http://127.0.0.1:3000;
    proxy_set_header Host $host;
}
location /ws/ {
    proxy_pass http://127.0.0.1:3001;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
`
	routingQuoted = "location / {\n    try_files $uri $uri/ /index.php?$args;\n}\nlocation = /hello.txt {\n    default_type text/plain;\n    return 200 \"first line\n  second line, indented in the string\n\";\n}\n"
)

func words(t *testing.T, text string) [][]string {
	t.Helper()
	p, err := parseConf(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	var out [][]string
	directives(p.top, nil, func(d, _ *directive) { out = append(out, d.words) })
	return out
}

// The tokenizer splits text where nginx does (ngx_conf_read_token).
func TestTokenizerReadsLikeNginx(t *testing.T) {
	for text, want := range map[string][][]string{
		`a b;`:           {{"a", "b"}},
		`"inc"'lude' x;`: nil, // nginx: unexpected "'" after a quote
		`"include" /x;`:  {{"include", "/x"}},
		`a "b c" 'd;e';`: {{"a", "b c", "d;e"}},
		// nginx keeps the backslash of an escape other than \" \' \\ \t \r \n.
		`a \; b;`:                 {{"a", `\;`, "b"}},
		`a b\;c;`:                 {{"a", `b\;c`}},
		"a # c ; d\n b;":          {{"a", "b"}},
		`a b#c;`:                  {{"a", "b#c"}},
		`a ${x}y;`:                {{"a", "${x}y"}},
		`a b}c;`:                  {{"a", "b}c"}},
		`if ($a = "b") { c; }`:    {{"if", "($a", "=", "b", ")"}, {"c"}},
		`a "x\"y" 'p\'q' "\t";`:   {{"a", `x"y`, "p'q", "\t"}},
		`a "\d";`:                 {{"a", `\d`}},
		"l / {\n  m {\n n;\n}\n}": {{"l", "/"}, {"m"}, {"n"}},
	} {
		if want == nil {
			if _, err := parseConf(text); err == nil {
				t.Errorf("%q: parsed, nginx refuses it", text)
			}
			continue
		}
		if got := words(t, text); !reflect.DeepEqual(got, want) {
			t.Errorf("%q:\n got %q\nwant %q", text, got, want)
		}
	}
}

func TestTokenizerRefusesBrokenText(t *testing.T) {
	for _, text := range []string{
		"a", "a b", `a "b;`, "a 'b", `a \`, "location / {", "}", "a; }", ";", "{ a; }",
		"location / { a }", `a "b"c;`,
		strings.Repeat("l {", 9) + strings.Repeat("}", 9),
	} {
		if _, err := parseConf(text); err == nil {
			t.Errorf("%q: parsed", text)
		}
	}
	if _, err := parseConf(strings.Repeat("l {", 8) + strings.Repeat("}", 8)); err != nil {
		t.Errorf("eight levels: %v", err)
	}
}

const root = "/srv/sites/a.example.test"

// rc is the test site's context: its tree, its public directory, a
// service of this machine on 7000.
var rc = RoutingContext{SiteRoot: root, DocRoot: root + "/current", Domains: []string{"a.example.test"}, SystemPorts: []int{7000}}

func TestRoutingRules(t *testing.T) {
	allowed := []string{
		routingSPA, routingStatic404, routingSlim, routingAPIProxy, routingRedirects, routingProxy, routingQuoted,
		"location /media/ { alias " + root + "/current/media/; }",
		"location = /robots.txt { root " + root + "/current; }",
		"location /r/ { alias " + root + "/releases/20261010-1/assets/; }",
		"location /files/ { root " + root + "/current/pub; autoindex off; }",
		"location / { types { text/plain txt log; application/json map; } default_type text/plain; }",
		"location / { if (!-f $request_filename) { return 404; } }",
		"location / { proxy_pass http://127.0.0.1:3000; proxy_set_header Host $host; }",
		"location / { proxy_pass http://localhost:3000; proxy_set_header Host a.example.test; }",
		"location / { proxy_pass https://203.0.113.5; proxy_set_header Host example.org; }",
		"location / { proxy_pass https://[2001:db8::5]:8443; }",
		"location ~ ^/static/(.+)$ { try_files /static/$uri =404; }",
		"location / { try_files $uri $uri/ /index.php?q=$arg_q&$args; }",
		"location / { return 301 https://example.test$request_uri; }",
		"location /x { internal; }",
		"location /api { limit_except GET { deny all; } proxy_pass https://127.0.0.1:8444; }",
		"set $my 1;",
		"location / { if ($http_user_agent ~* bot) { return 403; } try_files $uri =404; }",
		"location ~ \\.json$ { expires 1h; add_header Cache-Control public; }",
		"location / { error_page 404 = @fallback; }\nlocation @fallback { proxy_pass http://127.0.0.1:3000; }",
		"location /files/ { autoindex off; }",
		"location /p { rewrite ^/p/(\\d+)$ /post.php?id=$1 last; }",
	}
	for _, text := range allowed {
		if err := CheckRouting(text, rc); err != nil {
			t.Errorf("refused:\n%s\n%v", text, err)
		}
	}
	refused := map[string]string{
		"include /etc/passwd;":                                             "include",
		"location / { \"include\" /etc/passwd; }":                          "include",
		"location / { Include x; }":                                        "Include",
		"load_module /x.so;":                                               "load_module",
		"access_log /tmp/x;":                                               "access_log",
		"location / { error_log /tmp/x; }":                                 "error_log",
		"location / { rewrite_log on; }":                                   "rewrite_log",
		"log_format x y;":                                                  "log_format",
		"ssl_certificate /etc/x;":                                          "ssl_certificate",
		"location / { proxy_ssl_certificate_key /k; }":                     "proxy_ssl",
		"listen 8080;":                                                     "listen",
		"server_name evil.test;":                                           "server_name",
		"location / { auth_basic off; }":                                   "auth_basic",
		"location / { satisfy any; }":                                      "satisfy",
		"location / { auth_request /x; }":                                  "auth_request",
		"location / { disable_symlinks off; }":                             "disable_symlinks",
		"location / { server_tokens on; }":                                 "server_tokens",
		"location / { more_clear_headers X-Frame-Options; }":               "more_clear",
		"location / { add_header_inherit off; }":                           "add_header_inherit",
		"location ~ \\.php$ { fastcgi_pass unix:/run/php.sock; }":          "fastcgi_pass",
		"location / { fastcgi_cache cg_1; }":                               "fastcgi_cache",
		"location / { proxy_cache cg_1; }":                                 "proxy_cache",
		"location / { proxy_no_cache 1; }":                                 "proxy_no_cache",
		"location / { proxy_store on; }":                                   "proxy_store",
		"location / { proxy_temp_path /tmp; }":                             "temp_path",
		"location / { client_body_temp_path /tmp; }":                       "temp_path",
		"client_max_body_size 10g;":                                        "client_max_body_size",
		"location / { tcp_nodelay off; }":                                  "tcp_nodelay",
		"location / { sendfile off; }":                                     "sendfile",
		"location / { gzip off; }":                                         "gzip",
		"location / { open_file_cache off; }":                              "open_file_cache",
		"location / { keepalive_timeout 0; }":                              "keepalive",
		"location / { perl Mod::handler; }":                                "perl",
		"location / { js_content x.y; }":                                   "js_content",
		"location / { content_by_lua_block { return; } }":                  "content_by_lua_block",
		"location / { set_by_lua $x 1; }":                                  "_by_",
		"location / { stub_status; }":                                      "stub_status",
		"location / { dav_methods PUT; }":                                  "dav",
		"location / { ssi on; }":                                           "ssi",
		"location / { geoip_country /x; }":                                 "geoip",
		"location / { xslt_stylesheet /x; }":                               "xslt",
		"location / { proxy_pass http://unix:/run/cloudground/api.sock; }": "unix",
		"location / { proxy_pass http://$arg_host; }":                      "variables",
		"location / { grpc_pass unix:/run/x.sock; }":                       "grpc_pass",
		"location / { uwsgi_pass UNIX:/x; }":                               "uwsgi_pass",
		"location / { root /etc; }":                                        "outside",
		"location / { root /srv/sites/b.example.test; }":                   "outside",
		"location / { root " + root + "x; }":                               "outside",
		"location / { root html; }":                                        "absolute",
		"location / { root " + root + "/$host; }":                          "variables",
		"location / { alias " + root + "; }":                               "outside",
		"location /x { alias " + root + "/current/dir/; }":                 "/x../",
		"location / { root " + root + "/a/../..; }":                        "..",
		"location / { try_files /../../etc/passwd =404; }":                 "..",
		"location / { try_files /$arg_f =404; }":                           "$arg_f",
		"location / { try_files $request_uri =404; }":                      "$request_uri",
		"location / { try_files /x${cookie_p} =404; }":                     "$cookie_p",
		"location ~ (.*) { try_files /$1 =404; }":                          "$1",
		"location / { rewrite ^ /$arg_p last; }":                           "$arg_p",
		"location / { error_page 404 /$http_x; }":                          "$http_x",
		"set $p $arg_p; location / { try_files /$p =404; }":                "$p",
		"location / { index $arg_i; }":                                     "$arg_i",
		"set $cg_skip_1 0;":                                                "cg_",
		"location / { set $CG_KEY x; }":                                    "cg_",
		"location ~ ^/(?<cg_key>.*)$ { }":                                  "cg_",
		"location ~ ^/(?P<cg_x>.*)$ { }":                                   "cg_",
		"if ($request_uri ~ (?'cg_p'.*)) { }":                              "cg_",
		"location / { merge_slashes off; }":                                "merge_slashes",
		"location / { post_action /x; }":                                   "post_action",
		"location / { return 200 x; }\n\x00":                               "control",
	}
	for text, wantIn := range refused {
		err := CheckRouting(text, rc)
		if err == nil {
			t.Errorf("accepted:\n%s", text)
			continue
		}
		if _, ok := err.(*RoutingError); !ok {
			t.Errorf("%q: %T is not a RoutingError", text, err)
		}
		if !strings.Contains(err.Error(), wantIn) {
			t.Errorf("%q: %q does not mention %q", text, err, wantIn)
		}
		if !strings.HasPrefix(err.Error(), "line ") {
			t.Errorf("%q: %q has no line", text, err)
		}
	}
	if err := CheckRouting(strings.Repeat("#", MaxRoutingBytes+1), rc); err == nil {
		t.Error("an oversized block passed")
	}
	if err := CheckRouting("location / {}", RoutingContext{}); err == nil {
		t.Error("a site without a tree passed")
	}
}

func TestRenderRoutingIndentsAndProtectsHeaders(t *testing.T) {
	sec := []string{"add_header A a always;", "add_header B b always;"}
	render := func(text string) string {
		p, err := parseConf(text)
		if err != nil {
			t.Fatal(err)
		}
		return renderRouting(text, p, sec)
	}
	for text, want := range map[string]string{
		"location / {\n    try_files $uri =404;\n}\n": "    location / {\n        try_files $uri =404;\n    }\n",
		// No trailing line break: one is added, so a comment cannot eat the vhost.
		"location / {}\n# end": "    location / {}\n    # end\n",
		"a;\n\nb;":             "    a;\n\n    b;\n",
		// The security headers go before the closing brace of every block
		// that adds a header, at its depth.
		"location / {\n    add_header X x;\n}\n":                 "    location / {\n        add_header X x;\n        add_header A a always;\n        add_header B b always;\n    }\n",
		"location / { add_header X x; }":                         "    location / { add_header X x; add_header A a always; add_header B b always; }\n",
		"location / {\n  if ($a) {\n    add_header X x;\n  }\n}": "    location / {\n      if ($a) {\n        add_header X x;\n        add_header A a always;\n        add_header B b always;\n      }\n    }\n",
		// A block without one inherits the server level's: nothing added.
		"location / {\n    expires 1h;\n}": "    location / {\n        expires 1h;\n    }\n",
		// A line that continues a quoted string keeps its leading space.
		"return 200 \"a\n  b\";": "    return 200 \"a\n  b\";\n",
		"x a\\\n  b;":            "    x a\\\n  b;\n",
	} {
		if got := render(text); got != want {
			t.Errorf("%q:\n got %q\nwant %q", text, got, want)
		}
	}
}

func TestRoutingView(t *testing.T) {
	in := matrixInput(3, state.SiteLaravel, state.ProfileBalanced, true)
	in.Site.RootPath = root
	view, err := Renderer{}.Routing(in)
	if err != nil {
		t.Fatal(err)
	}
	if view.Default != DefaultRouting(in.Site, true) || view.Current != "" {
		t.Errorf("view of a default site: %+v", view)
	}
	files, _ := Renderer{}.SiteFiles(in)
	whole := files[DefaultSitesDir+"/c3.example.test.conf"]
	if whole != view.Before+renderRouting(view.Default, mustParse(t, view.Default), nil)+view.After {
		t.Error("Before + block + After is not the vhost")
	}
	if !strings.Contains(view.Before, "ssl_certificate") || !strings.Contains(view.After, "fastcgi_pass") {
		t.Error("the panel's parts are not around the block")
	}

	in.Site.Config.Routing = "location / {\r\n    try_files $uri /index.php?$args;\r\n}\r\n"
	view, err = Renderer{}.Routing(in)
	if err != nil {
		t.Fatal(err)
	}
	if view.Current != "location / {\n    try_files $uri /index.php?$args;\n}\n" {
		t.Errorf("current %q", view.Current)
	}

	// A PHP-type site without a socket has no routing to edit.
	in.PHPSocket = ""
	if view, err = (Renderer{}).Routing(in); err != nil || view.Default != "" || view.Current != "" {
		t.Errorf("no-socket view %+v %v", view, err)
	}
}

func TestRebaseRouting(t *testing.T) {
	from, to := "/srv/sites/a.example.test", "/srv/sites/staging.a.example.test"
	in := "# served from " + from + "/current, see " + from + "/current/README\n" +
		"location /m/ { alias " + from + "/current/m/; }\n" +
		"location = /r { root \"" + from + "/current\"; }\n" +
		"location /o/ { alias " + from + "2/current/o/; }\n" +
		"location /t/ { try_files " + from + "/current/x $uri =404; add_header X-Path " + from + "/current; }\n" +
		"location /q/ { alias '" + from + "/current/q/'; }\n"
	want := "# served from " + from + "/current, see " + from + "/current/README\n" +
		"location /m/ { alias " + to + "/current/m/; }\n" +
		"location = /r { root \"" + to + "/current\"; }\n" +
		"location /o/ { alias " + from + "2/current/o/; }\n" +
		"location /t/ { try_files " + from + "/current/x $uri =404; add_header X-Path " + from + "/current; }\n" +
		"location /q/ { alias '" + to + "/current/q/'; }\n"
	if got := RebaseRouting(in, from, to); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	moved := "location /m/ { alias " + to + "/current/m/; }\nlocation = /r { root \"" + to + "/current\"; }\n"
	if err := CheckRouting(RebaseRouting("location /m/ { alias "+from+"/current/m/; }\nlocation = /r { root \""+from+"/current\"; }\n", from, to),
		RoutingContext{SiteRoot: to}); err != nil {
		t.Errorf("the rebased block does not pass for its new tree: %v (%s)", err, moved)
	}
	if RebaseRouting("", from, to) != "" || RebaseRouting(in, "", to) != in || RebaseRouting("location / {", from, to) != "location / {" {
		t.Error("nothing to rebase changed something")
	}
}

// The allowlist (SPEC §7.3): what is not on it is refused with its line,
// and what is on it is held to its own rules.
func TestRoutingAllowlistAndTargets(t *testing.T) {
	pub := RoutingContext{SiteRoot: root, DocRoot: root + "/current/public", Domains: []string{"a.example.test"}}
	resolving := rc
	resolving.LocalAddrs = []netip.Addr{netip.MustParseAddr("203.0.113.7")}
	resolving.Resolve = func(h string) ([]netip.Addr, error) {
		switch h {
		case "inside.example.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "outside.example.test":
			return []netip.Addr{netip.MustParseAddr("198.51.100.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	for _, c := range []struct {
		ctx  RoutingContext
		text string
		want string // "" accepted
	}{
		{rc, "location / {\n    gzip_static on;\n}", `line 2: "gzip_static" is not one`},
		{rc, "location / { more_set_headers x; }", `"more_set_headers" is not one`},
		{rc, "location / { proxy_cache_valid 1m; }", `"proxy_cache_valid" is not one`},
		{rc, "location /;", "location needs a block"},
		{rc, "index index.html { }", "index does not take a block"},
		{rc, "location bad mod x { }", "location takes"},
		{rc, "location ! /x { }", "location modifier"},
		// Proxy targets.
		{rc, "location / { proxy_pass grpc://127.0.0.1:50051; }", "not an http:// or https://"},
		{rc, "location / { proxy_pass http://user@example.org; }", "not an http:// or https://"},
		{rc, "location / { proxy_pass http://127.0.0.1:3306; }", "port 3306"},
		{rc, "location / { proxy_pass http://127.0.0.1; }", "port 80"},
		{rc, "location / { proxy_pass https://127.0.0.2; }", "port 443"},
		{rc, "location / { proxy_pass https://localhost:8443; }", "port 8443"},
		{rc, "location / { proxy_pass http://[::1]:6379; }", "port 6379"},
		{rc, "location / { proxy_pass http://10.1.2.3:5252; }", "port 5252"},
		{rc, "location / { proxy_pass http://192.168.1.1:9080; }", "port 9080"},
		{rc, "location / { proxy_pass http://169.254.169.254; }", "port 80"},
		{rc, "location / { proxy_pass http://172.20.0.1:3306/; }", "port 3306"},
		{rc, "location / { proxy_pass http://127.0.0.1:7000; }", "port 7000"},
		{rc, "location / { proxy_pass http://203.0.113.5:3306; }", ""},
		// A host name is resolved again by nginx at every reload: only an
		// address (or localhost) is what was checked (R-10.5).
		{rc, "location / { proxy_pass http://example.org:3306; }", "is a host name"},
		{rc, "location / { proxy_pass https://example.org; }", "is a host name"},
		{rc, "location / { proxy_pass http://127.0.0.1:3000/app/; }", ""},
		{resolving, "location / { proxy_pass https://203.0.113.7; }", "port 443"},
		{resolving, "location / { proxy_pass http://inside.example.test:6379; }", "is a host name"},
		{resolving, "location / { proxy_pass http://outside.example.test:6379; }", "is a host name"},
		{resolving, "location / { proxy_pass http://nowhere.example.test; }", "is a host name"},
		// Host towards this machine.
		{rc, "location / { proxy_pass http://127.0.0.1:3000; proxy_set_header Host other.example.test; }", "proxy_set_header Host"},
		{rc, "location /a { proxy_set_header Host other.example.test; }\nlocation /b { proxy_pass http://127.0.0.1:3000; }", "proxy_set_header Host"},
		{rc, "location / { proxy_pass http://127.0.0.1:3000; proxy_set_header X-Host other.example.test; }", ""},
		// The release tree.
		{rc, "location / { root " + root + "; }", "release tree"},
		{rc, "location / { root " + root + "/shared/uploads; }", "release tree"},
		{rc, "location / { root " + root + "/logs; }", "release tree"},
		{rc, "location / { root " + root + "/tmp; }", "release tree"},
		{rc, "location / { root " + root + "/src; }", "release tree"},
		{rc, "location / { root " + root + "/.ssh; }", "release tree"},
		{rc, "location / { root " + root + "/current/.git; }", "release tree"},
		{rc, "location / { root " + root + "/releases; }", "release tree"},
		{rc, "location / { root " + root + "/releases/r1/public; }", ""},
		// Listings only of the public part.
		// No listing at all (R-10.5): nginx stats through a symlink in it.
		{pub, "location /f/ { autoindex on; }", "autoindex on is not allowed"},
		{pub, "location /f/ { root " + root + "/current/public/f; autoindex on; }", "autoindex on is not allowed"},
		{pub, "location /f/ { root " + root + "/current; autoindex on; }", "autoindex on is not allowed"},
		{RoutingContext{SiteRoot: root}, "location /f/ { autoindex on; }", "autoindex on is not allowed"},
		{pub, "location /f/ { autoindex off; }", ""},
		// Conditions.
		{rc, "location / { if (-f /etc/passwd) { return 403; } }", "file test"},
		{rc, "location / { if (-d $request_filename) { return 403; } }", ""},
		{rc, "location / { if ($http_user_agent ~* \"bot|crawl\") { return 403; } }", ""},
		{rc, "location / { if ($slow) { return 503; } }", ""},
		{rc, "location / { if (foo) { return 403; } }", "not a condition"},
		{rc, "location / { if $a { return 403; } }", "parentheses"},
		// Local pages only.
		{rc, "error_page 404 https://evil.example/;", "path of this site"},
		{rc, "error_page 404 //evil.example/x;", "path of this site"},
		{rc, "error_page 404 =200 /index.html;", ""},
		{rc, "error_page 4o4 /x;", "status code"},
		{rc, "location / { try_files $uri https://evil.example/; }", "fallback"},
		{rc, "location / { try_files $uri; }", "fallback"},
		{rc, "location / { return 301; }", ""},
		{rc, "location / { return 999 x; }", "status code"},
		{rc, "location / { rewrite ^ /x sideways; }", "rewrite flag"},
		// types holds media types only.
		{rc, "location / { types { include /etc/passwd; } }", "media type"},
		{rc, "location / { types { text/html html; image/svg+xml svg svgz; } }", ""},
	} {
		err := CheckRouting(c.text, c.ctx)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("refused %q: %v", c.text, err)
		case c.want != "" && err == nil:
			t.Errorf("accepted %q", c.text)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%q: %q does not say %q", c.text, err, c.want)
		}
	}
}

func mustParse(t *testing.T, text string) *parsed {
	t.Helper()
	p, err := parseConf(text)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A managed app's block may proxy to its own upstream, and to no other
// (SPEC §4.11).
func TestAppUpstreamRouting(t *testing.T) {
	app := rc
	app.DocRoot, app.AppUpstream = "", "cg_app_7"
	if err := CheckRouting("location / { proxy_pass http://cg_app_7; proxy_set_header Host $host; }", app); err != nil {
		t.Fatalf("the own app refused: %v", err)
	}
	for _, text := range []string{
		"location / { proxy_pass http://cg_app_8; }",
		"location / { proxy_pass http://cg_app_7:8080; }",
		"location / { proxy_pass http://cg_app_7; proxy_set_header Host evil.example.org; }",
	} {
		if err := CheckRouting(text, app); err == nil {
			t.Errorf("taken: %s", text)
		}
	}
	if err := CheckRouting("location / { proxy_pass http://cg_app_7; }", rc); err == nil {
		t.Error("another site reached an app's upstream")
	}
}
