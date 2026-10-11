//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// interruptedCreate (R-3.13): the agent dies while it provisions a site.
// The API used to drop the site's record on any failed provisioning call, so
// the half-built site (user, PHP master, slice, files) kept running with no
// panel page showing it, and its domain could never be created again. The
// record must stay, in error, and deleting it must remove the site.
func (h *harness) interruptedCreate(t *testing.T) {
	dom := h.dom("r313")
	id := h.api.createSite(h.wpSiteJSON(dom, "wordpress", ""))
	h.assert(t, "R-3.13: the site to interrupt is accepted", id != 0, "R-3.13: create %s refused", dom)
	if id == 0 {
		return
	}
	user := fmt.Sprintf("cg-site-%d", id)

	// Kill the agent once the site's user exists: provisioning has begun
	// and is far from over (WordPress is not installed yet).
	began := false
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if h.run("getent", "passwd", user).OK() {
			began = true
			break
		}
	}
	h.assert(t, "R-3.13: provisioning reaches the agent", began, "R-3.13: %s never appeared", user)
	if !began {
		return
	}
	h.run("systemctl", "kill", "-s", "KILL", "cloudground-agent")
	back := false
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if c, _, err := h.agentCall("Ping", "{}"); err == nil && c == 200 {
			back = true
			break
		}
	}
	h.assert(t, "R-3.13: the agent restarts after the kill", back, "R-3.13: the agent did not come back")

	status := h.api.waitActive(id, 2*time.Minute)
	if status == "active" {
		t.Logf("R-3.13: provisioning finished before the kill landed; nothing to check")
		h.check(t, "R-3.13: site delete accepted", h.api.deleteSite(id), "202")
		h.check(t, "R-3.13: site teardown completes", h.api.teardownGone(id), "yes")
		return
	}
	c, _ := h.api.get(fmt.Sprintf("/api/sites/%d", id))
	h.check(t, "R-3.13: the interrupted site is still listed", fmt.Sprintf("%d %s", c, status), fmt.Sprintf("%d error", http.StatusOK))

	// Deleting it removes whatever the interrupted provisioning built.
	h.check(t, "R-3.13: the interrupted site's delete is accepted", h.api.deleteSite(id), "202")
	h.check(t, "R-3.13: the interrupted site's teardown completes", h.api.teardownGone(id), "yes")
	h.assert(t, "R-3.13: "+user+" is gone after the delete", !h.run("getent", "passwd", user).OK(),
		"R-3.13: getent passwd %s still answers", user)
	h.assert(t, "R-3.13: the group "+user+" is gone after the delete", !h.run("getent", "group", user).OK(),
		"R-3.13: getent group %s still answers", user)
	h.assert(t, "R-3.13: the site's files are gone after the delete", !exists("/srv/sites/"+dom),
		"R-3.13: /srv/sites/%s outlived its site", dom)
	h.check(t, "R-3.13: the domain can be created again", fmt.Sprint(h.api.waitActive(h.api.createSite(h.wpSiteJSON(dom, "wordpress", "")), 4*time.Minute)), "active")
}
