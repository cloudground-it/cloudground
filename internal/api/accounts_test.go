package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
)

// Two-factor over HTTP (SPEC.md §2.3, §2.4): enrolment answers the secret,
// the URI and a QR code; sign-in then asks for a code, takes one once, and
// the session list marks the caller's own session.
func TestTwoFactorOverHTTP(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)

	resp, body := c.do("POST", "/api/account/2fa/begin", map[string]string{"password": "a-long-password!"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("begin = %d %s", resp.StatusCode, body)
	}
	var en struct{ Secret, URI, QRData string }
	var raw map[string]string
	json.Unmarshal(body, &raw)
	en.Secret, en.URI, en.QRData = raw["secret"], raw["uri"], raw["qr_data"]
	if len(en.Secret) != 32 || !strings.HasPrefix(en.URI, "otpauth://totp/CloudGround:admin@example.com?") || !strings.HasPrefix(en.QRData, "data:image/png;base64,") {
		t.Fatalf("begin answered %s", body)
	}
	code := func(offset int64) string {
		v, _ := totpCode(en.Secret, uint64(time.Now().Unix()/30+offset))
		return v
	}
	if resp, body := c.do("POST", "/api/account/2fa/confirm", map[string]string{"code": code(0)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d %s", resp.StatusCode, body)
	}

	fresh := &client{t: t, base: ts.URL}
	resp, body = fresh.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!"})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"totp_required":true`) || !strings.Contains(string(body), "two-factor code required") {
		t.Fatalf("login without a code = %d %s", resp.StatusCode, body)
	}
	// One code for both sign-ins: computed again, it may fall in a later
	// step, which is a fresh code.
	signInCode := code(1)
	resp, body = fresh.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!", "totp": signInCode})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"totp_enabled":true`) {
		t.Fatalf("login with a code = %d %s", resp.StatusCode, body)
	}
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == insecureSessionName {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 86400 || cookie.Path != "/" {
		t.Fatalf("session cookie = %+v", cookie)
	}
	again := &client{t: t, base: ts.URL}
	resp, body = again.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!", "totp": signInCode})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "already been used") {
		t.Fatalf("replayed code = %d %s", resp.StatusCode, body)
	}

	resp, body = fresh.do("GET", "/api/account/sessions", nil)
	var sessions []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	json.Unmarshal(body, &sessions)
	// The session that confirmed 2FA and this sign-in, newest first.
	if resp.StatusCode != http.StatusOK || len(sessions) != 2 || !sessions[0].Current || sessions[1].Current || len(sessions[0].ID) != 12 {
		t.Fatalf("sessions = %d %s", resp.StatusCode, body)
	}
	if resp, _ := fresh.do("DELETE", "/api/account/sessions/ffffffffffff", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown = %d", resp.StatusCode)
	}
	if resp, _ := fresh.do("DELETE", "/api/account/sessions/"+sessions[1].ID, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked session = %d, want 401", resp.StatusCode)
	}
}

// Failures of an account's password, on its own routes as at sign-in, make
// its later checks wait; they never lock it (SPEC.md §2.3).
func TestAccountChangesShareTheFailureDelay(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	var mu sync.Mutex
	var waits []time.Duration
	s.accounts.Sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return nil
	}
	for i := 0; i < 10; i++ {
		if resp, _ := c.do("POST", "/api/account/password", map[string]string{"current_password": "wrong-password!!", "new_password": "another-password"}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d = %d, want 403", i, resp.StatusCode)
		}
	}
	fresh := &client{t: t, base: ts.URL}
	if resp, body := fresh.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in after ten failures = %d %s, want 200 after a wait", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 6 || waits[len(waits)-1] != 32*time.Second {
		t.Fatalf("waits %v, want 1s … 32s", waits)
	}
}

// The per-address limits count an IPv6 client's /64 and an IPv4 address.
func TestClientNetwork(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":          "203.0.113.9",
		"::ffff:203.0.113.9":   "203.0.113.9",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"fe80::1%eth0":         "fe80::/64",
		"unix":                 "unix",
	} {
		if got := clientNetwork(in); got != want {
			t.Errorf("clientNetwork(%q) = %q, want %q", in, got, want)
		}
	}
	// An IPv6 client also counts towards its /48; IPv4 has no wider net.
	for in, want := range map[string]string{
		"203.0.113.9":          "",
		"::ffff:203.0.113.9":   "",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1::/48",
		"2001:db8:1:ffff::1":   "2001:db8:1::/48",
		"unix":                 "",
	} {
		if got := clientWideNetwork(in); got != want {
			t.Errorf("clientWideNetwork(%q) = %q, want %q", in, got, want)
		}
	}
}

// A full password queue answers 503 with Retry-After.
func TestBusyAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	respondAccountErr(rec, &accounts.Error{Kind: accounts.Unavailable, Msg: accounts.ErrBusy.Error()})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}
