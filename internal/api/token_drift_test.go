package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// An API token may not accept drift (it would bless a change it planted);
// it may still ask for a restore, and the session may accept (SPEC.md
// §1.2.1, §7.1).
func TestTokensCannotAcceptDrift(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	tok := mintToken(t, admin, "script", 0)
	ev, err := s.Store.CreateDriftEvent(state.DriftEvent{Path: "/etc/nginx/sites-enabled/planted.conf", ExpectedHash: "v1", ActualHash: "planted"})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/system/drift/%d/resolve", ev.ID)

	code, body := bearer(t, ts, tok.Token, "POST", path, map[string]string{"action": "accept"})
	if code != http.StatusForbidden || !strings.Contains(string(body), "cannot accept drift") {
		t.Fatalf("token accept = %d %s, want 403", code, body)
	}
	if open, _ := s.Store.ListDriftEvents(false); len(open) != 1 {
		t.Fatalf("the token's refused accept resolved the event: %+v", open)
	}
	// Restore is the panel's own content put back: the token reaches it
	// (here no site owns the file, so it is the handler's 422).
	if code, body := bearer(t, ts, tok.Token, "POST", path, map[string]string{"action": "restore"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("token restore = %d %s, want the handler's 422", code, body)
	}
	if resp, body := admin.do("POST", path, map[string]string{"action": "accept"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("session accept = %d %s", resp.StatusCode, body)
	}
}
