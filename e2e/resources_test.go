//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// siteResources (#18, #19): each site's slice has its limits and its
// accounting; a site over its memory limit is killed inside its slice and
// another site goes on serving; the usage is sampled, shown by site, and a
// site over its threshold raises an alert.
func (h *harness) siteResources(t *testing.T) {
	type tenant struct {
		id        int64
		dom, user string
	}
	mk := func(name string) (tenant, bool) {
		dom := h.dom(name)
		id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
		ok := h.api.waitActive(id, 4*time.Minute) == "active"
		h.assert(t, "resources: PHP site "+name+" provisions", ok, "site %s did not become active", dom)
		return tenant{id: id, dom: dom, user: h.api.site(id).SystemUser}, ok
	}
	a, okA := mk("resa")
	b, okB := mk("resb")
	defer func() {
		for _, s := range []tenant{a, b} {
			if s.id != 0 {
				h.api.deleteSite(s.id)
				h.api.teardownGone(s.id)
			}
		}
		if a.user != "" {
			h.assert(t, "#19: the slice unit goes with its site", !exists("/etc/systemd/system/"+a.user+".slice"),
				"/etc/systemd/system/%s.slice is still there", a.user)
		}
	}()
	if !okA || !okB {
		return
	}
	cg := func(s tenant, file string) string {
		return readTrim(filepath.Join("/sys/fs/cgroup/cg.slice/cg-site.slice", s.user+".slice", file))
	}
	unit := readTrim("/etc/systemd/system/" + a.user + ".slice")
	h.assert(t, "#19: the site's slice unit has its limits and accounting",
		strings.Contains(unit, "\nMemoryMax=") && strings.Contains(unit, "\nTasksMax=512") && strings.Contains(unit, "\nIOAccounting=yes"), "%s", unit)
	h.check(t, "#19: the kernel bounds the site's tasks by default", cg(a, "pids.max"), "512")

	// #19: a memory limit, enforced by the kernel on the running slice.
	c, body := h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", a.id), map[string]any{"memory_max_mb": 128})
	h.check(t, "#19: an administrator sets a site's memory limit", fmt.Sprint(c), "202")
	if c == 202 {
		h.api.waitTask(taskID(body), 120*time.Second)
	}
	h.check(t, "#19: the running slice takes the new memory limit", cg(a, "memory.max"), strconv.Itoa(128<<20))

	bPID := h.run("systemctl", "show", "-p", "MainPID", "--value", "cloudground-php-"+b.user+".service").Trim()
	cur := func(s tenant) string { return "/srv/sites/" + s.dom + "/current" }
	h.mustWriteAs(t, cur(a)+"/cg-eat.php", `<?php $a = []; for ($i = 0; $i < 400; $i++) { $a[] = str_repeat('x', 1 << 20); } echo 'survived';`, a.user)
	h.mustWriteAs(t, cur(b)+"/cg-ok.php", `<?php echo 'b-ok';`, b.user)
	done := make(chan resp, 1)
	go func() { done <- h.get(a.dom, fmt.Sprintf("/cg-eat.php?x=%d", time.Now().UnixNano())) }()
	during := h.get(b.dom, fmt.Sprintf("/cg-ok.php?x=%d", time.Now().UnixNano()))
	eat := <-done
	h.assert(t, "#19: a site over its memory limit does not finish the request", !strings.Contains(string(eat.Body), "survived"),
		"a 400 MB request under a 128 MB limit answered %d %q", eat.Code, eat.Body)
	kills, _ := strconv.Atoi(keyedValue(cg(a, "memory.events"), "oom_kill"))
	h.assert(t, "#19: the kernel killed inside the site's slice", kills > 0, "memory.events: %s", cg(a, "memory.events"))
	h.check(t, "#19: another site serves while one is over its limit", fmt.Sprintf("%d %s", during.Code, during.Body), "200 b-ok")
	after := h.get(b.dom, fmt.Sprintf("/cg-ok.php?x=%d", time.Now().UnixNano()))
	h.check(t, "#19: another site serves after one was killed", fmt.Sprintf("%d %s", after.Code, after.Body), "200 b-ok")
	h.check(t, "#19: the other site's PHP master was not touched",
		h.run("systemctl", "show", "-p", "MainPID", "--value", "cloudground-php-"+b.user+".service").Trim(), bPID)
	h.check(t, "#19: nothing was killed in the other site's slice", keyedValue(cg(b, "memory.events"), "oom_kill"), "0")
	h.check(t, "#19: the site over its limit serves at once (only the worker was killed)", h.get(a.dom, fmt.Sprintf("/?x=%d", time.Now().UnixNano())).CodeStr(), "200")
	h.remove(cur(a)+"/cg-eat.php", cur(b)+"/cg-ok.php")

	// #18: three samples (each maintenance tick takes one), a threshold
	// the idle site is over, and the alert.
	c, body = h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", a.id), map[string]any{"usage_alerts": map[string]any{"memory_pct": 1}})
	h.check(t, "#18: the site's usage alert thresholds are set", fmt.Sprint(c), "202")
	if c == 202 {
		h.api.waitTask(taskID(body), 120*time.Second)
	}
	// Samples of a site are at least 20 s apart (a rate over less says
	// nothing): one tick, then three more after a wait each.
	for i := range 4 {
		if i > 0 {
			time.Sleep(21 * time.Second)
		}
		h.api.runMaintenance()
	}
	c, body = h.api.get(fmt.Sprintf("/api/sites/%d/usage", a.id))
	var u struct {
		Limits struct {
			MemoryMaxMB int `json:"memory_max_mb"`
		} `json:"limits"`
		Points []struct {
			MemBytes int64 `json:"mem_bytes"`
		} `json:"points"`
		Latest *struct {
			MemBytes  int64 `json:"mem_bytes"`
			DiskBytes int64 `json:"disk_bytes"`
			OOMKills  int64 `json:"oom_kills"`
		} `json:"latest"`
	}
	_ = json.Unmarshal(body, &u)
	h.assert(t, "#18: the site's usage has samples with its memory", c == 200 && len(u.Points) >= 3 && u.Latest != nil && u.Latest.MemBytes > 0,
		"GET usage = %d %s", c, body)
	h.assert(t, "#18: the site's disk use is measured", u.Latest != nil && u.Latest.DiskBytes > 0, "%s", body)
	h.assert(t, "#18: the usage shows the kill at the memory limit", u.Latest != nil && u.Latest.OOMKills > 0, "%s", body)
	h.check(t, "#18: the usage shows the site's limit", strconv.Itoa(u.Limits.MemoryMaxMB), "128")
	// The evaluation follows the sample: give it a few seconds.
	var alerts []byte
	raised := false
	for end := time.Now().Add(20 * time.Second); !raised && time.Now().Before(end); time.Sleep(time.Second) {
		_, alerts = h.api.get("/api/system/alerts")
		var list []struct{ Kind, Site string }
		_ = json.Unmarshal(alerts, &list)
		for _, al := range list {
			raised = raised || (al.Kind == "usage" && al.Site == a.dom)
		}
	}
	h.assert(t, "#18: a site over its memory threshold for three samples is an alert", raised, "alerts: %.2000s", alerts)
	_, other := h.api.get(fmt.Sprintf("/api/sites/%d/usage", b.id))
	h.assert(t, "#18: the other site has samples of its own", strings.Contains(string(other), `"points":[{`), "%s", other)
}

// keyedValue is the value of key in a "key value" file's text.
func keyedValue(text, key string) string {
	for _, l := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(l, " "); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
