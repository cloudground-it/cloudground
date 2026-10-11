package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// A failure of the panel's own database answers 500 with a fixed message;
// the database's own text (SQLite's "no such table: …", a column, a
// constraint) reaches only the journal (SPEC.md §1.7). Each case breaks one
// table, by renaming it from a second connection, calls the handler, and
// puts the table back.
func TestStoreFailuresDoNotReachTheBrowser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, _, ts := testServerAt(t, path)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("errors.example.com"))
	tok := mintToken(t, admin, "walk", 0)
	db := rawDB(t, path)
	me, err := s.Store.GetUserByEmail(adminEmail)
	if err != nil {
		t.Fatal(err)
	}
	session := ""
	for _, ck := range admin.cookies {
		if ck.Name == s.sessionCookieName() {
			session = ck.Value
		}
	}
	c := &call{s: s, User: me, Site: site}

	type handler func(w http.ResponseWriter, r *http.Request)
	withCall := func(h func(http.ResponseWriter, *http.Request, *call)) handler {
		return func(w http.ResponseWriter, r *http.Request) { h(w, r, c) }
	}
	cases := []struct {
		name, table, method string
		body                any
		pathID              string
		h                   handler
	}{
		{"bootstrap", "users", "GET", nil, "", s.handleBootstrap},
		{"login, account lookup", "users", "POST", map[string]string{"email": adminEmail, "password": adminPassword}, "", s.handleLogin},
		{"login, session start", "sessions", "POST", map[string]string{"email": adminEmail, "password": adminPassword}, "", s.handleLogin},
		{"logout", "sessions", "POST", map[string]string{}, "", s.handleLogout},
		{"own sessions", "sessions", "GET", nil, "", withCall(s.handleListSessions)},
		{"revoke a session", "sessions", "DELETE", nil, "abcdef", withCall(s.handleRevokeSession)},
		{"password change", "users", "POST", map[string]string{"current_password": adminPassword, "new_password": "another-long-password"}, "", withCall(s.handleChangePassword)},
		{"tokens", "api_tokens", "GET", nil, "", s.handleListAPITokens},
		{"revoke a token", "api_tokens", "DELETE", nil, fmt.Sprint(tok.APIToken.ID), withCall(s.handleRevokeAPIToken)},
		{"users", "users", "GET", nil, "", s.handleListUsers},
		{"sites", "sites", "GET", nil, "", withCall(s.handleListSites)},
		{"SFTP", "sites", "POST", map[string]any{"enable": false}, "", withCall(s.handleSetSFTP)},
		{"shell", "sites", "POST", map[string]any{"enable": false}, "", withCall(s.handleSetShell)},
		{"routing", "sites", "PUT", map[string]any{"routing": ""}, "", withCall(s.handleSetRouting)},
		{"backup policy", "sites", "PUT", map[string]any{"enabled": true}, "", withCall(s.handleBackupPolicy)},
		{"backups", "backups", "GET", nil, "", withCall(s.handleListBackups)},
		{"run a backup", "panel_settings", "POST", map[string]any{}, "", withCall(s.handleRunBackup)},
		{"deployments", "deployments", "GET", nil, "", withCall(s.handleListDeployments)},
		{"safe push", "sites", "POST", map[string]any{}, "", withCall(s.handleSafePush)},
	}
	leaks := []string{"no such table", "sqlite", "SQL", "_broken"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s_broken`, tc.table, tc.table)); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s_broken RENAME TO %s`, tc.table, tc.table)); err != nil {
					t.Fatal(err)
				}
			}()
			var body bytes.Buffer
			if tc.body != nil {
				json.NewEncoder(&body).Encode(tc.body)
			}
			r := httptest.NewRequest(tc.method, "/", &body)
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: session})
			if tc.pathID != "" {
				r.SetPathValue("id", tc.pathID)
			}
			w := httptest.NewRecorder()
			tc.h(w, r)
			out := w.Body.String()
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("with %s broken: %d %s, want 500", tc.table, w.Code, out)
			}
			for _, leak := range leaks {
				if strings.Contains(out, leak) {
					t.Fatalf("with %s broken the answer carries %q: %s", tc.table, leak, out)
				}
			}
			var e struct{ Error string }
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Error == "" {
				t.Fatalf("not an error body: %s", out)
			}
		})
	}
}
