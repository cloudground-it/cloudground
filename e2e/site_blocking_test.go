//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The site rules' client: a namespace of its own, so the section does not
// share one with "blocklist".
const (
	sbNS     = "cge2esb"
	sbHostIf = "cge2esb0"
	sbPeerIf = "cge2esb1"
	sbHost   = "198.18.79.1"
	sbClient = "198.18.79.2"
)

// siteBlocking (#25): a static site refuses, with 403, an address, a user
// agent (any case) and, serving one country only, a client with no country,
// each from a client in a network namespace, while the server itself (its
// own address, exempt) still gets the page; the rules live in their own
// file, which goes when the rules go and with the site; the country data is
// downloaded from DB-IP and attributed.
func (h *harness) siteBlocking(t *testing.T) {
	dom := h.dom("siteblock")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#25: the static site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	deleted := false
	defer func() {
		if !deleted {
			h.api.deleteSite(id)
			h.api.teardownGone(id)
		}
	}()
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.html", "cg-site-blocking", h.api.site(id).SystemUser)

	h.run("ip", "netns", "del", sbNS)
	h.run("ip", "link", "del", sbHostIf)
	teardown := h.undo("remove the site rules' client namespace", func() {
		h.run("ip", "netns", "del", sbNS)
		h.run("ip", "link", "del", sbHostIf)
	})
	defer teardown()
	for _, c := range [][]string{
		{"ip", "netns", "add", sbNS},
		{"ip", "link", "add", sbHostIf, "type", "veth", "peer", "name", sbPeerIf},
		{"ip", "link", "set", sbPeerIf, "netns", sbNS},
		{"ip", "addr", "add", sbHost + "/30", "dev", sbHostIf},
		{"ip", "link", "set", sbHostIf, "up"},
		{"ip", "netns", "exec", sbNS, "ip", "addr", "add", sbClient + "/30", "dev", sbPeerIf},
		{"ip", "netns", "exec", sbNS, "ip", "link", "set", sbPeerIf, "up"},
		{"ip", "netns", "exec", sbNS, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", sbNS, "ip", "route", "add", "default", "via", sbHost},
	} {
		if r := h.run(c[0], c[1:]...); !r.OK() {
			h.fail(t, "#25: the client namespace is set up", "%s: %s", strings.Join(c, " "), r)
			return
		}
	}
	// get is the status the client (or the server itself) gets for the
	// site's page, with a user agent.
	get := func(fromClient bool, ua string) string {
		args := []string{"curl", "-sk", "-m", "10", "-o", "/dev/null", "-w", "%{http_code}", "-A", ua,
			"--resolve", dom + ":443:" + h.IP, fmt.Sprintf("https://%s/?cg25=%d", dom, time.Now().UnixNano())}
		if fromClient {
			args = append([]string{"ip", "netns", "exec", sbNS}, args...)
		}
		return h.run(args[0], args[1:]...).Out
	}
	const browser = "Mozilla/5.0 (cg-e2e)"
	h.check(t, "#25: control: the client gets the page", get(true, browser), "200")

	path := fmt.Sprintf("/api/sites/%d/blocking", id)
	rules := fmt.Sprintf("/etc/nginx/conf.d/cloudground-block-%d.conf", id)
	put := func(what, body string) {
		c, b := h.api.put(path, body)
		h.check(t, "#25: "+what+": the rules are saved", fmt.Sprint(c), "200")
		if c != 200 {
			t.Logf("PUT %s: %s", path, head(b, 300))
		}
	}

	put("an address", `{"addresses":["`+sbClient+`"]}`)
	h.check(t, "#25: a refused address gets 403", get(true, browser), "403")
	h.check(t, "#25: the server itself still gets the page", get(false, browser), "200")
	owner, mode := statOwnerMode(rules)
	h.check(t, "#25: the rules have a file of their own, root's 0644", owner+" "+mode, "root 644")
	h.assert(t, "#25: the rules file holds the address", strings.Contains(readTrim(rules), sbClient+"/32  1;"), "%s:\n%s", rules, readTrim(rules))

	put("a user agent", `{"user_agents":["cg-e2e-bot"]}`)
	h.check(t, "#25: the address is served again", get(true, browser), "200")
	h.check(t, "#25: a refused user agent gets 403, in any case", get(true, "Mozilla/5.0 CG-E2E-BOT/1.0"), "403")

	put("one country only", `{"countries":["IT"],"allow_countries_only":true}`)
	h.check(t, "#25: serving Italy only, a client with no country gets 403", get(true, browser), "403")
	h.check(t, "#25: serving Italy only, the server itself still gets the page", get(false, browser), "200")
	h.assert(t, "#25: the country data was downloaded from DB-IP", exists("/var/lib/cloudground-agent/geo/dbip-country-lite.csv.gz"), "no country data under /var/lib/cloudground-agent/geo")
	h.assert(t, "#25: the rules file names DB-IP, as its licence asks", strings.Contains(readTrim(rules), "IP Geolocation by DB-IP"), "%s:\n%s", rules, head([]byte(readTrim(rules)), 400))

	c, _ := h.api.put(path, `{"addresses":["`+h.IP+`"]}`)
	h.check(t, "#25: a rule holding the server's own address is refused", fmt.Sprint(c), "400")
	c, _ = h.api.put(path, `{"user_agents":["bad\"agent"]}`)
	h.check(t, "#25: a user agent nginx would read as syntax is refused", fmt.Sprint(c), "400")

	put("none", `{}`)
	h.check(t, "#25: without rules the client gets the page", get(true, browser), "200")
	h.assert(t, "#25: without rules there is no rules file", !exists(rules), "%s is still there", rules)

	put("a user agent again", `{"user_agents":["cg-e2e-bot"]}`)
	h.api.deleteSite(id)
	h.api.teardownGone(id)
	deleted = true
	h.assert(t, "#25: the rules file goes with the site", !exists(rules), "%s outlived the site", rules)
}
