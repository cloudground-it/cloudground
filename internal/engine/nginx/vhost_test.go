package nginx

import (
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
