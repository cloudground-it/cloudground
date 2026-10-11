package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// Before setup nothing is reachable; setup makes the first admin and spends
// its token; then the password signs in and a wrong one does not.
func TestFirstAdminAndSignIn(t *testing.T) {
	s, _, ts := testServer(t)
	anon := &client{t: t, base: ts.URL}
	if resp, _ := anon.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sites before sign-in = %d, want 401", resp.StatusCode)
	}

	admin := setupAdmin(t, s, ts)
	if resp, _ := admin.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("sites with the setup session = %d, want 200", resp.StatusCode)
	}
	again := map[string]string{"email": "second@example.com", "password": "another-password!", "token": "boot-token"}
	if resp, _ := anon.do("POST", "/api/setup", again); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second setup = %d, want 409", resp.StatusCode)
	}

	for _, c := range []struct {
		password string
		want     int
	}{{"wrong-password!!", http.StatusUnauthorized}, {adminPassword, http.StatusOK}} {
		fresh := &client{t: t, base: ts.URL}
		if resp, _ := fresh.do("POST", "/api/login", map[string]string{"email": adminEmail, "password": c.password}); resp.StatusCode != c.want {
			t.Errorf("sign in with %q = %d, want %d", c.password, resp.StatusCode, c.want)
		}
	}
}

// With no account yet, a setup token that expired or was never issued is
// refused by the token check itself, and makes no account.
func TestSetupRefusesExpiredAndUnknownTokens(t *testing.T) {
	s, _, ts := testServer(t)
	if err := s.Store.CreateSetupToken("old-token", -time.Hour); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: ts.URL}
	for _, token := range []string{"old-token", "never-issued"} {
		resp, body := c.do("POST", "/api/setup", map[string]string{"email": adminEmail, "password": adminPassword, "token": token})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("setup with %s = %d %s, want 403", token, resp.StatusCode, body)
		}
		if n, _ := s.Store.CountUsers(); n != 0 {
			t.Fatalf("setup with %s made %d accounts", token, n)
		}
	}
}

// A setup form with a mistake is refused without spending the token (S-L1);
// the address is stored normalised.
func TestSetupTypoKeepsTheToken(t *testing.T) {
	s, _, ts := testServer(t)
	if err := s.Store.CreateSetupToken("boot-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: ts.URL}
	for _, form := range [][2]string{
		{"not-an-email", adminPassword},
		{"Admin <admin@example.com>", adminPassword},
		{adminEmail, "short"},
	} {
		body := map[string]string{"email": form[0], "password": form[1], "token": "boot-token"}
		if resp, out := c.do("POST", "/api/setup", body); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("setup %q/%q = %d %s, want 400", form[0], form[1], resp.StatusCode, out)
		}
	}
	resp, out := c.do("POST", "/api/setup", map[string]string{"email": "  Admin@Example.COM ", "password": adminPassword, "token": "boot-token"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup after the typos = %d %s, want 201", resp.StatusCode, out)
	}
	if _, err := s.Store.GetUserByEmail(adminEmail); err != nil {
		t.Fatalf("the admin is not stored under the normalised address: %v", err)
	}
}

// A sign-in costs 64 MiB of argon2. Changing the address from one client
// buys no fresh attempts, and an oversized address is refused before any
// hashing.
func TestSignInIsRateLimited(t *testing.T) {
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	// The limits' clock stands still: on a slow runner 40 argon2 checks
	// under the race detector took longer than the window's minute, and the
	// limit was never reached.
	frozen := time.Now()
	clock := func() time.Time { return frozen }
	l := s.accounts.Limits
	for _, w := range []*throttle.Window{l.Address, l.Wide, l.AddressAccount, l.Codes} {
		w.WithClock(clock)
	}
	l.Delays.WithClock(clock)
	attempt := func(email string) int {
		body, _ := json.Marshal(map[string]string{"email": email, "password": "wrong-password-123"})
		resp, err := ts.Client().Post(ts.URL+"/api/login", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		limited = attempt(fmt.Sprintf("guess%d@example.com", i)) == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("40 sign-ins with 40 addresses from one client were never limited")
	}
	if code := attempt(strings.Repeat("x", 300) + "@example.com"); code != http.StatusBadRequest && code != http.StatusTooManyRequests {
		t.Fatalf("an oversized address = %d, want 400", code)
	}
}

// The session cookie is SameSite=Strict, which a sibling subdomain still
// sends: a write must be JSON from the panel's own origin.
func TestWritesMustBeSameOriginJSON(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	write := func(contentType string, headers map[string]string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/api/users",
			strings.NewReader(`{"email":"x@example.com","password":"aaaaaaaaaaaaaaaa","role":"admin"}`))
		req.Header.Set("Content-Type", contentType)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		for _, ck := range admin.cookies {
			req.AddCookie(ck)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	refused := []struct {
		name, contentType string
		headers           map[string]string
	}{
		{"a text/plain form", "text/plain", nil},
		{"a cross-site fetch", "application/json", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"another origin", "application/json", map[string]string{"Origin": "https://staging.example.com"}},
		{"an opaque origin", "application/json", map[string]string{"Origin": "null"}},
		{"octet-stream outside uploads", "application/octet-stream", nil},
	}
	for _, c := range refused {
		// A 400 would mean the write got as far as the handler.
		if code := write(c.contentType, c.headers); code < 400 || code == http.StatusBadRequest {
			t.Errorf("%s: %d, want a refusal", c.name, code)
		}
	}
	panel, _ := url.Parse(ts.URL)
	if code := write("application/json", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://" + panel.Host}); code >= 400 && code != http.StatusConflict {
		t.Fatalf("a same-origin JSON write was refused: %d", code)
	}
}

// With require_2fa_admin on, an admin without a second factor is refused
// by every guard (manage, server-wide, admin, signed-in), but can still
// see their session and enrol; enrolling gives the same session its access
// back.
func TestAdminSecondFactorRequirement(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	if resp, _ := admin.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("before the requirement, an admin without 2FA is refused")
	}
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa_admin": "1"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("turn the requirement on = %d %s", resp.StatusCode, body)
	}

	for _, rt := range [][2]string{
		{"GET", "/api/sites"},
		{"POST", "/api/sites/1/purge"},
		{"GET", "/api/audit"},
		{"PUT", "/api/settings"},
		{"GET", "/api/tasks"},
	} {
		if resp, body := admin.do(rt[0], rt[1], map[string]string{}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s = %d %s, want 403 without a second factor", rt[0], rt[1], resp.StatusCode, body)
		}
	}

	// The way to comply stays open.
	if resp, body := admin.do("GET", "/api/session", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"email"`) {
		t.Fatalf("session = %d %s, want the signed-in user", resp.StatusCode, body)
	}
	if resp, _ := admin.do("POST", "/api/account/2fa/begin", map[string]string{}); resp.StatusCode != http.StatusForbidden {
		t.Fatal("an enrolment began without the password")
	}
	if resp, body := admin.do("POST", "/api/account/2fa/begin", map[string]string{"password": adminPassword}); resp.StatusCode != http.StatusOK {
		t.Fatalf("enrolment = %d %s, want it reachable", resp.StatusCode, body)
	}

	// Enrolled, the same session works again: the session lookup reads
	// the account's totp_enabled.
	u, err := s.Store.GetUserByEmail(adminEmail)
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := s.Store.PendingTOTP(u.ID)
	if err := s.Store.ActivatePendingTOTP(u.ID, pending); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/sites", "/api/audit", "/api/system/status"} {
		if resp, body := admin.do("GET", path, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("enrolled: GET %s = %d %s", path, resp.StatusCode, body)
		}
	}
}

// enrolTOTP turns the second factor on for the account and returns a
// function giving the code of the time-step offset from now.
func enrolTOTP(t *testing.T, s *Server, email string) func(offset int64) string {
	t.Helper()
	u, err := s.Store.GetUserByEmail(email)
	if err != nil {
		t.Fatal(err)
	}
	secret := newTOTPSecret()
	if err := s.Store.SetPendingTOTP(u.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.ActivatePendingTOTP(u.ID, secret); err != nil {
		t.Fatal(err)
	}
	return func(offset int64) string {
		code, _ := totpCode(secret, uint64(time.Now().Unix()/30+offset))
		return code
	}
}

// Someone else failing an account's sign-ins slows it down but never keeps
// out its owner with the second factor; turning 2FA off takes a code, and
// not one already used.
func TestTwoFactorOwnerAndDisable(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	code := enrolTOTP(t, s, adminEmail)

	s.accounts.Sleep = func(context.Context, time.Duration) error { return nil } // the delay is not waited out here
	for range 12 {
		s.accounts.Limits.Delays.Fail(adminEmail, "192.0.2.1") // from another network
	}
	used := code(0)
	owner := &client{t: t, base: ts.URL}
	if resp, body := owner.do("POST", "/api/login", map[string]string{"email": adminEmail, "password": adminPassword, "totp": used}); resp.StatusCode != http.StatusOK {
		t.Fatalf("the 2FA owner was kept out by another's failures: %d %s", resp.StatusCode, body)
	}

	for _, body := range []map[string]string{
		{"password": adminPassword},
		{"password": adminPassword, "code": used},
	} {
		if resp, _ := admin.do("POST", "/api/account/2fa/disable", body); resp.StatusCode == http.StatusOK {
			t.Fatalf("2FA was turned off with %v", body)
		}
	}
}

// A read-only account sees neither a repository's credentials in the backup
// list nor the webhook in the settings.
func TestReadOnlySeesNoCredentials(t *testing.T) {
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, "https://hooks.example.com/services/T0/B0/hookpart"); err != nil {
		t.Fatal(err)
	}
	site := seedActiveSite(t, s, "readonly.example.com", state.SiteStatic, "", 0)
	if _, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abc", Kind: "full",
		Repository: "rest:https://backup:repopass@backups.example.com/repo"}); err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("viewer-long-password")
	if _, err := s.Store.CreateUser("viewer@example.com", hash, "readonly"); err != nil {
		t.Fatal(err)
	}
	viewer := signIn(t, ts, "viewer@example.com", "viewer-long-password")
	for path, secret := range map[string]string{
		fmt.Sprintf("/api/sites/%d/backups", site.ID): "repopass",
		"/api/settings": "hookpart",
	} {
		if _, body := viewer.do("GET", path, nil); strings.Contains(string(body), secret) {
			t.Errorf("%s shows a read-only account %q: %s", path, secret, body)
		}
	}
}

// /api/session answers 200 with a null user before sign-in and the user
// after.
func TestSessionEndpointBeforeAndAfterSignIn(t *testing.T) {
	s, _, ts := testServer(t)
	anon := &client{t: t, base: ts.URL}
	if resp, body := anon.do("GET", "/api/session", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"user":null`) {
		t.Fatalf("anonymous session = %d %s", resp.StatusCode, body)
	}
	admin := setupAdmin(t, s, ts)
	if _, body := admin.do("GET", "/api/session", nil); !strings.Contains(string(body), adminEmail) {
		t.Fatalf("signed-in session = %s", body)
	}
}

// An alert someone can see, they can dismiss, and it leaves the list.
func TestAlertDismissal(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "alerting.example.com", state.SiteStatic, "", 0)
	if err := s.Store.SetCertError(site.ID, "rate limited"); err != nil {
		t.Fatal(err)
	}
	evaluateAt(t, s, time.Now()) // the list reads what the evaluator wrote
	var listed []Alert
	_, body := admin.do("GET", "/api/system/alerts", nil)
	if err := json.Unmarshal(body, &listed); err != nil || len(listed) == 0 || listed[0].ID == "" {
		t.Fatalf("alerts = %s (%v)", body, err)
	}
	id := listed[0].ID
	if resp, body := admin.do("POST", "/api/system/alerts/"+id+"/dismiss", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss = %d %s", resp.StatusCode, body)
	}
	if _, body := admin.do("GET", "/api/system/alerts", nil); strings.Contains(string(body), id) {
		t.Fatalf("the dismissed alert is still listed: %s", body)
	}
}

// An admin creates an account with a name, edits it, disables it (which
// ends its session and keeps it out), enables it and sets its password;
// the last admin cannot demote themselves; a user edits their own name.
func TestAccountAdministration(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	resp, body := admin.do("POST", "/api/users", map[string]any{"email": "ops@example.com", "password": "ops-long-password",
		"role": "readonly", "first_name": "Ada", "last_name": "Lovelace"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(body), `"first_name":"Ada"`) {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var ops state.User
	if err := json.Unmarshal(body, &ops); err != nil {
		t.Fatal(err)
	}
	opsSession := signIn(t, ts, "ops@example.com", "ops-long-password")
	edit := func(disabled bool) {
		t.Helper()
		body := map[string]any{"first_name": "Ada", "last_name": "King", "role": "readonly", "disabled": disabled}
		if resp, out := admin.do("PUT", fmt.Sprintf("/api/users/%d", ops.ID), body); resp.StatusCode != http.StatusOK {
			t.Fatalf("update (disabled=%v) = %d %s", disabled, resp.StatusCode, out)
		}
	}
	edit(true)
	if resp, _ := opsSession.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disabled account's session answered %d", resp.StatusCode)
	}
	blocked := &client{t: t, base: ts.URL}
	if resp, _ := blocked.do("POST", "/api/login", map[string]string{"email": "ops@example.com", "password": "ops-long-password"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a disabled account signed in: %d", resp.StatusCode)
	}
	edit(false)
	if resp, out := admin.do("POST", fmt.Sprintf("/api/users/%d/password", ops.ID), map[string]string{"password": "brand-new-password"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set password = %d %s", resp.StatusCode, out)
	}
	signIn(t, ts, "ops@example.com", "brand-new-password")

	me, _ := s.Store.GetUserByEmail(adminEmail)
	if resp, _ := admin.do("PUT", fmt.Sprintf("/api/users/%d", me.ID), map[string]any{"role": "readonly"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the last admin demoted themselves: %d", resp.StatusCode)
	}
	if resp, out := admin.do("PUT", "/api/account/profile", map[string]string{"first_name": "Grace", "last_name": "Hopper"}); resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "Hopper") {
		t.Fatalf("own profile = %d %s", resp.StatusCode, out)
	}
}

// A role that is not one of the three is refused, never read as admin.
func TestCreateUserRefusesUnknownRoles(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	for _, role := range []string{"viewer", "read-only", "root", ""} {
		resp, body := admin.do("POST", "/api/users", map[string]any{"email": "new@example.com", "password": "long-enough-password", "role": role})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("role %q = %d %s, want 400", role, resp.StatusCode, body)
		}
	}
}
