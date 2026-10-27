//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// trafficStats (#33): a static site's visits are counted from its access
// log on the maintenance tick: page views and distinct visitors, a bot
// apart, assets and the panel's own requests not counted as pages, a
// missing file in the not-found list (a static site answers any other
// path with its index); the agent's file keeps no address;
// a deleted site's statistics go at the next tick.
func (h *harness) trafficStats(t *testing.T) {
	dom := h.dom("traffic")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#33: the static site provisions", ok, "%s (id %d) did not become active", dom, id)
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
	user := h.api.site(id).SystemUser
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.html", "cg-traffic-home", user)
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/about.html", "cg-traffic-about", user)
	stateFile := fmt.Sprintf("/var/lib/cloudground-agent/traffic/%d.json", id)

	get := func(path, ua string) {
		h.run("curl", "-sk", "-m", "10", "-o", "/dev/null", "-A", ua, "--resolve", dom+":443:127.0.0.1", "https://"+dom+path)
	}
	const alice = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0 cg33-a"
	const bob = "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_5) AppleWebKit/605.1.15 Safari/605.1.15 cg33-b"
	for range 3 {
		get("/", alice)
	}
	get("/about.html", bob)
	get("/", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	get("/style.css", alice)
	get("/cg33-missing.png", alice)
	get("/", "CloudGround-e2e")
	time.Sleep(2 * time.Second) // the access log is flushed every second

	c, b := h.api.runMaintenance()
	h.check(t, "#33: a maintenance tick runs", fmt.Sprint(c), "200")
	c, b = h.api.get(fmt.Sprintf("/api/sites/%d/traffic?days=7", id))
	h.check(t, "#33: the statistics answer", fmt.Sprint(c), "200")
	var v struct {
		Totals struct {
			Requests  int64 `json:"requests"`
			Pageviews int64 `json:"pageviews"`
			Visitors  int64 `json:"visitors"`
			Bots      int64 `json:"bots"`
			Status4xx int64 `json:"status_4xx"`
		} `json:"totals"`
		Days     []json.RawMessage `json:"days"`
		Hours    []json.RawMessage `json:"hours"`
		TopPages []struct {
			Path  string `json:"path"`
			Count int64  `json:"count"`
		} `json:"top_pages"`
		TopNotFound []struct {
			Path string `json:"path"`
		} `json:"top_not_found"`
		CollectedAt string `json:"collected_at"`
	}
	_ = json.Unmarshal(b, &v)
	h.assert(t, "#33: page views count the pages people opened, not the bot, the asset or the panel's own request",
		v.Totals.Pageviews == 4, "%s", head(b, 800))
	h.assert(t, "#33: two distinct visitors", v.Totals.Visitors == 2, "%s", head(b, 800))
	h.assert(t, "#33: the bot is counted apart", v.Totals.Bots == 1, "%s", head(b, 800))
	h.assert(t, "#33: every request but the panel's own is counted", v.Totals.Requests == 7 && v.Totals.Status4xx == 2, "%s", head(b, 800))
	h.assert(t, "#33: the top page is / with its three views", len(v.TopPages) > 0 && v.TopPages[0].Path == "/" && v.TopPages[0].Count == 3, "%s", head(b, 800))
	missing := false
	for _, p := range v.TopNotFound {
		missing = missing || p.Path == "/cg33-missing.png"
	}
	h.assert(t, "#33: the missing path is in the not-found list", missing, "%s", head(b, 800))
	h.assert(t, "#33: seven days, 48 hours and the time of the last read", len(v.Days) == 7 && len(v.Hours) == 48 && v.CollectedAt != "", "%s", head(b, 400))

	st := h.run("stat", "-c", "%a", stateFile).Out
	h.check(t, "#33: the agent's statistics are root's alone", strings.TrimSpace(st), "600")
	body := h.run("cat", stateFile).Out
	h.assert(t, "#33: the statistics keep no address", body != "" && !strings.Contains(body, "127.0.0.1") && !strings.Contains(body, "cg33-a"), "%s", head([]byte(body), 300))
	c, _ = h.api.get(fmt.Sprintf("/api/sites/%d/traffic?days=5", id))
	h.check(t, "#33: another range is refused", fmt.Sprint(c), "400")

	h.api.deleteSite(id)
	h.api.teardownGone(id)
	deleted = true
	h.api.runMaintenance()
	h.assert(t, "#33: a deleted site's statistics go at the next tick", !h.run("test", "-e", stateFile).OK(), "%s still exists", stateFile)
}
