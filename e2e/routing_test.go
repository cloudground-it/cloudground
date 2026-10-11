//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// routing (R-5.4, internal/engine SPEC §7) and the per-type cache bypass
// (R-5.2, SPEC §5.1). Every check is on what nginx serves, what the vhost on
// disk says and what the drift check reports; on a build without them the
// routing rows answer 404 and a Laravel session is served from the cache.
func (h *harness) routing(t *testing.T) {
	h.laravelBypass(t)
	h.staticRouting(t)
	h.phpRouting(t)
}

// routingSite creates a site of this section and waits for it; 0 when it
// did not provision.
func (h *harness) routingSite(t *testing.T, label, body string) (int64, string, string) {
	dom := h.dom(label)
	id := h.api.createSite(strings.Replace(body, "DOMAIN", dom, 1))
	ok := id != 0 && h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, label+": the site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return 0, dom, ""
	}
	return id, dom, h.api.site(id).SystemUser
}

func (h *harness) dropRoutingSite(t *testing.T, id int64, label string) {
	h.check(t, label+": delete accepted", h.api.deleteSite(id), "202")
	h.check(t, label+": teardown completes", h.api.teardownGone(id), "yes")
}

// setRouting is PUT /api/sites/{id}/routing: the status and the body.
func (h *harness) setRouting(id int64, routing string) (int, string) {
	c, b := h.api.put(fmt.Sprintf("/api/sites/%d/routing", id), map[string]string{"routing": routing})
	return c, string(b)
}

// cacheOf is the X-CloudGround-Cache word for one request with a cookie
// ("" for none).
func (h *harness) cacheOf(dom, path, cookie string) string {
	hdr := []string{"Accept-Encoding", "gzip"}
	if cookie != "" {
		hdr = append(hdr, "Cookie", cookie)
	}
	return CacheStatus(h.get(dom, path, hdr...).Header)
}

// laravelBypass (R-5.2): a Laravel page is cached for anonymous visitors
// and never served from the cache to one holding a Laravel session.
func (h *harness) laravelBypass(t *testing.T) {
	id, dom, user := h.routingSite(t, "lvcache", fmt.Sprintf(`{"domain":"DOMAIN","type":"laravel","php_version":%q}`, h.PHPV))
	if id == 0 {
		return
	}
	defer h.dropRoutingSite(t, id, "R-5.2: the Laravel site")
	// A page that sets no cookie, so the cache stores it.
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/public/index.php", `<?php echo "laravel-e2e";`, user)
	h.cacheOf(dom, "/", "")
	h.check(t, "R-5.2 control: an anonymous Laravel page is cached", h.cacheOf(dom, "/", ""), "HIT")
	for _, cookie := range []string{"laravel_session=e2e", "shop_session=e2e", "XSRF-TOKEN=e2e", "remember_web_59ba36addc2b2f9401580f014c7f58ea4e30989d=e2e"} {
		name, _, _ := strings.Cut(cookie, "=")
		h.check(t, "R-5.2: a Laravel page is not served from the cache with "+name, h.cacheOf(dom, "/", "a=1; "+cookie), "BYPASS")
	}
	h.check(t, "R-5.2: Laravel's /login bypasses the cache", h.cacheOf(dom, "/login", ""), "BYPASS")

	// R-5.4: never a listing of the release with its .env; R-10.5: no
	// listing at all, since nginx's listing stats through a symlink the
	// tenant plants (another site's file's size and date).
	c, body := h.setRouting(id, "location /f/ {\n    root /srv/sites/"+dom+"/current;\n    autoindex on;\n}\n")
	h.assert(t, "R-5.4: a listing outside the public directory is refused", c == 400 && strings.Contains(body, "autoindex"), "PUT = %d %s", c, body)
	c, body = h.setRouting(id, "location /f/ {\n    autoindex on;\n}\nlocation / {\n    try_files $uri $uri/ /index.php?$args;\n}\n")
	h.assert(t, "R-10.5/F: a listing even of the public directory is refused", c == 400 && strings.Contains(body, "autoindex on is not allowed"), "PUT = %d %s", c, body)
	c, body = h.setRouting(id, "location /f/ {\n    autoindex off;\n}\nlocation / {\n    try_files $uri $uri/ /index.php?$args;\n}\n")
	h.check(t, "R-10.5/F control: autoindex off is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT routing: %s", body)
	}
}

// staticRouting (R-5.4): a static site's routing is edited, survives a
// regeneration, is covered by drift, refuses what the rules forbid and
// keeps serving when nginx refuses an edit.
func (h *harness) staticRouting(t *testing.T) {
	id, dom, user := h.routingSite(t, "rtstatic", `{"domain":"DOMAIN","type":"static"}`)
	if id == 0 {
		return
	}
	defer h.dropRoutingSite(t, id, "R-5.4: the static site")
	cur := "/srv/sites/" + dom + "/current"
	h.mustWriteAs(t, cur+"/index.html", "spa-index", user)
	h.mustWriteAs(t, cur+"/404.html", "real-404-page", user)
	vhost := "/etc/nginx/sites-enabled/" + dom + ".conf"

	// The view: the default, not custom, with the panel's parts around it.
	_, b := h.api.get(fmt.Sprintf("/api/sites/%d/routing", id))
	var v struct {
		Routing, Default, Before, After string
		Custom, Editable                bool
	}
	_ = json.Unmarshal(b, &v)
	h.assert(t, "R-5.4: the routing view is the static default, with the panel's part read-only around it",
		v.Editable && !v.Custom && strings.Contains(v.Routing, "/index.html =404") && strings.Contains(v.Before, "ssl_certificate"),
		"view: %s", head(b, 400))

	// An SPA: client-side routes are answered with index.html.
	spa := "location / {\n    try_files $uri $uri/ /index.html;\n}\n" +
		"location /assets-e2e/ {\n    add_header X-E2E-Route yes;\n    try_files $uri =404;\n}\n"
	c, body := h.setRouting(id, spa)
	h.check(t, "R-5.4: an SPA routing is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT routing: %s", body)
	}
	r := h.get(dom, "/app/settings/profile")
	h.check(t, "R-5.4: an SPA route is served index.html", r.CodeStr()+" "+string(r.Body), "200 spa-index")
	h.as(user, "mkdir", "-p", cur+"/assets-e2e")
	h.mustWriteAs(t, cur+"/assets-e2e/a.txt", "asset", user)
	r = h.get(dom, "/assets-e2e/a.txt")
	h.assert(t, "R-5.4: a location adding a header keeps the security headers",
		r.Header.Get("X-E2E-Route") == "yes" && r.Header.Get("X-Frame-Options") == "SAMEORIGIN" && r.Header.Get("X-Content-Type-Options") == "nosniff",
		"headers: %v", r.Header)

	// A static site with a real 404.
	real404 := "error_page 404 /404.html;\nlocation / {\n    try_files $uri $uri/ =404;\n}\n"
	c, body = h.setRouting(id, real404)
	h.check(t, "R-5.4: a real-404 routing is accepted", fmt.Sprint(c), "200")
	r = h.get(dom, "/no/such/page")
	h.check(t, "R-5.4: a missing page is a real 404", r.CodeStr()+" "+strings.TrimSpace(string(r.Body)), "404 real-404-page")
	h.check(t, "R-5.4: the index still serves", h.get(dom, "/").CodeStr(), "200")
	_, b = h.api.get(fmt.Sprintf("/api/sites/%d/routing", id))
	_ = json.Unmarshal(b, &v)
	h.assert(t, "R-5.4: the routing is stored as custom", v.Custom && v.Routing == real404, "view: %s", head(b, 300))
	h.routingOnlyToAdminSessions(t, id)

	// Drift: the panel recorded the vhost it wrote, so a hand edit shows.
	driftHas := func() bool { _, d := h.api.get("/api/system/drift"); return strings.Contains(string(d), vhost) }
	h.assert(t, "R-5.4: no drift after a routing change", !driftHas(), "the vhost is reported as drift after the panel wrote it")
	orig, err := os.ReadFile(vhost)
	if err == nil {
		_ = os.WriteFile(vhost, append(append([]byte{}, orig...), "# hand edit\n"...), 0o644)
		h.assert(t, "R-5.4: a hand edit of the routing's vhost is drift", driftHas(), "the edited vhost is not reported")
		_ = os.WriteFile(vhost, orig, 0o644)
		h.assert(t, "R-5.4: the drift resolves once the file is back", !driftHas(), "the restored vhost is still drift")
	}

	// A regeneration renders the stored routing again. The change is one
	// that needs no certificate: HSTS is refused (409) on a site whose Let's
	// Encrypt issuance failed, as it does once a box has rerun the suite
	// past the weekly limit for this name.
	c, b = h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", id), map[string]any{"cache_ttl_sec": 600})
	h.check(t, "R-5.4: a configuration change is accepted", fmt.Sprint(c), "202")
	h.check(t, "R-5.4: it applies", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	h.check(t, "R-5.4: the routing survives the regeneration", h.get(dom, "/no/such/page").CodeStr(), "404")

	// Refusals change nothing.
	before, _ := os.ReadFile(vhost)
	c, body = h.setRouting(id, "location / {\n    include /etc/passwd;\n}\n")
	h.assert(t, "R-5.4: a forbidden directive is refused", c == 400 && strings.Contains(body, "include"), "PUT = %d %s", c, body)
	c, body = h.setRouting(id, "location / {\n    gzip_static off;\n}\n")
	h.assert(t, "R-5.4: a directive off the allowlist is refused with its line", c == 400 && strings.Contains(body, "line 2") && strings.Contains(body, "gzip_static"), "PUT = %d %s", c, body)
	c, body = h.setRouting(id, "location / {\n    root /srv;\n}\n")
	h.assert(t, "R-5.4: a root outside the site's tree is refused", c == 400 && strings.Contains(body, "outside"), "PUT = %d %s", c, body)
	c, body = h.setRouting(id, "location / {\n    root /srv/sites/"+dom+"/shared;\n}\n")
	h.assert(t, "R-5.4: a root in shared/ is refused", c == 400 && strings.Contains(body, "release tree"), "PUT = %d %s", c, body)
	for _, port := range []string{"3306", "6379", "443", "8443"} {
		c, body = h.setRouting(id, "location /x/ {\n    proxy_pass http://127.0.0.1:"+port+";\n}\n")
		h.assert(t, "R-5.4: a proxy to the system service on "+port+" is refused", c == 400 && strings.Contains(body, "port "+port), "PUT = %d %s", c, body)
	}
	// sshd's port is on no fixed list: the agent finds it listening.
	if strings.TrimSpace(h.run("ss", "-Hltn", "sport = :22").Out) != "" {
		c, body = h.setRouting(id, "location /x/ {\n    proxy_pass http://127.0.0.1:22;\n}\n")
		h.assert(t, "R-5.4: a proxy to a listening system service (sshd) is refused", c == 400 && strings.Contains(body, "port 22"), "PUT = %d %s", c, body)
	} else {
		t.Logf("nothing listens on :22 here; the listening-service check is not exercised")
	}
	c, body = h.setRouting(id, "location /x/ {\n    proxy_pass http://127.0.0.1:3999;\n    proxy_set_header Host other.example.test;\n}\n")
	h.assert(t, "R-5.4: another Host towards this machine is refused", c == 400 && strings.Contains(body, "proxy_set_header Host"), "PUT = %d %s", c, body)
	// nginx's own verdict on this line is `"expires" directive invalid
	// value`: it names the directive, not the value, and the panel gives
	// the line in the block instead of the vhost's.
	c, body = h.setRouting(id, "location / {\n    expires notatime;\n}\n")
	h.assert(t, "R-5.4: an edit nginx rejects is refused with nginx's reason",
		c == 400 && strings.Contains(body, "nginx refused") && strings.Contains(body, `\"expires\" directive invalid value`) && strings.Contains(body, "routing line 2"),
		"PUT = %d %s", c, body)
	after, _ := os.ReadFile(vhost)
	h.assert(t, "R-5.4: refused edits leave the vhost as it was", string(after) == string(before), "the vhost changed")
	h.check(t, "R-5.4: nginx -t passes after the refused edits", fmt.Sprint(h.run("nginx", "-t").OK()), "true")
	h.check(t, "R-5.4: the site keeps serving its routing", h.get(dom, "/no/such/page").CodeStr()+" "+h.get(dom, "/").CodeStr(), "404 200")

	// The server's symlink protection holds inside the custom block: a link
	// the site user plants to another tenant's file is not followed.
	oid, odom, ouser := h.routingSite(t, "rtother", `{"domain":"DOMAIN","type":"static"}`)
	if oid != 0 {
		h.mustWriteAs(t, "/srv/sites/"+odom+"/current/secret.txt", "other-tenant-secret", ouser)
		h.as(user, "mkdir", "-p", cur+"/linked")
		h.mustWriteAs(t, cur+"/linked/own.txt", "own-file", user)
		h.as(user, "ln", "-sfn", "/srv/sites/"+odom+"/current/secret.txt", cur+"/linked/secret.txt")
		c, body = h.setRouting(id, "location /linked/ {\n    alias /srv/sites/"+dom+"/current/linked/;\n}\nlocation / {\n    try_files $uri $uri/ =404;\n}\n")
		h.check(t, "R-5.4: a routing with an alias in the release tree is accepted", fmt.Sprint(c), "200")
		h.check(t, "R-5.4 control: the alias serves the site's own file", h.get(dom, "/linked/own.txt").CodeStr(), "200")
		r = h.get(dom, "/linked/secret.txt")
		h.assert(t, "R-5.4: a symlink to another tenant is not followed inside the custom block",
			(r.Code == 403 || r.Code == 404) && !strings.Contains(string(r.Body), "other-tenant-secret"),
			"GET /linked/secret.txt = %d %s", r.Code, head(r.Body, 100))
		h.dropRoutingSite(t, oid, "R-5.4: the other tenant")
	}

	// Restore default.
	c, _ = h.setRouting(id, "")
	h.check(t, "R-5.4: restore default is accepted", fmt.Sprint(c), "200")
	r = h.get(dom, "/no/such/page")
	h.check(t, "R-5.4: the default routing is back (index.html fallback)", r.CodeStr()+" "+string(r.Body), "200 spa-index")
}

// phpRouting (R-5.4): a front controller other than index.php, and a PHP
// site made of endpoints.
func (h *harness) phpRouting(t *testing.T) {
	id, dom, user := h.routingSite(t, "rtphp", fmt.Sprintf(`{"domain":"DOMAIN","type":"php","php_version":%q}`, h.PHPV))
	if id == 0 {
		return
	}
	defer h.dropRoutingSite(t, id, "R-5.4: the PHP site")
	cur := "/srv/sites/" + dom + "/current"
	h.mustWriteAs(t, cur+"/index.php", `<?php echo "index-php";`, user)
	h.mustWriteAs(t, cur+"/app.php", `<?php echo "slim:" . $_SERVER["REQUEST_URI"] . ":" . ($_GET["x"] ?? "");`, user)
	h.as(user, "mkdir", "-p", cur+"/api")
	h.mustWriteAs(t, cur+"/api/users.php", `<?php echo "users-endpoint";`, user)

	c, body := h.setRouting(id, "# Slim\nlocation / {\n    try_files $uri /app.php$is_args$args;\n}\n")
	h.check(t, "R-5.4: a Slim front controller routing is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT routing: %s", body)
	}
	r := h.get(dom, "/hello/world?x=1")
	h.check(t, "R-5.4: the front controller app.php answers every route", r.CodeStr()+" "+string(r.Body), "200 slim:/hello/world?x=1:1")

	c, _ = h.setRouting(id, "location / {\n    try_files $uri $uri/ =404;\n}\n")
	h.check(t, "R-5.4: an endpoints routing is accepted", fmt.Sprint(c), "200")
	r = h.get(dom, "/api/users.php")
	h.check(t, "R-5.4: a PHP endpoint runs", r.CodeStr()+" "+string(r.Body), "200 users-endpoint")
	h.check(t, "R-5.4: a path that is no endpoint is a 404, not index.php", h.get(dom, "/api/users").CodeStr(), "404")
}
