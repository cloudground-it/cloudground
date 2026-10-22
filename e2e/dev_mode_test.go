//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// devMode (#80): a PHP site that prints a new value on every render is
// cached (a second view is a HIT with the same body); in development mode
// the cache is bypassed (no HIT, a new body each time, no cache header),
// the site's record carries the end of the mode; a length other than 15, 30
// or 60 is 400; ending it puts the cache back and the warmer refills it
// before any visitor asks (the cache directory holds pages again with no
// request from this test), and the next view is a HIT.
func (h *harness) devMode(t *testing.T) {
	dom := h.dom("devmode")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#80: the site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.php", `<?php echo 'cg-80 ', hrtime(true);`, h.api.site(id).SystemUser)

	path := fmt.Sprintf("/api/sites/%d/dev-mode", id)
	view := func() (until string) {
		c, b := h.api.get(fmt.Sprintf("/api/sites/%d", id))
		var s struct {
			Config struct {
				DevModeUntil string `json:"dev_mode_until"`
			} `json:"config"`
		}
		if c != 200 || json.Unmarshal(b, &s) != nil {
			t.Logf("GET site: %d %s", c, head(b, 200))
		}
		return s.Config.DevModeUntil
	}
	// twice is the status and body of two views, a moment apart.
	twice := func() (string, bool) {
		a := h.get(dom, "/")
		time.Sleep(1500 * time.Millisecond) // the entry is stored once the response is done
		b := h.get(dom, "/")
		return CacheStatus(b.Header), string(a.Body) == string(b.Body)
	}

	status, same := twice()
	h.check(t, "#80: control: the second view is a HIT", status, "HIT")
	h.assert(t, "#80: control: it is the cached body", same, "two views of a cached page differ")

	c, _ := h.api.put(path, map[string]any{"minutes": 7})
	h.check(t, "#80: a length that is not 15, 30 or 60 is refused", fmt.Sprint(c), "400")
	c, b := h.api.put(path, map[string]any{"minutes": 15})
	h.check(t, "#80: development mode for 15 minutes is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT %s: %s", path, head(b, 300))
	}
	until, err := time.Parse(time.RFC3339, view())
	h.assert(t, "#80: the site's record carries the end, about 15 minutes ahead",
		err == nil && time.Until(until) > 13*time.Minute && time.Until(until) < 16*time.Minute, "dev_mode_until %q (%v)", view(), err)

	status, same = twice()
	h.assert(t, "#80: in development mode the page is not served from the cache", status != "HIT", "cache status %q", status)
	h.assert(t, "#80: in development mode every view is rendered again", !same, "two views gave the same body")

	c, _ = h.api.put(path, map[string]any{"minutes": 0})
	h.check(t, "#80: ending development mode is accepted", fmt.Sprint(c), "200")
	h.check(t, "#80: the site's record has no end any more", view(), "")
	dir := "/var/cache/cloudground/" + strings.ToLower(dom)
	// No request from here: whatever the cache holds, the warmer put there.
	var files string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		files = h.run("sh", "-c", "find "+dir+" -type f | wc -l").Trim()
		if files != "0" {
			break
		}
	}
	h.assert(t, "#80: the warmer refilled the cache before any visitor", files != "0", "no cached page under %s", dir)
	h.check(t, "#80: the first view after the end is a HIT", h.cacheStatus(dom, "", "/"), "HIT")
}
