package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/accounts/softkey"
)

const pkHost = "panel.example.com"

func TestRelyingPartyFromTheRequest(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		host, origin string
		want         accounts.RP
	}{
		{"panel.example.com", "https://panel.example.com", accounts.RP{ID: "panel.example.com", Origin: "https://panel.example.com"}},
		{"Panel.Example.com", "", accounts.RP{ID: "panel.example.com", Origin: "https://panel.example.com"}},
		{"panel.example.com:8443", "https://panel.example.com:8443", accounts.RP{ID: "panel.example.com", Origin: "https://panel.example.com:8443"}},
		{"203.0.113.5:8443", "https://203.0.113.5:8443", accounts.RP{}},
		{"[2001:db8::1]:8443", "", accounts.RP{}},
		{"panel.example.com", "https://evil.example.net", accounts.RP{}},
		{"panel.example.com", "http://panel.example.com", accounts.RP{}},
		{"localhost:3000", "http://localhost:3000", accounts.RP{ID: "localhost", Origin: "http://localhost:3000"}},
		{"bad_host!", "", accounts.RP{}},
	} {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := s.relyingParty(r); got != tc.want {
			t.Errorf("host %q origin %q: %+v, want %+v", tc.host, tc.origin, got, tc.want)
		}
	}
}

// passkeyFor adds a software passkey to the client's account over the API.
func passkeyFor(t *testing.T, c *client, proof map[string]any) (*softkey.Key, []string) {
	t.Helper()
	resp, opts := c.do("POST", "/api/account/passkeys/begin", proof)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("passkeys/begin = %d %s", resp.StatusCode, opts)
	}
	key := softkey.New()
	cred, err := key.Create(opts, "https://"+pkHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := c.do("POST", "/api/account/passkeys/finish", map[string]any{"name": "Laptop", "credential": json.RawMessage(cred)})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("passkeys/finish = %d %s", resp.StatusCode, body)
	}
	var out struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	_ = json.Unmarshal(body, &out)
	return key, out.RecoveryCodes
}

func TestPasskeyFlowsOverHTTP(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	admin.host = pkHost

	// At the panel's IP address there are no passkeys.
	ipClient := &client{t: t, base: ts.URL, cookies: admin.cookies}
	if resp, body := ipClient.do("POST", "/api/account/passkeys/begin", map[string]string{"password": adminPassword}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "domain name") {
		t.Fatalf("passkeys at an IP = %d %s", resp.StatusCode, body)
	}

	key, codes := passkeyFor(t, admin, map[string]any{"password": adminPassword})
	if len(codes) != accounts.RecoveryCodeCount {
		t.Fatalf("recovery codes with the first passkey: %v", codes)
	}
	if resp, body := admin.do("GET", "/api/account/passkeys", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"Laptop"`) {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}

	// Password, then the passkey.
	fresh := &client{t: t, base: ts.URL, host: pkHost}
	resp, body := fresh.do("POST", "/api/login", map[string]string{"email": adminEmail, "password": adminPassword})
	var refusal struct {
		Factors []string        `json:"factors"`
		Passkey json.RawMessage `json:"passkey"`
	}
	_ = json.Unmarshal(body, &refusal)
	if resp.StatusCode != http.StatusUnauthorized || strings.Join(refusal.Factors, ",") != "passkey,recovery_code" || len(refusal.Passkey) == 0 {
		t.Fatalf("password alone = %d %s", resp.StatusCode, body)
	}
	a, _ := key.Get(refusal.Passkey, "https://"+pkHost)
	resp, body = fresh.do("POST", "/api/login", map[string]any{"email": adminEmail, "password": adminPassword, "passkey": json.RawMessage(a)})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"enrol_required":false`) {
		t.Fatalf("password and passkey = %d %s", resp.StatusCode, body)
	}

	// A passkey alone.
	alone := &client{t: t, base: ts.URL, host: pkHost}
	resp, opts := alone.do("POST", "/api/login/passkey/begin", map[string]string{})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(opts), `"userVerification":"required"`) {
		t.Fatalf("passkey/begin = %d %s", resp.StatusCode, opts)
	}
	a, _ = key.Get(opts, "https://"+pkHost)
	if resp, body := alone.do("POST", "/api/login/passkey", map[string]any{"credential": json.RawMessage(a)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("passkey alone = %d %s", resp.StatusCode, body)
	}
	if resp, body := alone.do("POST", "/api/login/passkey", map[string]any{"credential": json.RawMessage(a)}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a replayed passkey sign-in = %d %s", resp.StatusCode, body)
	}

	// A recovery code, once.
	rc := &client{t: t, base: ts.URL}
	if resp, body := rc.do("POST", "/api/login", map[string]string{"email": adminEmail, "password": adminPassword, "recovery_code": codes[0]}); resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery code = %d %s", resp.StatusCode, body)
	}
	if resp, body := rc.do("POST", "/api/login", map[string]string{"email": adminEmail, "password": adminPassword, "recovery_code": codes[0]}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "invalid recovery code") {
		t.Fatalf("a spent recovery code = %d %s", resp.StatusCode, body)
	}
}

func TestRequirementForEveryAccount(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	admin.host = pkHost
	passkeyFor(t, admin, map[string]any{"password": adminPassword})
	if resp, body := admin.do("POST", "/api/users", map[string]any{"email": "ro@example.com", "password": adminPassword, "role": "readonly"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	// Admins only: a read-only account is not held.
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa": "admins"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("require admins = %d %s", resp.StatusCode, body)
	}
	ro := signIn(t, ts, "ro@example.com", adminPassword)
	if resp, _ := ro.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("a read-only account held by a requirement for admins")
	}
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa": "all"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("require all = %d %s", resp.StatusCode, body)
	}
	if _, body := admin.do("GET", "/api/settings", nil); !strings.Contains(string(body), `"require_2fa":"all"`) || !strings.Contains(string(body), `"require_2fa_admin":"1"`) {
		t.Fatalf("settings: %s", body)
	}
	ro = &client{t: t, base: ts.URL, host: pkHost}
	resp, body := ro.do("POST", "/api/login", map[string]string{"email": "ro@example.com", "password": adminPassword})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"enrol_required":true`) {
		t.Fatalf("held sign-in = %d %s", resp.StatusCode, body)
	}
	if resp, body := ro.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), msgSecondFactorRequired) {
		t.Fatalf("held account elsewhere = %d %s", resp.StatusCode, body)
	}
	if resp, _ := ro.do("GET", "/api/account/passkeys", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("a held account cannot list its passkeys")
	}
	passkeyFor(t, ro, map[string]any{"password": adminPassword})
	if resp, body := ro.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("after adding a passkey = %d %s", resp.StatusCode, body)
	}

	// The older key: agreeing with require_2fa it is ignored, disagreeing
	// refused; alone, off turns the requirement off.
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa": "all", "require_2fa_admin": ""}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("disagreeing keys = %d %s", resp.StatusCode, body)
	}
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa_admin": "1"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy on = %d %s", resp.StatusCode, body)
	}
	if _, body := admin.do("GET", "/api/settings", nil); !strings.Contains(string(body), `"require_2fa":"all"`) {
		t.Fatalf("legacy on changed all: %s", body)
	}
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa": "sometimes"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad value = %d %s", resp.StatusCode, body)
	}
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"require_2fa_admin": ""}); resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy off = %d %s", resp.StatusCode, body)
	}
	if _, body := admin.do("GET", "/api/settings", nil); !strings.Contains(string(body), `"require_2fa":"off"`) {
		t.Fatalf("legacy off: %s", body)
	}

	// An admin resets the read-only account's factors.
	u, _ := s.Store.GetUserByEmail("ro@example.com")
	if resp, body := admin.do("POST", fmt.Sprintf("/api/users/%d/2fa/reset", u.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("2fa reset = %d %s", resp.StatusCode, body)
	}
	if got, _ := s.Store.GetUser(u.ID); got.HasSecondFactor() {
		t.Fatal("factors left after the reset")
	}
}
