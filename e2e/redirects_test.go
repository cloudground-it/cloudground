//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// redirects (#83): a PHP site with a www alias, behind the page cache. Its
// rules answer with the Location headers a browser follows, on the same
// request: a path that was served from the cache a moment ago is redirected
// at once, by nginx and not from the cache, and is served again, fresh, once
// the rule is gone; a prefix rule carries the rest of the path and the query
// string; a rule that is not a path of the site, or that nginx could read as
// something else, is refused; the canonical host sends the other name to it
// on both ports with path and query kept, and an alias that is the canonical
// host cannot be removed meanwhile.
func (h *harness) redirects(t *testing.T) {
	dom := h.dom("redir83")
	www := "www." + dom
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q,"aliases":[%q]}`, dom, h.PHPV, www))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#83: the PHP site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	owner := h.api.site(id).SystemUser
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/page.php", `<?php echo 'cg-83-page ', hrtime(true);`, owner)
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/moved.php", `<?php echo 'cg-83-moved';`, owner)
	path := fmt.Sprintf("/api/sites/%d/redirects", id)
	rule := func(match, from, to string, status int) map[string]any {
		return map[string]any{"match": match, "from": from, "to": to, "status": status}
	}
	location := func(r resp) string { return r.Header.Get("Location") }
	// at is the path (with query) of a Location, whatever its scheme and host.
	at := func(r resp) string { return URLPath(location(r)) }

	// Control: the page is served from the cache.
	h.get(dom, "/page.php")
	time.Sleep(1500 * time.Millisecond) // the entry is stored once the response is done
	before := h.get(dom, "/page.php")
	h.check(t, "#83: control: the second view of the page is a HIT", CacheStatus(before.Header), "HIT")

	// What is refused, with nothing stored.
	for name, body := range map[string]map[string]any{
		"a path with a directive in it":    {"rules": []any{rule("exact", "/a; return 200 x", "/b", 301)}},
		"a target with a variable":         {"rules": []any{rule("exact", "/a", "/$host", 301)}},
		"a target that is another site":    {"rules": []any{rule("exact", "/a", "//evil.test/x", 301)}},
		"a rule that redirects to itself":  {"rules": []any{rule("prefix", "/a", "/a/b", 301)}},
		"two rules for one path":           {"rules": []any{rule("exact", "/a", "/b", 301), rule("prefix", "/a", "/c", 302)}},
		"a status that is not 301 or 302":  {"rules": []any{rule("exact", "/a", "/b", 307)}},
		"a canonical host that is no name": {"canonical": "evil.test"},
	} {
		c, b := h.api.put(path, body)
		h.assert(t, "#83: refused with 400: "+name, c == 400, "= %d %s", c, head(b, 300))
	}
	var stored struct {
		Canonical string `json:"canonical"`
		Rules     []any  `json:"rules"`
	}
	_, vb := h.api.get(path)
	_ = json.Unmarshal(vb, &stored)
	h.assert(t, "#83: and nothing is stored", stored.Canonical == "" && len(stored.Rules) == 0, "%s", head(vb, 300))

	// Rules: an exact one, a prefix one with a query string, a prefix one to
	// another site, a temporary one.
	c, b := h.api.put(path, map[string]any{"rules": []any{
		rule("exact", "/page.php", "/moved.php", 301),
		rule("prefix", "/blog", "/news", 302),
		rule("prefix", "/docs/v1", "https://docs.example.org/v1", 301),
		rule("exact", "/shop/", "/store/?src=old", 302),
	}})
	h.check(t, "#83: the rules are accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT %s: %s", path, head(b, 300))
	}
	h.check(t, "#83: the site's nginx has them (nginx -t passed before it was stored)", fmt.Sprint(strings.Contains(h.run("nginx", "-T").Out, "location = /page.php")), "true")

	r := h.get(dom, "/page.php")
	h.check(t, "#83: a path that was cached a moment ago answers 301 at once", fmt.Sprintf("%d %s", r.Code, at(r)), "301 /moved.php")
	h.assert(t, "#83: not from the page cache", CacheStatus(r.Header) == "" && !strings.Contains(string(r.Body), "cg-83-page"), "cache %q, body %q", CacheStatus(r.Header), head(r.Body, 100))
	r = h.get(dom, "/page.php?utm=1&x=2")
	h.check(t, "#83: the query string goes along", fmt.Sprintf("%d %s", r.Code, at(r)), "301 /moved.php?utm=1&x=2")
	r = h.get(dom, "/moved.php")
	h.assert(t, "#83: the target itself is served", r.Code == 200 && strings.Contains(string(r.Body), "cg-83-moved"), "= %d %s", r.Code, head(r.Body, 100))

	r = h.get(dom, "/blog")
	h.check(t, "#83: a prefix rule redirects its own path, temporarily", fmt.Sprintf("%d %s", r.Code, at(r)), "302 /news")
	r = h.get(dom, "/blog/2024/a-post?x=1")
	h.check(t, "#83: and what is under it, with the rest of the path and the query", fmt.Sprintf("%d %s", r.Code, at(r)), "302 /news/2024/a-post?x=1")
	r = h.get(dom, "/blogger")
	h.assert(t, "#83: a path that only starts with the same letters is left alone", r.Code != 301 && r.Code != 302, "= %d %s", r.Code, location(r))
	r = h.get(dom, "/docs/v1/guide/intro?a=b")
	h.check(t, "#83: a prefix rule may go to another site", fmt.Sprintf("%d %s", r.Code, location(r)), "301 https://docs.example.org/v1/guide/intro?a=b")
	r = h.get(dom, "/shop/?old=1")
	h.check(t, "#83: an exact rule's own query string replaces the visitor's", fmt.Sprintf("%d %s", r.Code, at(r)), "302 /store/?src=old")
	r = h.get(dom, "/shop/cart")
	h.assert(t, "#83: and does not match a path under it", r.Code != 301 && r.Code != 302, "= %d %s", r.Code, location(r))

	// Taking the rule away serves the page again, fresh.
	c, _ = h.api.put(path, map[string]any{"rules": []any{}})
	h.check(t, "#83: removing the rules is accepted", fmt.Sprint(c), "200")
	r = h.get(dom, "/page.php")
	h.assert(t, "#83: the page is served again", r.Code == 200 && strings.Contains(string(r.Body), "cg-83-page"), "= %d %s", r.Code, head(r.Body, 100))
	h.assert(t, "#83: rendered again, not what the cache held before the rule", string(r.Body) != string(before.Body),
		"cache %q, the body is the old one: %t", CacheStatus(r.Header), string(r.Body) == string(before.Body))

	// The canonical host.
	c, b = h.api.put(path, map[string]any{"canonical": dom})
	h.check(t, "#83: the bare name as canonical host is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT %s: %s", path, head(b, 300))
	}
	r = h.get(www, "/some/path?q=1&r=2")
	h.check(t, "#83: the www name goes to the bare one, path and query kept", fmt.Sprintf("%d %s", r.Code, location(r)), "301 https://"+dom+"/some/path?q=1&r=2")
	r = h.get(dom, "/moved.php")
	h.assert(t, "#83: the bare name is served", r.Code == 200 && strings.Contains(string(r.Body), "cg-83-moved"), "= %d %s", r.Code, head(r.Body, 100))
	plain := h.run("curl", "-s", "-m", "10", "-o", "/dev/null", "-D", "-", "-H", "Host: "+www, "http://127.0.0.1/x?y=1").Out
	h.assert(t, "#83: on port 80 the www name goes straight to the bare one over https", strings.Contains(plain, " 301") && strings.Contains(strings.ToLower(plain), "location: https://"+dom+"/x?y=1"), "%s", head([]byte(plain), 400))
	plain = h.run("curl", "-s", "-m", "10", "-o", "/dev/null", "-D", "-", "-H", "Host: "+dom, "http://127.0.0.1/x?y=1").Out
	h.assert(t, "#83: and so does the bare name", strings.Contains(plain, " 301") && strings.Contains(strings.ToLower(plain), "location: https://"+dom+"/x?y=1"), "%s", head([]byte(plain), 400))

	// The other way round, and off.
	c, _ = h.api.put(path, map[string]any{"canonical": www})
	h.check(t, "#83: the www name as canonical host is accepted", fmt.Sprint(c), "200")
	r = h.get(dom, "/a/b?c=d")
	h.check(t, "#83: the bare name goes to the www one", fmt.Sprintf("%d %s", r.Code, location(r)), "301 https://"+www+"/a/b?c=d")
	var view struct {
		Canonical         string `json:"canonical"`
		CounterpartServed bool   `json:"counterpart_served"`
	}
	_, b = h.api.get(path)
	_ = json.Unmarshal(b, &view)
	h.assert(t, "#83: the settings say which name is canonical and that the other is served", view.Canonical == www && view.CounterpartServed, "%s", head(b, 300))
	// With the alias as canonical host, the alias cannot go.
	code, body := h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", id), map[string]any{"aliases": []string{}})
	h.assert(t, "#83: the alias that is the canonical host cannot be removed meanwhile", code == 400 && strings.Contains(string(body), "canonical"), "= %d %s", code, head(body, 300))
	c, _ = h.api.put(path, map[string]any{"canonical": ""})
	h.check(t, "#83: turning it off is accepted", fmt.Sprint(c), "200")
	r = h.get(www, "/moved.php")
	h.assert(t, "#83: each name is served as it is again", r.Code == 200 && strings.Contains(string(r.Body), "cg-83-moved"), "= %d %s", r.Code, head(r.Body, 100))
}
