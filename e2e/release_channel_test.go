//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseChannel (R-6.3): the panel follows a release channel, stable by
// default, chosen in the settings; the update check reports the channel it
// follows; an unknown channel is refused and changes nothing. The installed
// cgctl verifies a release directory with the keys compiled into it, and
// refuses one whose signature is missing.
func (h *harness) releaseChannel(t *testing.T) {
	settings := func() map[string]string {
		var m map[string]string
		_, b := h.api.get("/api/settings")
		_ = json.Unmarshal(b, &m)
		return m
	}
	before := settings()["update_channel"]
	h.check(t, "R-6.3: the settings carry a release channel", fmt.Sprint(before == "stable" || before == "beta"), "true")
	defer h.api.put("/api/settings", map[string]string{"update_channel": before})

	h.check(t, "R-6.3: choosing the beta channel is accepted", code(h.api.put("/api/settings", map[string]string{"update_channel": "beta"})), "200")
	h.check(t, "R-6.3: the beta channel is kept", settings()["update_channel"], "beta")
	var status struct {
		Channel string `json:"channel"`
	}
	_, b := h.api.get("/api/system/update")
	_ = json.Unmarshal(b, &status)
	h.check(t, "R-6.3: the update check follows the chosen channel", status.Channel, "beta")

	c, body := h.api.put("/api/settings", map[string]string{"update_channel": "nightly"})
	h.check(t, "R-6.3: an unknown channel is refused",
		fmt.Sprintf("%d %t", c, strings.Contains(string(body), "update_channel: must be stable or beta")), "400 true")
	h.check(t, "R-6.3: a refused channel changes nothing", settings()["update_channel"], "beta")

	h.check(t, "R-6.3: stable is accepted again", code(h.api.put("/api/settings", map[string]string{"update_channel": "stable"})), "200")
	h.check(t, "R-6.3: the stable channel is kept", settings()["update_channel"], "stable")

	// A directory with binaries but no signed manifest: the installed cgctl
	// (version.VerifyRelease, with the keys it was built with) refuses it.
	dir, err := os.MkdirTemp("", "cg-e2e-r63-")
	if err != nil {
		t.Fatal(err)
	}
	defer h.remove(dir)
	bin, _ := os.ReadFile("/usr/local/bin/cgctl")
	_ = os.WriteFile(filepath.Join(dir, "cgctl"), bin, 0o755)
	r := h.run("/usr/local/bin/cgctl", "release", "verify", dir, "cgctl")
	h.assert(t, "R-6.3: cgctl release verify refuses an unsigned release", r.Code != 0 && strings.Contains(r.Err, "SHA256SUMS"),
		"R-6.3: cgctl release verify on an unsigned directory: %s", r)
}
