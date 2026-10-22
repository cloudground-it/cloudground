//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// osUpdatesView is GET /api/system/updates.
type osUpdatesView struct {
	Packages []struct {
		Name     string `json:"name"`
		To       string `json:"to"`
		Security bool   `json:"security"`
	} `json:"packages"`
	Security       int    `json:"security"`
	RebootRequired bool   `json:"reboot_required"`
	CheckedAt      string `json:"checked_at"`
}

// simulatedUpgrades is apt's own answer, read here and not through the
// panel: the packages `apt-get -s upgrade --with-new-pkgs` would install,
// and those from a -security archive.
func (h *harness) simulatedUpgrades() (all, security []string) {
	r := h.run("apt-get", "-s", "-q", "-o", "Debug::NoLocking=1", "upgrade", "--with-new-pkgs")
	for _, line := range strings.Split(r.Out, "\n") {
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		all = append(all, f[1])
		if strings.Contains(line, "-security") {
			security = append(security, f[1])
		}
	}
	sort.Strings(all)
	sort.Strings(security)
	return all, security
}

// osUpdates (#27): the panel lists what apt would upgrade, security first,
// as apt itself says; a refresh and a security upgrade run as tasks; the
// reboot notice follows /run/reboot-required.
func (h *harness) osUpdates(t *testing.T) {
	c, b := h.api.post("/api/system/updates/refresh", `{}`)
	st := h.api.waitTask(taskID(b), 15*time.Minute)
	h.check(t, "#27: a refresh of the package lists runs", fmt.Sprintf("%d %s", c, st), "202 succeeded")

	read := func() (osUpdatesView, bool) {
		var v osUpdatesView
		code, body := h.api.get("/api/system/updates")
		return v, code == 200 && json.Unmarshal(body, &v) == nil && v.CheckedAt != ""
	}
	v, ok := read()
	h.assert(t, "#27: the pending updates are listed", ok, "#27: GET /api/system/updates failed")
	all, security := h.simulatedUpgrades()
	var listed, listedSec []string
	secondPart := false
	ordered := true
	for _, p := range v.Packages {
		listed = append(listed, p.Name)
		if p.Security {
			listedSec = append(listedSec, p.Name)
			ordered = ordered && !secondPart
		} else {
			secondPart = true
		}
	}
	sort.Strings(listed)
	sort.Strings(listedSec)
	h.check(t, "#27: the list is apt's own (apt-get -s upgrade)", strings.Join(listed, " "), strings.Join(all, " "))
	h.check(t, "#27: the security updates are the -security archive's", strings.Join(listedSec, " "), strings.Join(security, " "))
	h.assert(t, "#27: security updates come first", ordered && v.Security == len(listedSec), "#27: %+v", v.Packages)
	_, err := os.Stat("/run/reboot-required")
	h.check(t, "#27: the reboot notice follows /run/reboot-required", fmt.Sprint(v.RebootRequired), fmt.Sprint(err == nil))

	// The security updates, installed through the panel: afterwards apt has
	// none left to install.
	c, b = h.api.post("/api/system/updates/apply", `{"security_only":true}`)
	id := taskID(b)
	st = h.api.waitTask(id, 60*time.Minute)
	tk := h.api.task(id)
	h.assert(t, "#27: the security upgrade runs", c == 202 && st == "succeeded", "#27: HTTP %d, task %s: %s", c, st, tk.Error)
	_, left := h.simulatedUpgrades()
	h.check(t, "#27: no security update is left after it", strings.Join(left, " "), "")
	v, _ = read()
	h.check(t, "#27: the panel's list shows none either", fmt.Sprint(v.Security), "0")
}
