package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// An operator lists, reads and manages only the sites assigned to them, and
// none of the server: not the accounts, not a server-wide log (even paired
// with a site they own), not the server log route at all, not the server's
// capacity. Their site's logs are read through the site.
func TestOperatorSeesOnlyTheirSites(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	mine := createViaAPI(t, s, admin, staticSite("assigned.example.com"))
	other := createViaAPI(t, s, admin, staticSite("private.example.com"))
	resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "operator@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{mine.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create the operator = %d %s", resp.StatusCode, body)
	}
	op := signIn(t, ts, "operator@example.com", "operator-password!")

	resp, body = op.do("GET", "/api/sites", nil)
	var listed []state.Site
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &listed) != nil || len(listed) != 1 || listed[0].ID != mine.ID {
		t.Fatalf("the operator's site list = %d %s", resp.StatusCode, body)
	}

	expect := func(method, path string, want int) {
		t.Helper()
		var payload any
		if method != "GET" {
			payload = map[string]any{}
		}
		if resp, body := op.do(method, path, payload); resp.StatusCode != want {
			t.Errorf("operator %s %s = %d %s, want %d", method, path, resp.StatusCode, body, want)
		}
	}
	expect("GET", fmt.Sprintf("/api/sites/%d", other.ID), http.StatusNotFound)
	expect("POST", fmt.Sprintf("/api/sites/%d/purge", mine.ID), http.StatusOK)
	expect("GET", "/api/users", http.StatusForbidden)
	for _, source := range []string{"nginx-access", "nginx-error", "mariadb", "agent", "api", "php-error"} {
		expect("GET", "/api/logs?source="+source+"&site=assigned.example.com", http.StatusForbidden)
	}
	expect("GET", "/api/logs?source=site:assigned.example.com:access&site=assigned.example.com", http.StatusForbidden)
	expect("GET", fmt.Sprintf("/api/sites/%d/logs?source=access", mine.ID), http.StatusOK)
	expect("GET", fmt.Sprintf("/api/sites/%d/logs?source=access", other.ID), http.StatusNotFound)
	expect("GET", fmt.Sprintf("/api/sites/%d/logs?source=nginx-access", mine.ID), http.StatusBadRequest)
	expect("GET", "/api/system/status", http.StatusForbidden)
	expect("GET", "/api/system/metrics", http.StatusForbidden)
}
