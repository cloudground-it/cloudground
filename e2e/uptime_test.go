//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// uptime (#32): the panel requests a monitored site from its public address
// every minute. The suite's sites keep the self-signed fallback
// certificate, which a visitor's browser refuses, so the check must report
// the site as failing for that reason, and after two failures raise an
// uptime alert of the site; turning the check off clears it. (A site with
// a trusted certificate answering is the unit tests'.)
func (h *harness) uptime(t *testing.T) {
	dom := h.dom("uptime")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#32: the static site provisions", ok, "#32: %s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	path := fmt.Sprintf("/api/sites/%d/uptime", id)
	c, b := h.api.put(path, `{"enabled":true,"path":"/"}`)
	h.check(t, "#32: the uptime check is turned on", fmt.Sprintf("%d %t", c, strings.Contains(string(b), `"url":"https://`+dom+`/"`)), "200 true")

	type last struct {
		Up       bool   `json:"up"`
		Error    string `json:"error"`
		Failures int    `json:"failures"`
	}
	var got last
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		_, b := h.api.get(path)
		var v struct {
			Last *last `json:"last"`
		}
		_ = json.Unmarshal(b, &v)
		if v.Last != nil {
			got = *v.Last
			if got.Failures >= 2 {
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
	h.assert(t, "#32: a site a browser would refuse (self-signed certificate) fails the check, twice",
		!got.Up && got.Failures >= 2 && strings.Contains(strings.ToLower(got.Error), "certificate"), "#32: last check %+v", got)

	alerted := func() bool {
		_, b := h.api.get("/api/system/alerts")
		var list []struct {
			Kind string `json:"kind"`
			Site string `json:"site"`
		}
		_ = json.Unmarshal(b, &list)
		for _, a := range list {
			if a.Kind == "uptime" && a.Site == dom {
				return true
			}
		}
		return false
	}
	waitFor := func(want bool) bool {
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Second) {
			if alerted() == want {
				return true
			}
		}
		return false
	}
	h.assert(t, "#32: the outage is an uptime alert of the site", waitFor(true), "#32: no uptime alert for %s", dom)
	c, _ = h.api.put(path, `{"enabled":false}`)
	h.check(t, "#32: the uptime check is turned off", fmt.Sprint(c), "200")
	h.assert(t, "#32: turning the check off clears the alert", waitFor(false), "#32: the uptime alert of %s outlived its check", dom)
}
