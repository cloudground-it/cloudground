//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
)

// whatsNew (#106): the panel serves the changelog that is inside its own
// binary: releases newest first with their text, the running release among
// the account's unseen ones until it is marked seen, and marking it records
// the release that is running, not one the request names.
func (h *harness) whatsNew(t *testing.T) {
	type answer struct {
		Current string   `json:"current"`
		Seen    string   `json:"seen"`
		Unseen  []string `json:"unseen"`
		Entries []struct {
			Version string `json:"version"`
			Body    string `json:"body"`
		} `json:"entries"`
	}
	read := func() (answer, int) {
		c, b := h.api.get("/api/changelog")
		var a answer
		if c != 200 || json.Unmarshal(b, &a) != nil {
			t.Logf("GET /api/changelog: %d %s", c, head(b, 300))
		}
		return a, c
	}
	a, c := read()
	h.check(t, "#106: the changelog is served", fmt.Sprint(c), "200")
	h.assert(t, "#106: it holds releases, each with a version tag and text",
		len(a.Entries) > 0 && regexp.MustCompile(`^v\d+\.\d+\.\d+`).MatchString(a.Entries[0].Version) && a.Entries[0].Body != "", "%+v", a)
	h.assert(t, "#106: it names the running release", a.Current != "" && a.Current != "dev", "current %q", a.Current)

	c, b := h.api.post("/api/account/whats-new", map[string]string{"seen": "v99.0.0"})
	h.check(t, "#106: marking the changelog seen is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("POST: %s", head(b, 300))
	}
	a, _ = read()
	h.check(t, "#106: the running release is recorded, not the one the request names", a.Seen, a.Current)
	h.assert(t, "#106: nothing is unseen afterwards", len(a.Unseen) == 0, "unseen %v", a.Unseen)
}
