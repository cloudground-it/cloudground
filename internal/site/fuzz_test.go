package site

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The fuzz targets of the validators whose output reaches rendered files or
// a command line as root (docs/build-and-test.md, "Fuzzing"). Each checks a
// property of what it accepts, not only that it does not panic.

// unsafeInConfig are the bytes that could end a word, a directive or a
// block in nginx's or systemd's text, or a line anywhere.
const unsafeInConfig = " \t\r\n\x00;{}\"'\\$#`"

func FuzzValidateDomain(f *testing.F) {
	for _, s := range []string{"example.com", "a.b.c.example.org", "xn--bcher-kva.example", "-a.example", "a..b", "A.example", "a.example.", "localhost", "a_b.example", strings.Repeat("a.", 130) + "com"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, d string) {
		if ValidateDomain(d) != nil {
			return
		}
		if len(d) > 253 || strings.ContainsAny(d, unsafeInConfig+"/:*?%[]()=,@") || strings.Contains(d, "..") {
			t.Fatalf("accepted %q", d)
		}
		if d != strings.ToLower(d) || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") || strings.HasPrefix(d, "-") || !strings.Contains(d, ".") {
			t.Fatalf("accepted %q", d)
		}
		for _, label := range strings.Split(d, ".") {
			if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				t.Fatalf("accepted %q (label %q)", d, label)
			}
		}
	})
}

// A site Validate accepts has its root at the sites root's folder named
// after its domain, and nothing in the fields root acts on can break a line.
func FuzzValidateSite(f *testing.F) {
	f.Add("example.com", "www.example.com", "public", "cg-site-3", "/srv/sites/example.com", "wordpress")
	f.Add("a.example", "", "", "cg-site-1", "/srv/sites/a.example/staging", "php")
	f.Add("a.example", "b.example", "../x", "cg-site-0", "/srv/sites/../etc", "static")
	f.Fuzz(func(t *testing.T, domain, alias, public, user, root, typ string) {
		s := state.Site{Domain: domain, Type: state.SiteType(typ), SystemUser: user, RootPath: root}
		if alias != "" {
			s.Aliases = []string{alias}
		}
		s.Config.PublicRoot = public
		v, err := Validate("/srv/sites", s)
		if err != nil {
			return
		}
		_ = v
		for _, x := range append([]string{s.Domain, s.SystemUser, s.Config.PublicRoot}, s.Aliases...) {
			if strings.ContainsAny(x, unsafeInConfig) || strings.Contains(x, "..") {
				t.Fatalf("accepted a field %q of %+v", x, s)
			}
		}
		if strings.ContainsAny(s.RootPath, "\x00\r\n") {
			t.Fatalf("accepted root %q", s.RootPath)
		}
		if filepath.Clean(s.RootPath) != "/srv/sites/"+s.Domain {
			t.Fatalf("accepted root %q for %s", s.RootPath, s.Domain)
		}
	})
}

// A redirect CheckRedirects accepts has a From and a To that nginx reads as
// one word each: no byte that ends a word or a directive, and no variable.
func FuzzCheckRedirects(f *testing.F) {
	f.Add("/old", "/new", "exact", 301, "")
	f.Add("/blog", "https://news.example.org/p", "prefix", 302, "www.example.com")
	f.Add("/a", "https://example.com/a?b=c", "exact", 301, "")
	f.Add("/a;b", "/x", "exact", 301, "")
	f.Add("/a", "/$host", "prefix", 301, "")
	f.Fuzz(func(t *testing.T, from, to, match string, status int, canonical string) {
		names := []string{"example.com", "www.example.com"}
		r := state.SiteRedirects{Canonical: canonical, Rules: []state.RedirectRule{{Match: match, From: from, To: to, Status: status}}}
		if CheckRedirects(r, names) != nil {
			return
		}
		for _, w := range []string{from, to, canonical} {
			if strings.ContainsAny(w, unsafeInConfig) {
				t.Fatalf("accepted %q (rule %+v)", w, r)
			}
		}
		if !strings.HasPrefix(from, "/") || strings.Contains(from, "/../") || strings.HasSuffix(from, "/..") {
			t.Fatalf("accepted from %q", from)
		}
	})
}

// A repository CheckGitRepo accepts cannot be read by git as an option or a
// transport helper, carries no secret, and a file: one stays in the root.
func FuzzCheckGitRepo(f *testing.F) {
	for _, s := range []string{"https://github.com/o/r.git", "ssh://git@github.com:22/o/r.git", "git@github.com:o/r.git",
		"file:///srv/sites/a.example/src/repo", "file:///etc/passwd", "ext::sh -c x", "-uhttps://x", "https://u:p@h/x", "git@h:-x", "fd::17"} {
		f.Add(s)
	}
	const root = "/srv/sites/a.example"
	f.Fuzz(func(t *testing.T, repo string) {
		if CheckGitRepo(repo, root) != nil {
			return
		}
		if strings.HasPrefix(repo, "-") || strings.ContainsAny(repo, " \t\r\n\x00") {
			t.Fatalf("accepted %q", repo)
		}
		low := strings.ToLower(repo)
		for _, bad := range []string{"ext::", "fd::", "::"} {
			if strings.Contains(low, bad) && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "ssh://") {
				t.Fatalf("accepted a transport helper %q", repo)
			}
		}
		if strings.HasPrefix(low, "file:") {
			// Spelled file:///: git reads "file:/x" as ssh to a host "file".
			if !strings.HasPrefix(repo, "file:///") {
				t.Fatalf("accepted a file repository git does not read as one: %q", repo)
			}
			u, err := url.Parse(repo)
			if err != nil {
				t.Fatalf("accepted %q, which does not parse", repo)
			}
			if c := filepath.Clean(u.Path); c != root && !strings.HasPrefix(c, root+"/") {
				t.Fatalf("accepted file: outside the root %q", repo)
			}
		}
		if i := strings.Index(repo, "://"); i > 0 {
			rest := repo[i+3:]
			if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 && strings.Contains(rest[:at], ":") {
				t.Fatalf("accepted a password %q", repo)
			}
		}
	})
}
