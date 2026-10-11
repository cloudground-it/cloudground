package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// An admin the 2FA requirement holds back lands on the Security page to
// enrol: the session says so (the UI keeps it there), and the page's
// session list answers; its tokens, password and profile stay refused (the
// page hides them meanwhile). The session list used to answer 403.
func TestForcedEnrolmentPage(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.Require2FAAdmin = true; return nil }); err != nil {
		t.Fatal(err)
	}
	resp, body := c.do("GET", "/api/session", nil)
	var sess struct {
		User          *struct{ Email string } `json:"user"`
		EnrolRequired bool                    `json:"enrol_required"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &sess) != nil || sess.User == nil || !sess.EnrolRequired {
		t.Fatalf("session = %d %s", resp.StatusCode, body)
	}
	if resp, body := c.do("GET", "/api/account/sessions", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("sessions during enrolment = %d %s", resp.StatusCode, body)
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/tokens"}, {"POST", "/api/tokens"}, {"PUT", "/api/account/profile"}, {"POST", "/api/account/password"},
	} {
		if resp, body := c.do(r.method, r.path, map[string]string{}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s during enrolment = %d %s", r.method, r.path, resp.StatusCode, body)
		}
	}

	// Without the requirement the flag is off.
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.Require2FAAdmin = false; return nil }); err != nil {
		t.Fatal(err)
	}
	_, body = c.do("GET", "/api/session", nil)
	sess.EnrolRequired = true
	if json.Unmarshal(body, &sess) != nil || sess.EnrolRequired {
		t.Fatalf("session without the requirement: %s", body)
	}
}
