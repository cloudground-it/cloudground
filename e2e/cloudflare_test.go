//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// cloudflareSection (#22): a static site behind Cloudflare takes the
// visitor's address from CF-Connecting-IP only from Cloudflare's ranges:
// the vhost includes the ranges' file, which holds nothing but Cloudflare's
// public networks; a request from anywhere else (here, the server itself)
// keeps its own address in the access log, whatever header it sends, and
// is not judged by the block list under the address it names; the block
// list reaches nginx for such sites; Cloudflare's network cannot be
// blocked; a purge with a zone and a token that Cloudflare refuses still
// empties the server's cache and says Cloudflare's failed.
func (h *harness) cloudflareSection(t *testing.T) {
	const (
		include = "/etc/nginx/conf.d/cloudground-cloudflare.inc"
		banned  = "/etc/nginx/conf.d/cloudground-banned.conf"
		spoofed = "203.0.113.77"
		blocked = "203.0.113.78"
		zone    = "0123456789abcdef0123456789abcdef"
	)
	dom := h.dom("cdn")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#22: the static site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.html", "cg-cloudflare-site", h.api.site(id).SystemUser)
	path := fmt.Sprintf("/api/sites/%d/cloudflare", id)
	vhost := "/etc/nginx/sites-enabled/" + dom + ".conf"

	c, _ := h.api.put(path, map[string]any{"zone_id": "not-a-zone"})
	h.check(t, "#22: a zone that is not 32 hex digits is refused", fmt.Sprint(c), "400")
	c, b := h.api.put(path, map[string]any{"real_ip": true, "zone_id": zone})
	h.check(t, "#22: real visitor addresses and a zone are accepted", fmt.Sprint(c), "200")
	h.assert(t, "#22: the answer has them", strings.Contains(string(b), `"real_ip":true`) && strings.Contains(string(b), zone), "%s", head(b, 300))

	conf := h.run("cat", vhost).Out
	h.assert(t, "#22: the vhost takes CF-Connecting-IP from the ranges' file",
		strings.Contains(conf, "include "+include+";") && strings.Contains(conf, "real_ip_header CF-Connecting-IP;") && strings.Contains(conf, "if ($cg_banned)"), "%s", head([]byte(conf), 2000))
	inc := h.run("cat", include).Out
	var nets []string
	bad := ""
	for _, l := range strings.Split(inc, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !strings.HasPrefix(l, "set_real_ip_from ") || strings.Contains(l, "0.0.0.0/0") || strings.Contains(l, " 127.") || strings.Contains(l, " ::/0") {
			bad = l
		}
		nets = append(nets, l)
	}
	h.assert(t, "#22: the ranges' file holds Cloudflare's networks and nothing else", len(nets) >= 10 && bad == "", "bad line %q in:\n%s", bad, inc)
	h.assert(t, "#22: the block list's geo is there for the site", strings.Contains(h.run("cat", banned).Out, "geo $cg_banned {"), "%s", h.run("ls", "-l", "/etc/nginx/conf.d").Out)

	// From anywhere but Cloudflare the header is not trusted: the server
	// itself sends one, and the log keeps the server's address.
	mark := fmt.Sprintf("/cg22-spoof-%d", time.Now().UnixNano())
	get := func(addr, p string) string {
		return h.run("curl", "-sk", "-m", "10", "-o", "/dev/null", "-w", "%{http_code}", "-H", "CF-Connecting-IP: "+addr,
			"--resolve", dom+":443:127.0.0.1", "https://"+dom+p).Out
	}
	get(spoofed, mark)
	var line string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && line == ""; time.Sleep(time.Second) {
		for _, l := range strings.Split(h.run("tail", "-n", "200", "/var/log/cloudground/"+dom+"/access.log").Out, "\n") {
			if strings.Contains(l, mark) {
				line = l
			}
		}
	}
	h.assert(t, "#22: a request from outside Cloudflare's ranges keeps its own address in the log",
		strings.HasPrefix(line, "127.0.0.1 ") && !strings.Contains(line, spoofed), "log line %q", line)

	// The block list reaches nginx; an address named in the header of a
	// request from outside Cloudflare is not the one judged.
	c, _ = h.api.post("/api/blocklist/blocks", map[string]any{"address": blocked, "note": "e2e #22", "minutes": 5})
	h.check(t, "#22: an address is blocked", fmt.Sprint(c), "200")
	defer h.api.do("DELETE", "/api/blocklist/blocks", map[string]string{"address": blocked})
	var geo string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if geo = h.run("cat", banned).Out; strings.Contains(geo, blocked+"/32  1;") {
			break
		}
	}
	h.assert(t, "#22: the block reaches the sites behind Cloudflare", strings.Contains(geo, blocked+"/32  1;"), "%s", geo)
	h.check(t, "#22: a header naming the blocked address, from outside Cloudflare, is not trusted", get(blocked, "/"), "200")
	c, _ = h.api.do("DELETE", "/api/blocklist/blocks", map[string]string{"address": blocked})
	h.check(t, "#22: the block is removed", fmt.Sprint(c), "200")
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if geo = h.run("cat", banned).Out; !strings.Contains(geo, blocked) {
			break
		}
	}
	h.assert(t, "#22: and leaves nginx's list", !strings.Contains(geo, blocked), "%s", geo)
	c, b = h.api.post("/api/blocklist/blocks", map[string]any{"address": "104.16.0.0/16"})
	h.assert(t, "#22: Cloudflare's network cannot be blocked", c == 400 && strings.Contains(string(b), "Cloudflare"), "%d %s", c, head(b, 300))

	// The purge: with a token Cloudflare refuses, the server's cache is
	// still emptied, and the answer says Cloudflare's was not.
	c, _ = h.api.put("/api/settings", map[string]string{"cloudflare_purge_token": "cg-e2e-not-a-real-token"})
	h.check(t, "#22: the purge token is saved", fmt.Sprint(c), "200")
	defer h.api.put("/api/settings", map[string]string{"cloudflare_purge_token": ""})
	_, sb := h.api.get("/api/settings")
	h.assert(t, "#22: the purge token is never shown", !strings.Contains(string(sb), "cg-e2e-not-a-real-token"), "%s", head(sb, 400))
	c, b = h.api.post(fmt.Sprintf("/api/sites/%d/purge", id), map[string]any{})
	h.assert(t, "#22: a purge Cloudflare refuses still succeeds, and says so", c == 200 && strings.Contains(string(b), `"cdn":"failed"`), "%d %s", c, head(b, 400))

	c, _ = h.api.put(path, map[string]any{"real_ip": false, "zone_id": ""})
	h.check(t, "#22: turning it off is accepted", fmt.Sprint(c), "200")
	conf = h.run("cat", vhost).Out
	h.assert(t, "#22: off, the vhost trusts no header", !strings.Contains(conf, "real_ip_header") && !strings.Contains(conf, include), "%s", head([]byte(conf), 1200))
}
