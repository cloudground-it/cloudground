package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/state"
)

func gitDeployOf(t *testing.T, body []byte) gitDeployView {
	t.Helper()
	var v gitDeployView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

// §15.1, §15.2: the settings and what refuses them; turning it on makes the
// hook, the secret and the deploy key and the private key is never
// answered; the secret and the key are rotated on request, and the URL goes
// with the secret.
func TestGitDeploySettings(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "git.example.com", state.SiteStatic, "", 0)
	base := "/api/sites/" + itoa(site.ID) + "/git-deploy"

	resp, body := c.do("GET", base, nil)
	v := gitDeployOf(t, body)
	if resp.StatusCode != 200 || v.Enabled || v.WebhookPath != "" || v.Secret != "" || v.Branch != "main" || len(v.Deliveries) != 0 {
		t.Fatalf("before it was turned on: %d %s", resp.StatusCode, body)
	}
	for _, rot := range []string{"/secret", "/key"} {
		if resp, _ := c.do("POST", base+rot, nil); resp.StatusCode != http.StatusConflict {
			t.Fatalf("rotating %s before it was on: %d, want 409", rot, resp.StatusCode)
		}
	}
	if resp, _ := c.do("POST", base+"/run", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("run before it was on: %d, want 409", resp.StatusCode)
	}
	for name, req := range map[string]map[string]any{
		"nothing":        {},
		"on, no repo":    {"enabled": true},
		"http":           {"enabled": true, "repo": "http://example.com/a.git"},
		"a password":     {"repo": "https://u:p@example.com/a.git"},
		"a flag":         {"repo": "-oProxyCommand=x"},
		"outside":        {"repo": "file:///etc"},
		"a bad branch":   {"repo": "https://example.com/a.git", "branch": "a..b"},
		"a long command": {"repo": "https://example.com/a.git", "build_command": strings.Repeat("x", 2000)},
		"two lines":      {"repo": "https://example.com/a.git", "build_command": "a\nb"},
	} {
		if resp, body := c.do("PUT", base, req); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, body)
		}
	}
	if _, err := s.Store.GitDeploy(site.ID); err == nil {
		t.Fatal("a refused request stored settings")
	}

	resp, body = c.do("PUT", base, map[string]any{"enabled": true, "repo": "git@example.com:acme/shop.git", "branch": "release", "build_command": "npm ci"})
	v = gitDeployOf(t, body)
	if resp.StatusCode != 200 || !v.Enabled || v.Repo != "git@example.com:acme/shop.git" || v.Branch != "release" || v.BuildCommand != "npm ci" {
		t.Fatalf("turn on: %d %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(v.WebhookPath, "/hooks/git/") || len(strings.TrimPrefix(v.WebhookPath, "/hooks/git/")) != 24 || len(v.Secret) != 64 {
		t.Fatalf("webhook %q secret %q", v.WebhookPath, v.Secret)
	}
	if !strings.HasPrefix(v.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(v.PublicKey, " cloudground-site-"+itoa(site.ID)) {
		t.Fatalf("public key %q", v.PublicKey)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(v.PublicKey)); err != nil {
		t.Fatalf("the public key does not parse: %v", err)
	}
	priv, _ := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretGitKey))
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil || !strings.HasPrefix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), strings.Fields(v.PublicKey)[0]+" "+strings.Fields(v.PublicKey)[1]) {
		t.Fatalf("the sealed private key does not match the public one: %v", err)
	}
	if strings.Contains(string(body), "PRIVATE KEY") || strings.Contains(string(body), "deploy_key") {
		t.Fatalf("the private key is in the answer: %s", body)
	}
	if got, _ := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretGitWebhook)); got != v.Secret {
		t.Fatal("the secret in the view is not the sealed one")
	}

	// A later change keeps the credentials; off keeps them too.
	resp, body = c.do("PUT", base, map[string]any{"branch": "main"})
	w := gitDeployOf(t, body)
	if resp.StatusCode != 200 || w.Branch != "main" || w.WebhookPath != v.WebhookPath || w.Secret != v.Secret || w.PublicKey != v.PublicKey || !w.Enabled {
		t.Fatalf("a later change: %d %s", resp.StatusCode, body)
	}

	// The secret and the URL change together; the old URL is gone.
	resp, body = c.do("POST", base+"/secret", nil)
	n := gitDeployOf(t, body)
	if resp.StatusCode != 200 || n.Secret == v.Secret || n.WebhookPath == v.WebhookPath || n.PublicKey != v.PublicKey {
		t.Fatalf("rotate the secret: %d %s", resp.StatusCode, body)
	}
	if _, err := s.Store.GitDeployByHook(strings.TrimPrefix(v.WebhookPath, "/hooks/git/")); err == nil {
		t.Fatal("the old hook id still finds the site")
	}
	resp, body = c.do("POST", base+"/key", nil)
	k := gitDeployOf(t, body)
	if resp.StatusCode != 200 || k.PublicKey == v.PublicKey || k.Secret != n.Secret || k.WebhookPath != n.WebhookPath {
		t.Fatalf("rotate the key: %d %s", resp.StatusCode, body)
	}

	// Audit: what was done, never a secret or the repository.
	events, _ := s.Store.ListAuditEvents(50)
	seen := map[string]bool{}
	for _, e := range events {
		if strings.HasPrefix(e.Action, "site.gitdeploy") {
			seen[e.Action] = true
			if strings.Contains(e.Detail, n.Secret) || strings.Contains(e.Detail, "example.com") {
				t.Errorf("audit detail %q", e.Detail)
			}
		}
	}
	for _, a := range []string{"site.gitdeploy", "site.gitdeploy.secret", "site.gitdeploy.key"} {
		if !seen[a] {
			t.Errorf("no audit entry %s", a)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
