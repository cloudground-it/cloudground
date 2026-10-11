package nginx

import (
	"regexp"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The product claims of SPEC §4, checked on every case of the matrix by
// reading the rendered vhost back with the routing tokenizer: the matrix
// pins the bytes, these say why they matter.

func vhostOf(t *testing.T, files map[string]string, domain string) string {
	t.Helper()
	v, ok := files[DefaultSitesDir+"/"+domain+".conf"]
	if !ok {
		t.Fatalf("no vhost for %s in %v", domain, files)
	}
	return v
}

// directives walks every directive, calling fn with the block it is in.
func directives(ds []*directive, parent *directive, fn func(d, parent *directive)) {
	for _, d := range ds {
		fn(d, parent)
		directives(d.children, d, fn)
	}
}

func TestMatrixClaims(t *testing.T) {
	for _, c := range matrixCases() {
		t.Run(c.name, func(t *testing.T) {
			files, err := Renderer{}.SiteFiles(c.in)
			if err != nil {
				t.Fatal(err)
			}
			text := vhostOf(t, files, c.in.Site.Domain)
			p, err := parseConf(text)
			if err != nil {
				t.Fatalf("the vhost does not read back: %v", err)
			}
			hsts := c.in.Site.Config.HSTS && c.in.CertAvailable
			directives(p.top, nil, func(d, parent *directive) {
				line := strings.Join(d.words, " ")
				// nginx inherits add_header only into a level that declares
				// none: every block that adds one repeats the security set.
				if d.block {
					var adds []string
					for _, ch := range d.children {
						if ch.name() == "add_header" {
							adds = append(adds, strings.Join(ch.words, " "))
						}
					}
					if len(adds) > 0 {
						want := []string{"add_header X-Content-Type-Options nosniff always", "add_header X-Frame-Options SAMEORIGIN always"}
						if hsts {
							want = append(want, `add_header Strict-Transport-Security max-age=31536000 always`)
						}
						for _, w := range want {
							if !strings.Contains(strings.Join(adds, "\n"), w) {
								t.Errorf("%s at line %d adds headers but not %q", d.words[0], d.line, w)
							}
						}
					}
				}
				switch d.name() {
				case "fastcgi_ignore_headers":
					// A response that sets a cookie must never be stored.
					for _, w := range d.words {
						if strings.EqualFold(w, "Set-Cookie") {
							t.Errorf("Set-Cookie ignored: %s", line)
						}
					}
				case "add_header":
					if strings.EqualFold(d.words[1], "Strict-Transport-Security") && !hsts {
						t.Errorf("HSTS without a real certificate and the site's switch: %s", line)
					}
				case "fastcgi_cache":
					if !c.in.Policy.Enabled {
						t.Errorf("a page cache without a policy: %s", line)
					}
				case "listen":
					if strings.Contains(line, "reuseport") {
						t.Errorf("reuseport in a vhost: the default server holds the only one (%s)", line)
					}
				case "ssl_certificate":
					want := c.in.FallbackCert
					if c.in.CertAvailable {
						want = c.in.CertFullchain
					}
					if d.words[1] != want {
						t.Errorf("certificate %s, want %s", d.words[1], want)
					}
				case "proxy_pass", "fastcgi_pass":
					if c.in.Site.Type == state.SiteStatic {
						t.Errorf("a static site passes requests on: %s", line)
					}
				}
			})
			if c.in.Policy.Enabled && !strings.Contains(text, "fastcgi_cache cg_") && c.in.PHPSocket != "" {
				t.Error("the policy caches but the PHP location has no cache")
			}
			if c.in.BasicAuthFile != "" && !strings.Contains(text, "auth_basic_user_file "+c.in.BasicAuthFile+";") {
				t.Error("staging without its password")
			}
		})
	}
}

func TestRefusesWhatWouldInjectConfiguration(t *testing.T) {
	base := matrixInput(1, state.SiteWordPress, state.ProfileBalanced, true)
	for name, mutate := range map[string]func(*engine.Input){
		"domain": func(in *engine.Input) { in.Site.Domain = "evil.com; }" },
		"alias":  func(in *engine.Input) { in.Site.Aliases = []string{"ok.example.test", "a b"} },
		"upstream": func(in *engine.Input) {
			in.Site.Type = state.SiteProxy
			in.Site.Config.ProxyUpstream = "http://127.0.0.1/;error_log /x"
		},
		"routing": func(in *engine.Input) { in.Site.Config.Routing = "location / { include /etc/shadow; }" },
	} {
		in := base
		mutate(&in)
		if _, err := (Renderer{}).SiteFiles(in); err == nil {
			t.Errorf("%s: rendered", name)
		}
		if _, err := (Renderer{}).Routing(in); err == nil {
			t.Errorf("%s: routing view rendered", name)
		}
	}
}

func TestOutputDirectories(t *testing.T) {
	in := matrixInput(9, state.SitePHP, state.ProfileBalanced, true)
	files, err := Renderer{SitesDir: "/x/sites", ConfDir: "/x/conf"}.SiteFiles(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/x/sites/c9.example.test.conf", "/x/conf/cloudground-cache-9.conf"} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing %s in %v", p, files)
		}
	}
	if _, ok := (Renderer{ConfDir: "/x/conf"}).GlobalFiles()["/x/conf/cloudground-global.conf"]; !ok {
		t.Error("global file not in ConfDir")
	}
}

// SPEC §4.4: every site with a root answers 404 for leftovers, from a
// regex location that comes before every other regex location.
func TestLeftoversAreNotServed(t *testing.T) {
	re := regexp.MustCompile("(?i)" + leftoverRegex)
	for _, path := range []string{
		"/wp-config.php.bak", "/wp-config.php.old", "/wp-config.php.save", "/wp-config.php~",
		"/wp-config.php-backup", "/WP-CONFIG.PHP.BAK", "/blog/wp-config.php.orig",
		"/index.php.bak", "/a/b/style.css.old", "/notes.txt.orig", "/x.swp", "/x.swo", "/readme~",
		"/dump.sql", "/dump.SQL", "/db/site.sql.gz", "/site.sql.bz2", "/site.sql.xz", "/site.sql.zst", "/site.sql.zip",
		"/backup.tar", "/backup.tar.gz", "/backup.TGZ", "/backup.tar.bz2", "/backup.tbz2", "/backup.tar.xz",
		"/backup.txz", "/backup.tar.zst", "/site.zip", "/site.7z", "/site.rar",
	} {
		if !re.MatchString(path) {
			t.Errorf("%s is served", path)
		}
	}
	for _, path := range []string{
		"/", "/index.php", "/wp-config.php", "/wp-config-sample.php", "/wp-login.php",
		"/wp-content/uploads/2026/10/photo.jpg", "/style.css", "/app.js.gz",
		"/wp-content/uploads/files/manual.zip", "/downloads/release.tar.gz",
		"/sql", "/old/", "/bak/page", "/backup", "/my.sqlite", "/zip",
	} {
		if re.MatchString(path) {
			t.Errorf("%s is refused", path)
		}
	}

	for _, c := range matrixCases() {
		files, err := Renderer{}.SiteFiles(c.in)
		if err != nil {
			t.Fatal(err)
		}
		p, err := parseConf(vhostOf(t, files, c.in.Site.Domain))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var regexes []*directive
		directives(p.top, nil, func(d, parent *directive) {
			if d.name() == "location" && len(d.words) > 2 && strings.HasPrefix(d.words[1], "~") {
				regexes = append(regexes, d)
			}
		})
		if c.in.Site.Type == state.SiteProxy {
			for _, d := range regexes {
				if d.words[2] == leftoverRegex {
					t.Errorf("%s: a proxy site has the leftovers location", c.name)
				}
			}
			continue
		}
		// The hidden-files location, then this one: first among the rest.
		if len(regexes) < 2 || regexes[1].words[1] != "~*" || regexes[1].words[2] != leftoverRegex {
			t.Errorf("%s: the leftovers location is not the second regex location", c.name)
			continue
		}
		body := regexes[1].children
		if len(body) != 1 || strings.Join(body[0].words, " ") != "return 404" {
			t.Errorf("%s: the leftovers location does not answer 404", c.name)
		}
	}
}

// #26: a staging copy takes no protected paths, and a path or file that is
// not plain is refused before anything is rendered.
func TestProtectedPathsRefused(t *testing.T) {
	in := matrixInput(900, state.SiteWordPress, state.ProfileBalanced, true)
	ok := engine.ProtectedPath{Path: "/wp-admin", File: "/etc/nginx/cloudground-auth/x.htpasswd"}
	for name, set := range map[string]func(*engine.Input){
		"staging": func(in *engine.Input) {
			in.BasicAuthFile = "/etc/nginx/cloudground-auth/s.htpasswd"
			in.ProtectedPaths = []engine.ProtectedPath{ok}
		},
		"path":          func(in *engine.Input) { in.ProtectedPaths = []engine.ProtectedPath{{Path: "/a;b", File: ok.File}} },
		"root":          func(in *engine.Input) { in.ProtectedPaths = []engine.ProtectedPath{{Path: "/", File: ok.File}} },
		"relative file": func(in *engine.Input) { in.ProtectedPaths = []engine.ProtectedPath{{Path: "/a", File: "x.htpasswd"}} },
		"file syntax":   func(in *engine.Input) { in.ProtectedPaths = []engine.ProtectedPath{{Path: "/a", File: "/x;y"}} },
	} {
		c := in
		set(&c)
		if _, err := (Renderer{}).SiteFiles(c); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}
