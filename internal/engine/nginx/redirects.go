package nginx

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's canonical host and redirect rules (SPEC §4.10, #83). Nothing in
// them is escaped: site.CheckRedirects keeps paths and targets to characters
// that mean nothing to nginx, and every one is written inside double quotes
// all the same. They are locations and an `if`, not cache entries: both run
// before the page cache is read, so a cached page never hides a redirect and
// a redirect is never cached.

// redirectLocations is the rules' locations: an exact rule is `location =`,
// a prefix rule is `location =` for its own path and `location ^~` for what
// is under it, which no regex location of the routing block can take over.
func redirectLocations(rules []state.RedirectRule) []redirectLocation {
	var out []redirectLocation
	for _, x := range rules {
		code := "permanent"
		if x.Status == 302 {
			code = "redirect"
		}
		switch x.Match {
		case state.RedirectExact:
			out = append(out, redirectLocation{key: "= " + x.From, from: x.From, exact: true, ret: x.Status, to: x.To})
		case state.RedirectPrefix:
			out = append(out,
				redirectLocation{key: "= " + x.From, from: x.From, exact: true, ret: x.Status, to: x.To},
				redirectLocation{key: "^~ " + x.From + "/", from: x.From + "/", rewrite: code, to: x.To})
		}
	}
	return out
}

// redirectLocation is one location of a rule.
type redirectLocation struct {
	key     string // the location's modifier and path, as nginx tells two apart
	from    string
	exact   bool
	ret     int    // the status of an exact location's return
	rewrite string // the flag of a prefix location's rewrite
	to      string
}

// withQuery is the target an exact location returns: the request's query
// string is kept unless the target has its own.
func withQuery(to string) string {
	if strings.Contains(to, "?") {
		return to
	}
	return to + "$is_args$args"
}

// redirectRest is the target a prefix location rewrites to: the rest of the
// path after the rule's, below the target (a bare address has no path, so
// the rest goes under its root).
func redirectRest(to string) string {
	if strings.HasSuffix(to, "/") {
		return to + "$1"
	}
	return to + "/$1"
}

// redirectServer writes the rules' locations. They stand before the
// document root and the static-file locations, so none of those can answer a
// path the site redirects.
func (v *vhost) redirectServer(c *conf) {
	locs := redirectLocations(v.in.Site.Config.Redirects.Rules)
	if len(locs) == 0 {
		return
	}
	c.put(1, "",
		"# The site's redirects (SPEC §4.10). Prefix and exact locations are chosen",
		"# before any regex location, and neither reads nor fills the page cache.")
	for _, l := range locs {
		c.putf(1, "location %s {", l.key)
		if l.exact {
			c.putf(2, `return %d "%s";`, l.ret, withQuery(l.to))
		} else {
			// The rule's path is literal text: quoted for the regex, which
			// check has kept to characters the quoting leaves as they are.
			c.putf(2, `rewrite "^%s(.*)$" "%s" %s;`, regexp.QuoteMeta(l.from), redirectRest(l.to), l.rewrite)
		}
		c.put(1, "}")
	}
}

// canonicalServer writes the canonical host's redirect: every other name of
// the site goes to it, with the path and query as asked.
func (v *vhost) canonicalServer(c *conf) {
	canon := v.in.Site.Config.Redirects.Canonical
	if canon == "" {
		return
	}
	c.put(1, "",
		"# Canonical host (SPEC §4.10): the site's other names go to this one, path and",
		"# query kept. The server rewrite phase runs before the page cache is read.")
	c.putf(1, `if ($host != "%s") {`, canon)
	c.putf(2, "return 301 https://%s$request_uri;", canon)
	c.put(1, "}")
}

// checkRedirectLocations refuses a rule whose location the routing block
// already has: nginx would refuse the file ("duplicate location").
func checkRedirectLocations(s state.Site, p *parsed) error {
	locs := redirectLocations(s.Config.Redirects.Rules)
	if len(locs) == 0 || p == nil {
		return nil
	}
	own := map[string]bool{}
	for _, l := range locs {
		own[l.key] = true
	}
	for _, d := range p.top {
		if d.name() != "location" || len(d.words) < 2 {
			continue
		}
		if key := strings.Join(d.words[1:], " "); own[key] {
			return fmt.Errorf("%s: the routing block already has location %s, which a redirect needs", s.Domain, key)
		}
	}
	return nil
}
