package nginx

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The tokenizer reads any text without panicking, and what it reads it can
// walk.
func FuzzParseConf(f *testing.F) {
	for _, s := range []string{routingQuoted, "a b;", `a "b c" 'd;e';`, "l / {\n m {\n n;\n}\n}", `a \`, "}", strings.Repeat("l {", 9)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		p, err := parseConf(text)
		if err != nil {
			return
		}
		directives(p.top, nil, func(d, _ *directive) {
			if len(d.words) == 0 {
				t.Fatalf("%q: a directive without words", text)
			}
		})
	})
}

// shapeOf is the names of the top-level directives and of each server
// block's direct children but its locations: what a value that escaped its
// word or its directive would change.
func shapeOf(text string) (string, error) {
	p, err := parseConf(text)
	if err != nil {
		return "", err
	}
	var out []string
	for _, d := range p.top {
		out = append(out, d.name())
		if d.name() == "server" {
			for _, c := range d.children {
				if c.name() != "location" {
					out = append(out, "  "+c.name())
				}
			}
		}
	}
	return strings.Join(out, "\n"), nil
}

// A redirect rule the site's check accepts renders as its locations and
// nothing else: each holds the one return or rewrite whose words are the
// rule's, and the rest of the vhost keeps the shape of a plain rule's.
func FuzzRedirectRender(f *testing.F) {
	f.Add("/old", "/new", false, false)
	f.Add("/blog", "https://news.example.org/p", true, true)
	f.Add("/a", "https://example.com/a?b=c", false, true)
	f.Add("/a(b)", "/x*y", true, false)
	f.Add("/a", "/b\";return 200 x;#", false, false)
	plain := map[bool]string{}
	for _, prefix := range []bool{false, true} {
		match := map[bool]string{false: state.RedirectExact, true: state.RedirectPrefix}[prefix]
		out, err := redirectInput(state.SiteRedirects{Rules: []state.RedirectRule{{Match: match, From: "/p", To: "/q", Status: 301}}})
		if err != nil {
			f.Fatal(err)
		}
		if plain[prefix], err = shapeOf(out[DefaultSitesDir+"/c900.example.test.conf"]); err != nil {
			f.Fatal(err)
		}
	}
	f.Fuzz(func(t *testing.T, from, to string, prefix, found bool) {
		match, status := state.RedirectExact, 301
		if prefix {
			match = state.RedirectPrefix
		}
		if found {
			status = 302
		}
		out, err := redirectInput(state.SiteRedirects{Rules: []state.RedirectRule{{Match: match, From: from, To: to, Status: status}}})
		if err != nil {
			return // the site's check refused it
		}
		text := vhostOf(t, out, "c900.example.test")
		if got, err := shapeOf(text); err != nil || got != plain[prefix] {
			t.Fatalf("rule %q -> %q changed the vhost's shape:\n%s", from, to, text)
		}
		p, _ := parseConf(text)
		var exact, rest int
		directives(p.top, nil, func(d, parent *directive) {
			if d.name() != "location" || parent == nil || parent.name() != "server" || len(d.words) != 3 {
				return
			}
			switch {
			case d.words[1] == "=" && d.words[2] == from:
				exact++
				want := []string{"return", strconv.Itoa(status), withQuery(to)}
				if len(d.children) != 1 || !slices.Equal(d.children[0].words, want) {
					t.Fatalf("rule %q -> %q: exact location %v holds %v", from, to, d.words, childWords(d))
				}
			case prefix && d.words[1] == "^~" && d.words[2] == from+"/":
				rest++
				flag := map[int]string{301: "permanent", 302: "redirect"}[status]
				want := []string{"rewrite", "^" + regexp.QuoteMeta(from+"/") + "(.*)$", redirectRest(to), flag}
				if len(d.children) != 1 || !slices.Equal(d.children[0].words, want) {
					t.Fatalf("rule %q -> %q: prefix location %v holds %v", from, to, d.words, childWords(d))
				}
			}
		})
		if exact != 1 || (prefix && rest != 1) {
			t.Fatalf("rule %q -> %q: %d exact and %d prefix locations\n%s", from, to, exact, rest, text)
		}
	})
}

func childWords(d *directive) [][]string {
	var out [][]string
	for _, c := range d.children {
		out = append(out, c.words)
	}
	return out
}

// A routing block CheckRouting accepts renders, the vhost parses, and the
// block stays inside its server: the vhost's shape is the default's.
func FuzzRouting(f *testing.F) {
	for _, s := range []string{routingQuoted, "location / {\n    try_files $uri $uri/ /index.php?$args;\n}\n",
		"location /ws/ {\n    proxy_pass http://127.0.0.1:3001;\n}\n", "}\nserver {\nlisten 80;\n", "location / { root /etc; }",
		"location /x/ { alias /srv/sites/c901.example.test/current/x/; }", "include /etc/passwd;"} {
		f.Add(s)
	}
	base := matrixInput(901, state.SitePHP, state.ProfileBalanced, false)
	files, err := Renderer{}.SiteFiles(base)
	if err != nil {
		f.Fatal(err)
	}
	want, err := shapeOf(files[DefaultSitesDir+"/c901.example.test.conf"])
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, text string) {
		in := matrixInput(901, state.SitePHP, state.ProfileBalanced, false)
		in.Site.Config.Routing = text
		if CheckRouting(text, RoutingContextOf(in)) != nil {
			return
		}
		files, err := Renderer{}.SiteFiles(in)
		if err != nil {
			t.Fatalf("CheckRouting accepted %q, the render refused it: %v", text, err)
		}
		v := vhostOf(t, files, "c901.example.test")
		if got, err := shapeOf(v); err != nil || got != want {
			t.Fatalf("routing %q changed the vhost's shape:\n%s", text, v)
		}
	})
}
