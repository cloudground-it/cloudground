package site

import (
	"regexp"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's redirect rules and canonical host (#83, internal/engine SPEC
// §4.10). Paths and targets are pasted into nginx's configuration, so they
// are not escaped but kept to a short list of characters none of which means
// anything to nginx: no space, quote, semicolon, brace, backslash, `$` or `#`.

// Bounds of the redirect rules.
const (
	MaxRedirectRules  = 200
	MaxRedirectFrom   = 512
	MaxRedirectTarget = 1024
)

var (
	// redirectPathRe is a path of the site: unreserved characters and a few
	// sub-delimiters. nginx matches the decoded path, so a percent escape
	// would never match and is not allowed.
	redirectPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/+,:@!()=*-]*$`)
	// redirectTargetRe is a relative target: a path with an optional query,
	// whose percent escapes are checked apart.
	redirectTargetRe = regexp.MustCompile(`^/[A-Za-z0-9._~/+,:@!()=*%&?-]*$`)
	// redirectAbsRe is an absolute target: http or https, a host name and an
	// optional port, then a path and query of the same characters.
	redirectAbsRe = regexp.MustCompile(`^(https?)://([a-z0-9.-]+)(?::([0-9]{1,5}))?(/[A-Za-z0-9._~/+,:@!()=*%&?-]*)?$`)
	percentRe     = regexp.MustCompile(`%`)
	percentOKRe   = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)

// NormalizeRedirects returns the rules as they are stored: a prefix rule's
// path and target without a trailing slash.
func NormalizeRedirects(r state.SiteRedirects) state.SiteRedirects {
	out := state.SiteRedirects{Canonical: r.Canonical}
	for _, x := range r.Rules {
		if x.Match == state.RedirectPrefix {
			x.From = strings.TrimRight(x.From, "/")
			if x.To != "/" {
				x.To = strings.TrimRight(x.To, "/")
			}
		}
		out.Rules = append(out.Rules, x)
	}
	return out
}

// percentsOK reports whether every % in s starts a two-digit escape.
func percentsOK(s string) bool {
	return len(percentRe.FindAllString(s, -1)) == len(percentOKRe.FindAllString(s, -1))
}

// segmentsOK refuses empty, "." and ".." segments: nginx resolves them
// before a location is chosen, so such a path would never match.
func segmentsOK(p string) bool {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i < len(segs)-1) {
			return false
		}
	}
	return true
}

// checkRedirectTarget validates a rule's target and says whether it is a
// path of the site, with that path (without a query).
func checkRedirectTarget(x state.RedirectRule) (path string, local bool, err error) {
	to := x.To
	if len(to) == 0 || len(to) > MaxRedirectTarget {
		return "", false, invalid("a redirect target is at most %d characters", MaxRedirectTarget)
	}
	if m := redirectAbsRe.FindStringSubmatch(to); m != nil {
		if ValidateDomain(m[2]) != nil || !percentsOK(m[4]) {
			return "", false, invalid("invalid redirect target %q", to)
		}
		if m[3] != "" && (m[3][0] == '0' || len(m[3]) > 5 || atoi(m[3]) > 65535) {
			return "", false, invalid("invalid port in the redirect target %q", to)
		}
		if x.Match == state.RedirectPrefix && strings.ContainsAny(m[4], "?") {
			return "", false, invalid("the target of a prefix rule has no query: %q", to)
		}
		return "", false, nil
	}
	if !redirectTargetRe.MatchString(to) || strings.HasPrefix(to, "//") || !percentsOK(to) {
		return "", false, invalid("a redirect target is a path of this site (/new) or an http(s) address, without spaces or special characters: %q", to)
	}
	if x.Match == state.RedirectPrefix && strings.Contains(to, "?") {
		return "", false, invalid("the target of a prefix rule has no query: %q", to)
	}
	path, _, _ = strings.Cut(to, "?")
	return path, true, nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// CheckRedirects refuses redirects that could not be rendered as they are.
// names are the site's domain and aliases. Rules are checked as stored
// (NormalizeRedirects): no loop to itself, no two rules for one path.
func CheckRedirects(r state.SiteRedirects, names []string) error {
	if r.Canonical != "" && !slices.Contains(names, r.Canonical) {
		return invalid("the canonical host %q is not one of the site's names", r.Canonical)
	}
	if len(r.Rules) > MaxRedirectRules {
		return invalid("at most %d redirects", MaxRedirectRules)
	}
	seen := map[string]bool{}
	for _, x := range r.Rules {
		if x.Match != state.RedirectExact && x.Match != state.RedirectPrefix {
			return invalid("a redirect matches the exact path or a prefix")
		}
		if x.Status != 301 && x.Status != 302 {
			return invalid("a redirect's status is 301 or 302")
		}
		if len(x.From) > MaxRedirectFrom || !redirectPathRe.MatchString(x.From) || !segmentsOK(x.From) {
			return invalid("the path %q is a path of this site (/old) of letters, digits and . _ ~ - / + , : @ ! ( ) = *", x.From)
		}
		if x.Match == state.RedirectPrefix {
			if x.From == "" || x.From == "/" || strings.HasSuffix(x.From, "/") {
				return invalid("a prefix rule needs a path below the root, without a trailing slash: %q", x.From)
			}
		}
		to, local, err := checkRedirectTarget(x)
		if err != nil {
			return err
		}
		if local && (to == x.From || (x.Match == state.RedirectPrefix && strings.HasPrefix(to, x.From+"/"))) {
			return invalid("%q redirects to itself", x.From)
		}
		if seen[x.From] {
			return invalid("two redirects for %q", x.From)
		}
		seen[x.From] = true
	}
	return nil
}
