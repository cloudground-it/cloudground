//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

// tokenGovernance (R-4.3 review): an API token automates sites, not the
// panel. On the box, a token's change of the alert webhook or the update
// check is refused (403) and the setting stays as it was; so are the
// firewall, the panel's certificate and the self-update, which answer the
// token 403 before reading the body (the probe's body has an unknown
// field, so a build without the rule answers 400 and changes nothing).
func (f *feat) tokenGovernance(t *testing.T, token string) {
	h := f.h
	bearer := "Authorization: Bearer " + token
	_, before := h.api.get("/api/settings")
	hook, check := jsonField(before, "notify_webhook"), jsonField(before, "update_check")
	// A build that let the token through would have changed them: put back
	// what the session read.
	h.undo("put back the settings the token tried to change", func() {
		h.api.put("/api/settings", map[string]string{"notify_webhook": hook, "update_check": check})
	})

	probe := fmt.Sprintf("https://cg-e2e-%d.invalid/hook", h.pid)
	c, _ := h.api.do("PUT", "/api/settings", map[string]string{"notify_webhook": probe}, bearer)
	h.check(t, "R-4.3 review: a token cannot change the alert webhook", fmt.Sprint(c), "403")
	other := "0"
	if check == "0" {
		other = ""
	}
	c, _ = h.api.do("PUT", "/api/settings", map[string]string{"update_check": other}, bearer)
	h.check(t, "R-4.3 review: a token cannot change the update check", fmt.Sprint(c), "403")
	_, after := h.api.get("/api/settings")
	h.assert(t, "R-4.3 review: the settings are as before the token's requests",
		jsonField(after, "notify_webhook") == hook && jsonField(after, "update_check") == check, "before %s, after %s", head(before, 300), head(after, 300))
	c, _ = h.api.do("PUT", "/api/settings", map[string]string{"acme_email": jsonField(before, "acme_email")}, bearer)
	h.check(t, "R-4.3 review: a token may still set acme_email", fmt.Sprint(c), "200")

	for _, p := range []string{"/api/firewall/rule", "/api/firewall/enable", "/api/panel/certificate", "/api/panel/update"} {
		c, _ = h.api.do("POST", p, map[string]any{"cg_e2e_probe": true}, bearer)
		h.check(t, "R-4.3 review: a token is refused "+p, fmt.Sprint(c), "403")
	}

	// Accepting drift is a session's; restoring a token may ask for. An id
	// that names no event: the refusal comes before the lookup (a build
	// without the rule answers 404), and nothing is resolved either way.
	drift := "/api/system/drift/999999999/resolve"
	c, _ = h.api.do("POST", drift, map[string]string{"action": "accept"}, bearer)
	h.check(t, "R-4.3 review: a token cannot accept drift", fmt.Sprint(c), "403")
	c, _ = h.api.do("POST", drift, map[string]string{"action": "restore"}, bearer)
	h.check(t, "R-4.3 review: control: a token reaches a drift restore (no such event)", fmt.Sprint(c), "404")
}
