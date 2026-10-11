//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// routingOnlyToAdminSessions (R-4.3 review): a site whose routing is
// custom carries the block in its record only for an administrator's
// session, the caller GET /routing admits. An API token of the same admin
// gets the site without it, in the list and alone.
func (h *harness) routingOnlyToAdminSessions(t *testing.T, id int64) {
	one := fmt.Sprintf("/api/sites/%d", id)
	_, b := h.api.get(one)
	h.assert(t, "R-4.3 review: control: the admin's session sees the site's custom routing", strings.Contains(string(b), `"routing":`), "site: %s", head(b, 400))

	c, tb := h.api.post("/api/tokens", map[string]any{"name": fmt.Sprintf("e2e-routing-%d", h.pid), "expires_in_days": 1})
	var created struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(tb, &created)
	h.check(t, "R-4.3 review: control: an admin creates an API token", fmt.Sprint(c), "201")
	if created.Token == "" {
		return
	}
	defer h.undo("revoke the routing check's API token", func() { h.api.del(fmt.Sprintf("/api/tokens/%d", created.APIToken.ID)) })()
	for _, p := range []string{one, "/api/sites"} {
		c, body := h.api.do("GET", p, nil, "Authorization: Bearer "+created.Token)
		h.assert(t, "R-4.3 review: an API token's GET "+strings.Replace(p, fmt.Sprint(id), "{id}", 1)+" has no routing block",
			c == 200 && !strings.Contains(string(body), `"routing":`), "%d %s", c, head(body, 400))
	}
}
