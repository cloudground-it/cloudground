package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A deletion's audit row says what went: a cron job by its site and the
// start of its command (never the whole of it), an account by its address.
func TestAuditNamesWhatWasDeleted(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("audit.example.com"))

	resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/cron", site.ID), map[string]string{
		"schedule": "*/15 * * * *", "command": "wp cron event run --due-now",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cron = %d %s", resp.StatusCode, body)
	}
	var job state.CronJob
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if resp, body := admin.do("DELETE", fmt.Sprintf("/api/cron/%d", job.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete cron = %d %s", resp.StatusCode, body)
	}

	resp, body = admin.do("POST", "/api/users", map[string]any{"email": "todelete@example.com", "password": adminPassword, "role": "readonly"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user = %d %s", resp.StatusCode, body)
	}
	var user state.User
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatal(err)
	}
	if resp, body := admin.do("DELETE", fmt.Sprintf("/api/users/%d", user.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user = %d %s", resp.StatusCode, body)
	}

	events, err := s.Store.ListAuditEvents(50)
	if err != nil {
		t.Fatal(err)
	}
	byAction := map[string]state.AuditEvent{}
	for _, e := range events {
		byAction[e.Action] = e
	}
	cron, ok := byAction["cron.delete"]
	switch {
	case !ok:
		t.Error("no cron.delete row")
	case cron.Subject != "audit.example.com" || !strings.Contains(cron.Detail, "wp …") || strings.Contains(cron.Detail, "event run"):
		t.Errorf("cron.delete: subject %q detail %q, want the domain and the command's start", cron.Subject, cron.Detail)
	}
	gone, ok := byAction["user.delete"]
	switch {
	case !ok:
		t.Error("no user.delete row")
	case gone.Subject != "todelete@example.com":
		t.Errorf("user.delete subject %q, want the address", gone.Subject)
	}
}
