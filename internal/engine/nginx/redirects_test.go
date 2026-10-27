package nginx

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

func redirectInput(r state.SiteRedirects) (files map[string]string, err error) {
	c := matrixInput(900, state.SiteWordPress, state.ProfileBalanced, true)
	c.Site.Aliases = []string{"www." + c.Site.Domain}
	c.Site.Config.Redirects = r
	return Renderer{}.SiteFiles(c)
}

// A redirect is a location or an `if` the vhost reads back as such, and its
// text never reaches a place where it could end a directive.
func TestRedirectsRender(t *testing.T) {
	domain := "c900.example.test"
	out, err := redirectInput(state.SiteRedirects{
		Canonical: "www." + domain,
		Rules: []state.RedirectRule{
			{Match: "exact", From: "/old", To: "/new", Status: 301},
			{Match: "prefix", From: "/blog", To: "https://news.example.org/p", Status: 302},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := vhostOf(t, out, domain)
	p, err := parseConf(text)
	if err != nil {
		t.Fatal(err)
	}
	var locs []string
	var canonical, plain, port80 []string
	directives(p.top, nil, func(d, parent *directive) {
		line := strings.Join(d.words, " ")
		switch {
		case d.name() == "location" && parent != nil && parent.name() == "server":
			locs = append(locs, line)
		case d.name() == "if":
			canonical = append(canonical, line)
		case d.name() == "return" && parent != nil && parent.name() == "location" && len(d.words) > 2 && strings.HasPrefix(d.words[2], "https://$host"):
			plain = append(plain, line)
		case d.name() == "return" && len(d.words) > 2 && d.words[2] == "https://www."+domain+"$request_uri":
			port80 = append(port80, line)
		}
	})
	for _, want := range []string{"location = /old", "location = /blog", "location ^~ /blog/"} {
		if !contains(locs, want) {
			t.Errorf("no %q among %v", want, locs)
		}
	}
	if !contains(canonical, `if ($host != www.`+domain+` )`) {
		t.Errorf("no canonical if among %v", canonical)
	}
	if len(plain) != 0 {
		t.Errorf("port 80 still sends each name to itself: %v", plain)
	}
	if len(port80) != 2 {
		t.Errorf("want the canonical redirect on port 80 and in the if, got %v", port80)
	}
	if !strings.Contains(text, `return 301 "/new$is_args$args";`) ||
		!strings.Contains(text, `rewrite "^/blog/(.*)$" "https://news.example.org/p/$1" redirect;`) {
		t.Errorf("the rules are not rendered as expected:\n%s", text)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// What the API would have refused is refused here too, at every render.
func TestRedirectsRefusedAtRender(t *testing.T) {
	cases := map[string]state.SiteRedirects{
		"injection in the path":   {Rules: []state.RedirectRule{{Match: "exact", From: "/a; return 200 'x'", To: "/b", Status: 301}}},
		"injection in the target": {Rules: []state.RedirectRule{{Match: "exact", From: "/a", To: "/b\"; return 200; \"", Status: 301}}},
		"variable in the target":  {Rules: []state.RedirectRule{{Match: "exact", From: "/a", To: "/$host", Status: 301}}},
		"canonical not a name":    {Canonical: "evil.test; x"},
	}
	for name, r := range cases {
		if _, err := redirectInput(r); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

// A routing block that already has a rule's location is refused with a
// reason, not left for nginx -t to refuse the whole file.
func TestRedirectsConflictWithRouting(t *testing.T) {
	c := matrixInput(901, state.SiteWordPress, state.ProfileBalanced, true)
	c.Site.Config.Routing = "location = /old {\n    return 404;\n}\nlocation / {\n    try_files $uri $uri/ /index.php?$args;\n}\n"
	c.Site.Config.Redirects = state.SiteRedirects{Rules: []state.RedirectRule{{Match: "exact", From: "/old", To: "/new", Status: 301}}}
	_, err := Renderer{}.SiteFiles(c)
	if err == nil || !strings.Contains(err.Error(), "location = /old") {
		t.Fatalf("err = %v", err)
	}
	c.Site.Config.Redirects.Rules[0].From = "/other"
	if _, err := (Renderer{}).SiteFiles(c); err != nil {
		t.Fatal(err)
	}
}
