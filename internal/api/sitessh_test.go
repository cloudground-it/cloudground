package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// R-4.5: the shell switch reaches the agent with the site as stored now and
// is stored once the agent has answered; a key's type reaches the agent.
func TestSiteShellAndKeyTypes(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "shell.example.com")
	base := fmt.Sprintf("/api/sites/%d", site.ID)

	// SFTP on first: the agent must see it with the shell request.
	if resp, body := c.do("POST", base+"/sftp", map[string]any{"enable": true, "password": "a-long-sftp-password"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sftp = %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	agent.results[agentrpc.SetShell.Name()] = map[string]string{"shell": "enabled", "user": "cg-shell-1"}
	agent.mu.Unlock()
	resp, body := c.do("POST", base+"/ssh-shell", map[string]any{"enable": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ssh-shell = %d %s", resp.StatusCode, body)
	}
	p, ok := agent.last(agentrpc.SetShell.Name()).(rpc.ShellParams)
	if !ok || !p.Enable || !p.Site.Config.SFTPEnabled {
		t.Fatalf("agent got %+v", agent.last(agentrpc.SetShell.Name()))
	}
	got, _ := s.Store.GetSite(site.ID)
	if !got.Config.ShellEnabled {
		t.Fatal("shell_enabled not stored")
	}
	if resp, _ := c.do("POST", base+"/ssh-shell", map[string]any{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no enable = %d", resp.StatusCode)
	}

	// The agent refusing leaves the switch as it was.
	agent.mu.Lock()
	agent.errs[agentrpc.SetShell.Name()] = agentrpc.Conflict("no")
	agent.mu.Unlock()
	if resp, _ := c.do("POST", base+"/ssh-shell", map[string]any{"enable": false}); resp.StatusCode == http.StatusOK {
		t.Fatal("a refused change answered 200")
	}
	if got, _ := s.Store.GetSite(site.ID); !got.Config.ShellEnabled {
		t.Fatal("a refused change was stored")
	}

	// Key types.
	if resp, body := c.do("POST", base+"/ssh-keys", map[string]any{"public_key": "ssh-ed25519 AAAA x", "access": "shell"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d %s", resp.StatusCode, body)
	}
	if kp := agent.last(agentrpc.SSHKeys.Name()).(rpc.SSHKeyParams); kp.Access != "shell" || kp.Action != "add" {
		t.Fatalf("agent got %+v", kp)
	}
	if resp, _ := c.do("POST", base+"/ssh-keys", map[string]any{"public_key": "ssh-ed25519 AAAA x", "access": "root"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad access = %d", resp.StatusCode)
	}
}

// R-4.5: an operator may add SFTP-only keys to its site, never a shell key.
func TestShellKeysAreAdmins(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createWPSite(t, s, admin, "opshell.example.com")
	base := fmt.Sprintf("/api/sites/%d", site.ID)
	if resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	if resp, body := op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator login = %d %s", resp.StatusCode, body)
	}
	if resp, _ := op.do("POST", base+"/ssh-keys", map[string]any{"public_key": "ssh-ed25519 AAAA x", "access": "shell"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator shell key = %d, want 403", resp.StatusCode)
	}
	if resp, _ := op.do("POST", base+"/ssh-keys", map[string]any{"public_key": "ssh-ed25519 AAAA x", "access": "sftp"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("operator SFTP key = %d, want 201", resp.StatusCode)
	}
}

// R-4.5 with R-4.6: an API token, an admin's included, can neither turn a
// site's shell on nor add a shell key; it can still add an SFTP key.
func TestTokensCannotOpenAShell(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createWPSite(t, s, admin, "tokshell.example.com")
	base := fmt.Sprintf("/api/sites/%d", site.ID)
	tok := mintToken(t, admin, "script", 0)

	if code, b := bearer(t, ts, tok.Token, "POST", base+"/ssh-shell", map[string]any{"enable": true}); code != http.StatusForbidden {
		t.Fatalf("token ssh-shell = %d %s, want 403", code, b)
	}
	if agent.called(agentrpc.SetShell.Name()) {
		t.Fatal("a token's shell switch reached the agent")
	}
	if code, b := bearer(t, ts, tok.Token, "POST", base+"/ssh-keys", map[string]any{"public_key": "ssh-ed25519 AAAA x", "access": "shell"}); code != http.StatusForbidden {
		t.Fatalf("token shell key = %d %s, want 403", code, b)
	}
	if agent.called(agentrpc.SSHKeys.Name()) {
		t.Fatal("a token's shell key reached the agent")
	}
	if code, b := bearer(t, ts, tok.Token, "POST", base+"/ssh-keys", map[string]any{"public_key": "ssh-ed25519 AAAA x"}); code != http.StatusCreated {
		t.Fatalf("token SFTP key = %d %s, want 201", code, b)
	}
	if kp := agent.last(agentrpc.SSHKeys.Name()).(rpc.SSHKeyParams); kp.Access != "sftp" {
		t.Fatalf("agent got %+v", kp)
	}
}

// #47: SFTP keys only. A public key with no keys_only makes a keys-only
// login, the key added first as an SFTP key; keys only with a password is
// refused; a password login stores keys_only false.
func TestSFTPKeysOnly(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "keys.example.com")
	base := fmt.Sprintf("/api/sites/%d", site.ID)
	sftp := func(body map[string]any, want int) {
		t.Helper()
		if resp, out := c.do("POST", base+"/sftp", body); resp.StatusCode != want {
			t.Fatalf("POST sftp %v = %d %s, want %d", body, resp.StatusCode, out, want)
		}
	}
	stored := func() (bool, bool) {
		got, _ := s.Store.GetSite(site.ID)
		return got.Config.SFTPEnabled, got.Config.SFTPKeysOnly
	}

	agent.forget()
	sftp(map[string]any{"enable": true, "public_key": "ssh-ed25519 AAAA dev"}, http.StatusOK)
	kp, _ := agent.last(agentrpc.SSHKeys.Name()).(rpc.SSHKeyParams)
	if kp.Action != "add" || kp.Access != "sftp" || kp.PublicKey != "ssh-ed25519 AAAA dev" {
		t.Fatalf("key: agent got %+v", kp)
	}
	if p, _ := agent.last(agentrpc.SetSFTP.Name()).(rpc.SFTPParams); !p.Enable || !p.KeysOnly || p.Password != "" {
		t.Fatalf("SetSFTP: agent got %+v", p)
	}
	agent.mu.Lock()
	calls := slices.Clone(agent.calls)
	agent.mu.Unlock()
	if i, j := slices.Index(calls, agentrpc.SSHKeys.Name()), slices.Index(calls, agentrpc.SetSFTP.Name()); i < 0 || i > j {
		t.Fatalf("the key is not added before the login opens: %v", calls)
	}
	if on, keys := stored(); !on || !keys {
		t.Fatalf("stored enabled %v, keys only %v", on, keys)
	}

	// Refused before the agent is asked.
	agent.forget()
	sftp(map[string]any{"enable": true, "keys_only": true, "password": "a-long-sftp-password"}, http.StatusBadRequest)
	sftp(map[string]any{"enable": true, "keys_only": false, "public_key": "ssh-ed25519 AAAA dev"}, http.StatusBadRequest)
	sftp(map[string]any{"enable": true}, http.StatusBadRequest)
	if agent.called(agentrpc.SetSFTP.Name()) || agent.called(agentrpc.SSHKeys.Name()) {
		t.Fatal("a refused request reached the agent")
	}

	// Back to a password, with a key kept: keys_only false is stored.
	sftp(map[string]any{"enable": true, "keys_only": false, "password": "a-long-sftp-password", "public_key": "ssh-ed25519 AAAA dev"}, http.StatusOK)
	if p, _ := agent.last(agentrpc.SetSFTP.Name()).(rpc.SFTPParams); p.KeysOnly || p.Password != "a-long-sftp-password" {
		t.Fatalf("SetSFTP: agent got %+v", p)
	}
	if on, keys := stored(); !on || keys {
		t.Fatalf("stored enabled %v, keys only %v", on, keys)
	}

	// Explicit keys only, no key given; a key already there is fine.
	agent.mu.Lock()
	agent.errs[agentrpc.SSHKeys.Name()] = agentrpc.Conflict("SSH key already exists")
	agent.mu.Unlock()
	sftp(map[string]any{"enable": true, "keys_only": true, "public_key": "ssh-ed25519 AAAA dev"}, http.StatusOK)
	if on, keys := stored(); !on || !keys {
		t.Fatalf("stored enabled %v, keys only %v", on, keys)
	}
	// Any other refusal of the key stops there.
	agent.mu.Lock()
	agent.errs[agentrpc.SSHKeys.Name()] = agentrpc.Invalid("not a key")
	agent.mu.Unlock()
	agent.forget()
	sftp(map[string]any{"enable": true, "public_key": "nonsense"}, http.StatusBadRequest)
	if agent.called(agentrpc.SetSFTP.Name()) {
		t.Fatal("SFTP was turned on after its key was refused")
	}
}
