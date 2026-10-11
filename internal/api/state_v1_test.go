package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Secrets set through /api/settings land in the secret store, come back
// only as a presence mask, and follow the PUT contract: the exact mask keeps
// the stored value, "" clears it, anything else is the new value verbatim.
func TestSettingsSecretsContract(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	get := func() map[string]string {
		t.Helper()
		resp, body := c.do("GET", "/api/settings", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET settings = %d %s", resp.StatusCode, body)
		}
		var out map[string]string
		json.Unmarshal(body, &out)
		return out
	}
	put := func(body map[string]string) {
		t.Helper()
		if resp, b := c.do("PUT", "/api/settings", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %v = %d %s", body, resp.StatusCode, b)
		}
	}

	put(map[string]string{"cloudflare_api_token": "cf-token", "backup_s3_secret_key": "s3-secret", "notify_webhook": "https://hooks.example.com/x/secret"})
	got := get()
	if got["cloudflare_api_token"] != secretMask || got["backup_s3_secret_key"] != secretMask {
		t.Fatalf("secrets not masked: %v", got)
	}
	if got["notify_webhook"] != "https://hooks.example.com/x/secret" {
		t.Fatalf("admin sees the webhook as %q", got["notify_webhook"])
	}
	if v, _ := s.Store.Secret(state.SecretCloudflareToken); v != "cf-token" {
		t.Fatalf("stored token = %q", v)
	}

	// Sending back everything GET returned changes nothing.
	resp, body := c.do("PUT", "/api/settings", got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("round-trip PUT = %d %s", resp.StatusCode, body)
	}
	if v, _ := s.Store.Secret(state.SecretCloudflareToken); v != "cf-token" {
		t.Fatalf("round trip changed the token to %q", v)
	}

	// A value that merely contains the mask character is a value.
	put(map[string]string{"cloudflare_api_token": "new•token"})
	if v, _ := s.Store.Secret(state.SecretCloudflareToken); v != "new•token" {
		t.Fatalf("token = %q, want the new value verbatim", v)
	}

	put(map[string]string{"cloudflare_api_token": ""})
	if v, _ := s.Store.Secret(state.SecretCloudflareToken); v != "" {
		t.Fatalf("an empty value did not clear the token: %q", v)
	}
	if got := get(); got["cloudflare_api_token"] != "" {
		t.Fatalf("a cleared secret still shows as set: %q", got["cloudflare_api_token"])
	}

	// Booleans keep the strings the settings page uses.
	put(map[string]string{"update_check": "0", "require_2fa_admin": ""})
	if got := get(); got["update_check"] != "0" || got["require_2fa_admin"] != "" {
		t.Fatalf("booleans = %q %q", got["update_check"], got["require_2fa_admin"])
	}
	if ps, _ := s.Store.PanelSettings(); ps.UpdateCheck {
		t.Fatal("update_check=0 left the update check on")
	}
	if resp, _ := c.do("PUT", "/api/settings", map[string]string{"update_check": "maybe"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed boolean = %d, want 400", resp.StatusCode)
	}
}

// Deleting a site removes its secrets: they used to be blanked, and the rows
// stayed behind.
func TestSiteDeletionRemovesItsSecrets(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "secrets.example.com", "type": "php", "php_version": "8.4"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create site = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain("secrets.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{state.SiteSecretDBPassword, state.SiteSecretConnectorToken} {
		if v, err := s.Store.Secret(state.SiteSecret(site.ID, k)); err != nil || v == "" {
			t.Fatalf("new site has no %s (%v)", k, err)
		}
	}
	if resp, body := c.do("DELETE", fmt.Sprintf("/api/sites/%d", site.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	for _, k := range []string{state.SiteSecretDBPassword, state.SiteSecretConnectorToken} {
		if v, err := s.Store.Secret(state.SiteSecret(site.ID, k)); err != nil || v != "" {
			t.Fatalf("deleted site's %s = %q, %v", k, v, err)
		}
	}
}

// The site JSON carries the desired and applied generations, and a failed
// apply is recorded instead of only being reverted.
func TestSiteGenerationsInAPI(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "gen.example.com", "type": "static"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create site = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain("gen.example.com")
	if err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		t.Helper()
		resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d", site.ID), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get site = %d %s", resp.StatusCode, body)
		}
		var out map[string]any
		json.Unmarshal(body, &out)
		if inner, ok := out["site"].(map[string]any); ok {
			return inner
		}
		return out
	}
	got := read()
	if got["desired_generation"] == nil || got["desired_generation"] != got["applied_generation"] || got["last_apply_error"] != "" {
		t.Fatalf("a freshly created site is not applied: %v / %v / %q", got["desired_generation"], got["applied_generation"], got["last_apply_error"])
	}

	agent.errs[agentrpc.ApplySiteConfig.Name()] = fmt.Errorf("simulated render failure")
	if resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"cache_ttl_sec": 77}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update config = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	got = read()
	if got["last_apply_error"] != "simulated render failure" {
		t.Fatalf("last_apply_error = %q after a failed apply", got["last_apply_error"])
	}
	if got["desired_generation"] == got["applied_generation"] {
		t.Fatalf("a failed apply reads as applied: %v / %v", got["desired_generation"], got["applied_generation"])
	}

	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	if resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"cache_ttl_sec": 78}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update config = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	got = read()
	if got["desired_generation"] != got["applied_generation"] || got["last_apply_error"] != "" {
		t.Fatalf("after a successful apply: %v / %v / %q", got["desired_generation"], got["applied_generation"], got["last_apply_error"])
	}
}
