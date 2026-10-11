package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A token automates sites, not the panel's governance (SPEC.md §1.2 step 6,
// §12): it cannot change the settings that decide the 2FA requirement,
// where backups go and what opens them, where alerts are posted, the DNS
// credential or the release channel, nor the firewall, the panel's
// certificate or its binary. The map it read, sent back, passes; acme_email
// and probe_target it may change. The session may change them all.
func TestTokensCannotGovernThePanel(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	tok := mintToken(t, admin, "script", 0)
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"notify_webhook": "https://hooks.example.com/owner"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("session sets the webhook: %d %s", resp.StatusCode, body)
	}
	before, _ := s.Store.PanelSettings()

	for key, value := range map[string]string{
		"require_2fa_admin":    "1",
		"backup_repository":    "s3:https://attacker.example.com/bucket",
		"backup_password":      "attacker-password",
		"backup_s3_access_key": "AKIAATTACKER",
		"backup_s3_secret_key": "attacker-secret",
		"backup_s3_region":     "eu-attacker-1",
		"notify_webhook":       "https://attacker.example.com/hook",
		"cloudflare_api_token": "attacker-token",
		"update_channel":       "beta",
		"update_check":         "0",
	} {
		code, body := bearer(t, ts, tok.Token, "PUT", "/api/settings", map[string]string{key: value})
		if code != http.StatusForbidden || !strings.Contains(string(body), "cannot change "+key) {
			t.Errorf("token PUT %s = %d %s, want 403", key, code, body)
		}
	}
	after, _ := s.Store.PanelSettings()
	if after != before {
		t.Fatalf("a refused token changed the settings: %+v -> %+v", before, after)
	}
	if hook, _ := s.Store.Secret(state.SecretNotifyWebhook); hook != "https://hooks.example.com/owner" {
		t.Fatalf("the webhook is now %q", hook)
	}

	// What GET showed, sent back whole, changes nothing and passes; the
	// two automation keys may change.
	_, view := admin.do("GET", "/api/settings", nil)
	var m map[string]string
	if err := json.Unmarshal(view, &m); err != nil {
		t.Fatal(err)
	}
	m["acme_email"] = "ops@example.com"
	if code, body := bearer(t, ts, tok.Token, "PUT", "/api/settings", m); code != http.StatusOK {
		t.Fatalf("token PUT of the map it read, acme_email changed = %d %s", code, body)
	}
	if ps, _ := s.Store.PanelSettings(); ps.ACMEEmail != "ops@example.com" {
		t.Fatalf("acme_email = %q", ps.ACMEEmail)
	}
	if code, _ := bearer(t, ts, tok.Token, "PUT", "/api/settings", map[string]string{"no_such_key": "x"}); code != http.StatusBadRequest {
		t.Fatalf("token PUT of an unknown key = %d, want the 400 naming it", code)
	}

	for _, p := range []string{"/api/firewall/rule", "/api/firewall/enable", "/api/panel/certificate", "/api/panel/update"} {
		if code, body := bearer(t, ts, tok.Token, "POST", p, map[string]any{}); code != http.StatusForbidden || !strings.Contains(string(body), "signed-in session") {
			t.Errorf("token POST %s = %d %s, want 403", p, code, body)
		}
	}
	// The session still governs.
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"update_channel": "beta"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("session sets the channel: %d %s", resp.StatusCode, body)
	}
}
