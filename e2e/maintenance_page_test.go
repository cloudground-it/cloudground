//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The maintenance page's client: a namespace of its own, so the section
// shares none with "site blocking".
const (
	mpNS     = "cge2emp"
	mpHostIf = "cge2emp0"
	mpPeerIf = "cge2emp1"
	mpHost   = "198.18.80.1"
	mpClient = "198.18.80.2"
)

// maintenancePage (#79): a static site answers a client in a network
// namespace with 503, Retry-After and the page text while it is in
// maintenance, with nginx reading the text as text (a ${variable} and
// markup are shown, not expanded), and the security headers; the server
// itself (its own address) still gets the site; a browser that opens the
// preview address gets a cookie that lets it through; so does an address on
// the list; ending it serves everyone again.
func (h *harness) maintenancePage(t *testing.T) {
	dom := h.dom("maintpage")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#79: the static site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.html", "cg-maintenance-page", h.api.site(id).SystemUser)

	h.run("ip", "netns", "del", mpNS)
	h.run("ip", "link", "del", mpHostIf)
	teardown := h.undo("remove the maintenance client namespace", func() {
		h.run("ip", "netns", "del", mpNS)
		h.run("ip", "link", "del", mpHostIf)
	})
	defer teardown()
	for _, c := range [][]string{
		{"ip", "netns", "add", mpNS},
		{"ip", "link", "add", mpHostIf, "type", "veth", "peer", "name", mpPeerIf},
		{"ip", "link", "set", mpPeerIf, "netns", mpNS},
		{"ip", "addr", "add", mpHost + "/30", "dev", mpHostIf},
		{"ip", "link", "set", mpHostIf, "up"},
		{"ip", "netns", "exec", mpNS, "ip", "addr", "add", mpClient + "/30", "dev", mpPeerIf},
		{"ip", "netns", "exec", mpNS, "ip", "link", "set", mpPeerIf, "up"},
		{"ip", "netns", "exec", mpNS, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", mpNS, "ip", "route", "add", "default", "via", mpHost},
	} {
		if r := h.run(c[0], c[1:]...); !r.OK() {
			h.fail(t, "#79: the client namespace is set up", "%s: %s", strings.Join(c, " "), r)
			return
		}
	}
	jar := "/tmp/cg-e2e-maint-jar"
	defer h.run("rm", "-f", jar)
	// get is the whole answer (headers and body) the client, or the server
	// itself, gets for a path; the jar holds the client's cookies.
	get := func(fromClient bool, path string) string {
		args := []string{"curl", "-sk", "-m", "10", "-i", "-b", jar, "-c", jar,
			"--resolve", dom + ":443:" + h.IP, "https://" + dom + path}
		if fromClient {
			args = append([]string{"ip", "netns", "exec", mpNS}, args...)
		}
		r := h.run(args[0], args[1:]...)
		if r.Out == "" {
			// Nothing came back: say why (curl's own words, its exit code).
			return fmt.Sprintf("(no answer: curl exit %d: %s)", r.Code, strings.TrimSpace(r.Err))
		}
		return r.Out
	}
	status := func(out string) string {
		f := strings.Fields(out)
		if len(f) < 2 {
			return "none"
		}
		return f[1]
	}
	put := func(body map[string]any) (int, []byte) {
		return h.api.put(fmt.Sprintf("/api/sites/%d/maintenance", id), body)
	}

	// The namespace is new: give the path to the site a minute to carry
	// traffic before the control, and say what it saw if it never does.
	var first string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if first = get(true, "/"); status(first) == "200" {
			break
		}
	}
	h.assert(t, "#79: control: the client gets the site", status(first) == "200", "%s\nroute: %s\nping: %s",
		head([]byte(first), 400), h.run("ip", "netns", "exec", mpNS, "ip", "route").Out, h.run("ip", "netns", "exec", mpNS, "ping", "-c1", "-W2", mpHost).Out)
	c, b := put(map[string]any{"enabled": true, "title": "Back <soon>", "retry_after_sec": 1800,
		"message": "cg79 ${host} <b>markup</b>\nsecond line"})
	h.check(t, "#79: turning the page on is accepted", fmt.Sprint(c), "200")
	var view struct {
		PreviewURL string `json:"preview_url"`
	}
	_ = json.Unmarshal(b, &view)
	h.assert(t, "#79: the answer names the preview address", strings.Contains(view.PreviewURL, "/.cg-maintenance/"), "%s", head(b, 300))

	out := get(true, "/")
	h.check(t, "#79: a visitor gets 503", status(out), "503")
	lower := strings.ToLower(out)
	h.assert(t, "#79: with Retry-After", strings.Contains(lower, "retry-after: 1800"), "%s", head([]byte(out), 600))
	h.assert(t, "#79: with the page's text", strings.Contains(out, "cg79") && strings.Contains(out, "second line"), "%s", head([]byte(out), 800))
	h.assert(t, "#79: the text is text: a variable is not expanded and markup is shown, not run",
		strings.Contains(out, "&#36;&#123;host&#125;") && strings.Contains(out, "&#60;b&#62;markup") && !strings.Contains(out, "<b>markup"), "%s", head([]byte(out), 800))
	h.assert(t, "#79: the security headers are on the page", strings.Contains(lower, "x-content-type-options: nosniff"), "%s", head([]byte(out), 600))
	h.assert(t, "#79: the page is not cached by anyone", strings.Contains(lower, "cache-control: no-store"), "%s", head([]byte(out), 600))
	h.assert(t, "#79: the site's own content is not served to a visitor", !strings.Contains(out, "cg-maintenance-page"), "%s", head([]byte(out), 300))
	h.check(t, "#79: the server itself still gets the site", status(get(false, "/")), "200")

	// The key: opening the preview address sets a cookie that lets a browser
	// through; a wrong key does not.
	h.check(t, "#79: a wrong key is the maintenance page", status(get(true, "/.cg-maintenance/"+strings.Repeat("0", 32))), "503")
	opened := get(true, strings.TrimPrefix(view.PreviewURL, "https://"+dom))
	h.check(t, "#79: the preview address answers a redirect", status(opened), "302")
	h.assert(t, "#79: and sets a cookie that is secure and HttpOnly",
		strings.Contains(strings.ToLower(opened), "set-cookie: cg_maint=") && strings.Contains(opened, "Secure") && strings.Contains(opened, "HttpOnly"), "%s", head([]byte(opened), 600))
	out = get(true, "/")
	h.assert(t, "#79: with the cookie the client gets the site", status(out) == "200" && strings.Contains(out, "cg-maintenance-page"), "%s", head([]byte(out), 400))
	h.run("rm", "-f", jar)
	h.check(t, "#79: without it, the maintenance page again", status(get(true, "/")), "503")

	// An address on the list.
	c, _ = put(map[string]any{"allow": []string{"nope"}})
	h.check(t, "#79: a list entry that is no address is refused", fmt.Sprint(c), "400")
	c, _ = put(map[string]any{"allow": []string{mpClient}})
	h.check(t, "#79: the list is saved", fmt.Sprint(c), "200")
	h.check(t, "#79: an address on the list gets the site", status(get(true, "/")), "200")
	put(map[string]any{"allow": []string{}})
	h.check(t, "#79: taken off the list, it gets the page", status(get(true, "/")), "503")

	c, _ = put(map[string]any{"enabled": false})
	h.check(t, "#79: turning the page off is accepted", fmt.Sprint(c), "200")
	h.check(t, "#79: everyone gets the site again", status(get(true, "/")), "200")
	c, b = h.api.get(fmt.Sprintf("/api/sites/%d/maintenance", id))
	h.assert(t, "#79: the text is kept for the next time", c == 200 && strings.Contains(string(b), "second line"), "%d %s", c, head(b, 300))
}
