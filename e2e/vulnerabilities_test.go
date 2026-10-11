//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// vulnPlugin is a plugin version with a vulnerability every source lists
// and a known fix: Contact Form 7 5.3.1, unrestricted file upload
// (CVE-2020-35489), fixed in 5.3.2. WordPress.org keeps the old version
// downloadable.
const (
	vulnPlugin  = "contact-form-7"
	vulnVersion = "5.3.1"
	vulnFixedIn = "5.3.2"
)

// wordpressVulnerabilities (#31): an old plugin version installed on the
// feature site is found by the panel's check, through the real database,
// with its fixed version; the site gets a vulnerability alert. With the
// plugin gone, a new check no longer lists it.
func (h *harness) wordpressVulnerabilities(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		installed := h.wp(f.fsu, f.fp, "plugin", "install", vulnPlugin, "--version="+vulnVersion, "--force")
		h.assert(t, "#31 control: an old Contact Form 7 is installed", installed.OK(), "#31: wp plugin install: %s", installed.Out)
		if !installed.OK() {
			return
		}
		remove := h.undo("remove "+vulnPlugin, func() { h.wp(f.fsu, f.fp, "plugin", "delete", vulnPlugin) })
		defer remove()

		type finding struct {
			Component string `json:"component"`
			Slug      string `json:"slug"`
			Version   string `json:"version"`
			FixedIn   string `json:"fixed_in"`
			CVE       string `json:"cve"`
		}
		check := func(label string) (findings []finding, ok bool) {
			c, b := h.api.post(f.api("/vulnerabilities/check"), `{}`)
			st := h.api.waitTask(taskID(b), 3*time.Minute)
			h.check(t, "#31: "+label, fmt.Sprintf("%d %s", c, st), "202 succeeded")
			var v struct {
				Checked  bool      `json:"checked"`
				Error    string    `json:"error"`
				Findings []finding `json:"findings"`
			}
			_, b = h.api.get(f.api("/vulnerabilities"))
			if err := json.Unmarshal(b, &v); err != nil || !v.Checked || v.Error != "" {
				h.assert(t, "#31: the check is recorded", false, "#31: %s", b)
				return nil, false
			}
			return v.Findings, true
		}
		hasOld := func(list []finding) bool {
			for _, x := range list {
				if x.Component == "plugin" && x.Slug == vulnPlugin && x.Version == vulnVersion && x.FixedIn == vulnFixedIn {
					return true
				}
			}
			return false
		}
		list, ok := check("the check runs against the vulnerability database")
		if !ok {
			return
		}
		h.assert(t, "#31: the old plugin is found, with the version that fixes it", hasOld(list), "#31: findings %+v", list)

		alerted := false
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && !alerted; time.Sleep(2 * time.Second) {
			_, b := h.api.get("/api/system/alerts")
			var alerts []struct {
				Kind string `json:"kind"`
				Site string `json:"site"`
			}
			_ = json.Unmarshal(b, &alerts)
			for _, a := range alerts {
				alerted = alerted || (a.Kind == "vulnerability" && a.Site == f.fs)
			}
		}
		h.assert(t, "#31: the site has a vulnerability alert", alerted, "#31: no vulnerability alert for %s", f.fs)

		remove()
		list, ok = check("a second check runs after the plugin is removed")
		h.assert(t, "#31: the removed plugin is no longer listed", ok && !hasOld(list), "#31: findings %+v", list)
	})
}
