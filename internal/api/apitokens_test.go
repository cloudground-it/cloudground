package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/state"
)

// bearer sends one request with a token, and optionally a cookie too.
func bearer(t *testing.T, ts *httptest.Server, token, method, path string, body any, cookies ...*http.Cookie) (int, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

type createdToken struct {
	Token    string         `json:"token"`
	APIToken state.APIToken `json:"api_token"`
}

func mintToken(t *testing.T, c *client, name string, days int) createdToken {
	t.Helper()
	resp, body := c.do("POST", "/api/tokens", map[string]any{"name": name, "expires_in_days": days})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d %s", resp.StatusCode, body)
	}
	var out createdToken
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAPITokenLifecycle(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	tok := mintToken(t, admin, "deploy script", 0)

	if !regexp.MustCompile(`^cgt_[A-Za-z0-9_-]{43}$`).MatchString(tok.Token) {
		t.Fatalf("token %q", tok.Token)
	}
	if !strings.HasPrefix(tok.Token, tok.APIToken.Hint) || len(tok.APIToken.Hint) != 10 || tok.APIToken.Email != "admin@example.com" {
		t.Fatalf("token record %+v", tok.APIToken)
	}

	// Shown once: the list has the hint, never the secret or its hash.
	resp, list := admin.do("GET", "/api/tokens", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(list), tok.APIToken.Hint) ||
		strings.Contains(string(list), tok.Token) || strings.Contains(string(list), "hash") {
		t.Fatalf("list: %d %s", resp.StatusCode, list)
	}

	// The token acts as its owner, writes included.
	if code, body := bearer(t, ts, tok.Token, "GET", "/api/sites", nil); code != 200 {
		t.Fatalf("GET with token: %d %s", code, body)
	}
	if code, body := bearer(t, ts, tok.Token, "PUT", "/api/settings", map[string]string{"acme_email": "ops@example.com"}); code != 200 {
		t.Fatalf("admin write with token: %d %s", code, body)
	}
	// A token cannot mint another.
	if code, _ := bearer(t, ts, tok.Token, "POST", "/api/tokens", map[string]any{"name": "child"}); code != http.StatusForbidden {
		t.Fatalf("token minting a token: %d", code)
	}

	// Revoked: refused.
	resp, body := admin.do("DELETE", fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d %s", resp.StatusCode, body)
	}
	if code, body := bearer(t, ts, tok.Token, "GET", "/api/sites", nil); code != http.StatusUnauthorized || !strings.Contains(string(body), "revoked") {
		t.Fatalf("revoked token: %d %s", code, body)
	}
	if resp, _ := admin.do("DELETE", fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID), nil); resp.StatusCode != 404 {
		t.Fatalf("revoking twice: %d", resp.StatusCode)
	}

	// Audited: creation, a write made with the token (named in the actor),
	// and revocation.
	events, _ := s.Store.ListAuditEvents(50)
	var seen []string
	for _, e := range events {
		seen = append(seen, e.Action+"|"+e.Actor)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{"token.create|admin@example.com", "token.revoke|admin@example.com",
		fmt.Sprintf(`|admin@example.com (API token "deploy script" #%d)`, tok.APIToken.ID)} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %q:\n%s", want, joined)
		}
	}
}

func TestAPITokenRefusals(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)

	for _, bad := range []string{"cgt_" + strings.Repeat("x", 43), "not-a-token", ""} {
		if code, _ := bearer(t, ts, bad, "GET", "/api/sites", nil); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", bad, code)
		}
	}
	// An API token is judged alone: a bad one with a good cookie is still
	// refused.
	if code, _ := bearer(t, ts, "cgt_"+strings.Repeat("z", 43), "GET", "/api/sites", nil, admin.cookies...); code != http.StatusUnauthorized {
		t.Errorf("bad token plus valid cookie: %d", code)
	}
	// Any other Authorization (Basic auth of a proxy in front, another
	// system's bearer) is not the panel's: the cookie decides.
	for _, h := range []string{"Basic YWRtaW46cHc=", "Bearer eyJhbGciOi.foreign", "Digest x"} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/sites", nil)
		req.Header.Set("Authorization", h)
		for _, c := range admin.cookies {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("Authorization %q with a valid cookie: %d", h, resp.StatusCode)
		}
	}

	// Expired.
	u, _ := s.Store.GetUserByEmail("admin@example.com")
	secret := apiTokenPrefix + randomToken(32)
	if _, err := s.Store.CreateAPIToken(u.ID, "old", secret, secret[:10], time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code, _ := bearer(t, ts, secret, "GET", "/api/sites", nil); code != http.StatusUnauthorized {
		t.Errorf("expired token: %d", code)
	}

	// Scoped to admins: a token stops working when its owner is no longer
	// an enabled admin.
	hash, _ := hashPassword("another-long-password")
	other, err := s.Store.CreateUser("second@example.com", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	secret2 := apiTokenPrefix + randomToken(32)
	s.Store.CreateAPIToken(other.ID, "second", secret2, secret2[:10], time.Time{})
	if code, _ := bearer(t, ts, secret2, "GET", "/api/sites", nil); code != 200 {
		t.Fatalf("second admin's token: %d", code)
	}
	if err := s.Store.UpdateUserAccess(other.ID, "readonly", false); err != nil {
		t.Fatal(err)
	}
	if code, _ := bearer(t, ts, secret2, "GET", "/api/sites", nil); code != http.StatusUnauthorized {
		t.Errorf("token of a demoted admin: %d", code)
	}

	// Bad input.
	for _, body := range []map[string]any{{"name": ""}, {"name": strings.Repeat("n", 65)}, {"name": "x", "expires_in_days": 400}, {"name": "x", "expires_in_days": -1}} {
		if resp, _ := admin.do("POST", "/api/tokens", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create %v: %d", body, resp.StatusCode)
		}
	}

	// The two-factor requirement holds for tokens as for sessions.
	tok := mintToken(t, admin, "ci", 30)
	if tok.APIToken.ExpiresAt == nil || tok.APIToken.ExpiresAt.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("expiry %+v", tok.APIToken.ExpiresAt)
	}
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.Require2FAAdmin = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if code, _ := bearer(t, ts, tok.Token, "GET", "/api/sites", nil); code != http.StatusForbidden {
		t.Errorf("token of an admin without 2FA under the 2FA requirement: %d", code)
	}
}

// Deleting the owner deletes their tokens.
func TestAPITokensGoWithTheirOwner(t *testing.T) {
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	hash, _ := hashPassword("another-long-password")
	other, _ := s.Store.CreateUser("second@example.com", hash, "admin")
	secret := apiTokenPrefix + randomToken(32)
	s.Store.CreateAPIToken(other.ID, "x", secret, secret[:10], time.Time{})
	if err := s.Store.DeleteUser(other.ID); err != nil {
		t.Fatal(err)
	}
	if toks, _ := s.Store.ListAPITokens(); len(toks) != 0 {
		t.Fatalf("tokens left: %+v", toks)
	}
}

// Every session-only route (router_test.go's sessionOnly, which
// TestRouteTableIsWellFormed holds the table's SessionOnly rows to) answers a
// token 403 before the handler runs: a token cannot create an admin, reset a
// password, touch a second factor or mint a token, so it cannot make itself
// permanent.
func TestTokensRefusedOnSessionOnlyRoutes(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	tok := mintToken(t, admin, "script", 0)
	registered := map[string]bool{}
	for _, p := range s.routePatterns {
		registered[p] = true
	}
	for pattern := range sessionOnly {
		if !registered[pattern] {
			t.Errorf("sessionOnly lists %q, which is not a route", pattern)
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = regexp.MustCompile(`\{[a-z]+\}`).ReplaceAllString(path, "1")
		var body any
		if method != "GET" && method != "DELETE" {
			body = map[string]any{"email": "evil@example.com", "password": "a-long-password!", "role": "admin", "name": "x"}
		}
		if code, b := bearer(t, ts, tok.Token, method, path, body); code != http.StatusForbidden || !strings.Contains(string(b), "signed-in session") {
			t.Errorf("%s with a token: %d %s", pattern, code, b)
		}
	}
	if _, err := s.Store.GetUserByEmail("evil@example.com"); err == nil {
		t.Fatal("a token created an account")
	}
	// The same routes work with the session.
	if resp, _ := admin.do("GET", "/api/tokens", nil); resp.StatusCode != 200 {
		t.Fatalf("tokens list with the session: %d", resp.StatusCode)
	}
}

func tokenWorks(t *testing.T, ts *httptest.Server, secret string) bool {
	t.Helper()
	code, _ := bearer(t, ts, secret, "GET", "/api/sites", nil)
	return code == 200
}

// What ends an account's sessions revokes its tokens too.
func TestTokensRevokedWithTheSessions(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	hash, _ := hashPassword("another-long-password")
	other, _ := s.Store.CreateUser("second@example.com", hash, "admin")
	mint := func(uid int64) string {
		secret := apiTokenPrefix + randomToken(32)
		if _, err := s.Store.CreateAPIToken(uid, "t", secret, secret[:10], time.Time{}); err != nil {
			t.Fatal(err)
		}
		if !tokenWorks(t, ts, secret) {
			t.Fatal("fresh token refused")
		}
		return secret
	}
	me, _ := s.Store.GetUserByEmail("admin@example.com")

	// Own password change.
	secret := mint(me.ID)
	if resp, b := admin.do("POST", "/api/account/password", map[string]string{"current_password": "a-long-password!", "new_password": "a-newer-long-password!"}); resp.StatusCode != 200 {
		t.Fatalf("change password: %d %s", resp.StatusCode, b)
	}
	if tokenWorks(t, ts, secret) {
		t.Error("a token outlived its owner's password change")
	}
	// Admin reset of another account.
	secret = mint(other.ID)
	if resp, b := admin.do("POST", fmt.Sprintf("/api/users/%d/password", other.ID), map[string]string{"password": "reset-long-password!"}); resp.StatusCode != 200 {
		t.Fatalf("reset: %d %s", resp.StatusCode, b)
	}
	if tokenWorks(t, ts, secret) {
		t.Error("a token outlived an admin reset of its owner's password")
	}
	// Second factor off.
	secret = mint(other.ID)
	if err := s.Store.DisableUserTOTP(other.ID); err != nil {
		t.Fatal(err)
	}
	if tokenWorks(t, ts, secret) {
		t.Error("a token outlived turning its owner's 2FA off")
	}
	// Demotion, then promotion again: the token does not come back.
	secret = mint(other.ID)
	if err := s.Store.UpdateUserAccess(other.ID, "readonly", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpdateUserAccess(other.ID, "admin", false); err != nil {
		t.Fatal(err)
	}
	if tokenWorks(t, ts, secret) {
		t.Error("a token came back after its owner was demoted and promoted")
	}
	// Disabled, then enabled.
	secret = mint(other.ID)
	s.Store.UpdateUserAccess(other.ID, "admin", true)
	s.Store.UpdateUserAccess(other.ID, "admin", false)
	if tokenWorks(t, ts, secret) {
		t.Error("a token came back after its owner was disabled and enabled")
	}
	// Host recovery.
	secret = mint(other.ID)
	if err := accounts.Recover(s.Store, io.Discard, accounts.Recovery{Email: "second@example.com", Root: true}); err != nil {
		t.Fatal(err)
	}
	if tokenWorks(t, ts, secret) {
		t.Error("a token outlived recover-admin")
	}
	// An unrelated change keeps the tokens.
	secret = mint(other.ID)
	if err := s.Store.UpdateUserProfile(other.ID, "Sec", "Ond"); err != nil {
		t.Fatal(err)
	}
	if !tokenWorks(t, ts, secret) {
		t.Error("a profile change revoked a token")
	}
}

// /api/events authenticates like every route: a token opens the stream, and
// a token next to someone else's cookie streams as the token's owner.
func TestEventsWithAToken(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	tok := mintToken(t, admin, "watcher", 0)
	op := mkOperator(t, s)
	other := mkSite(t, s, "other.example.com", 0)

	st := openStream(t, ts.URL, sessionFor(t, s, op.ID), "Authorization: Bearer "+tok.Token)
	defer st.close()
	time.Sleep(50 * time.Millisecond)
	s.Events.Publish(events.SiteChanged, other.ID, map[string]any{"reason": "updated"})
	got := st.until(3*time.Second, func(e sseEvent) bool { return e.SiteID == other.ID })
	if len(got) == 0 || got[len(got)-1].SiteID != other.ID {
		t.Fatal("the stream ran as the cookie's operator, not as the token's admin")
	}

	// A revoked token with a valid cookie is refused, not run as the cookie.
	admin.do("DELETE", fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID), nil)
	if code, _ := bearer(t, ts, tok.Token, "GET", "/api/events", nil, &http.Cookie{Name: insecureSessionName, Value: sessionFor(t, s, op.ID)}); code != http.StatusUnauthorized {
		t.Fatalf("revoked token on the stream: %d", code)
	}
}
