//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// cgctlBinary is where the installer puts cgctl.
const cgctlBinary = "/usr/local/bin/cgctl"

// cgctlTokens (R-4.6): the installed cgctl, authenticated with an API token
// an admin created, runs a task with --wait and its exit code follows the
// task; the actions are audited under the token; a revoked token is refused,
// by cgctl and by the API, even next to a valid session cookie.
func (h *harness) cgctlTokens(t *testing.T) {
	h.withFeature(t, func(f *feat) { f.cgctlTokens(t) })
}

func (f *feat) cgctlTokens(t *testing.T) {
	h := f.h
	name := fmt.Sprintf("e2e-%d", h.pid)
	c, b := h.api.post("/api/tokens", map[string]any{"name": name, "expires_in_days": 1})
	var created struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(b, &created)
	h.check(t, "R-4.6: an admin creates an API token", fmt.Sprint(c), "201")
	if created.Token == "" {
		return
	}
	revoke := h.undo("revoke the e2e API token", func() { h.api.del(fmt.Sprintf("/api/tokens/%d", created.APIToken.ID)) })

	// The token goes in a 0600 file, not on a command line ps can show.
	tokenFile := fmt.Sprintf("/root/cg-e2e-token-%d", h.pid)
	if err := os.WriteFile(tokenFile, []byte(created.Token+"\n"), 0o600); err != nil {
		h.fail(t, "R-4.6: write the token file", "%v", err)
		return
	}
	h.undo("remove the e2e token file", func() { _ = os.Remove(tokenFile) })
	cgctl := func(args ...string) result {
		env := []string{"-u", "CLOUDGROUND_EMAIL", "-u", "CLOUDGROUND_PASSWORD", "-u", "CLOUDGROUND_TOKEN",
			"-u", "CLOUDGROUND_CONFIG", "CLOUDGROUND_TOKEN_FILE=" + tokenFile, cgctlBinary}
		return h.run("env", append(env, args...)...)
	}

	r := cgctl("sites", "list")
	h.assert(t, "R-4.6: cgctl with the token lists the sites", r.OK() && strings.Contains(r.Out, f.fs), "%s", r)

	r = cgctl("--wait", "warm", fmt.Sprint(f.fid))
	h.assert(t, "R-4.6: cgctl --wait on a task that succeeds exits 0", r.OK() && strings.Contains(r.Out, `"succeeded"`), "%s", r)

	missing := fmt.Sprintf("cg-e2e-missing-%d.sql", h.pid)
	r = cgctl("--wait", "database", "import", fmt.Sprint(f.fid), f.fs, missing)
	h.assert(t, "R-4.6: cgctl --wait on a task that fails exits non-zero", r.Code == 1 && strings.Contains(r.Err, "failed"), "%s", r)
	var printed struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(r.Out), &printed)
	_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", printed.ID))
	h.check(t, "R-4.6: the panel recorded that task as failed", jsonField(tb, "status"), "failed")
	f.taskAlertCarriesKind(t, "database.import")

	_, audit := h.api.get("/api/audit")
	h.assert(t, "R-4.6: actions taken with the token are audited under it",
		strings.Contains(string(audit), fmt.Sprintf(`(API token \"%s\" #%d)`, name, created.APIToken.ID)), "audit: %s", head(audit, 400))

	// A token cannot make itself permanent: no new admin, no new token.
	evil := fmt.Sprintf("cg-e2e-token-admin-%d@example.com", h.pid)
	c, _ = h.api.do("POST", "/api/users", map[string]any{"email": evil, "password": "a-long-password-123", "role": "admin"},
		"Authorization: Bearer "+created.Token)
	h.check(t, "R-4.6: a token cannot create a user", fmt.Sprint(c), "403")
	c, _ = h.api.do("POST", "/api/tokens", map[string]any{"name": "child"}, "Authorization: Bearer "+created.Token)
	h.check(t, "R-4.6: a token cannot create a token", fmt.Sprint(c), "403")
	_, users := h.api.get("/api/users")
	h.assert(t, "R-4.6: no account was created by the token", !strings.Contains(string(users), evil), "users: %s", head(users, 300))

	// Nor can it open a shell (R-4.5): the site's SSH shell switch and a
	// "shell" key are an admin's session's. An SFTP key it may add.
	c, _ = h.api.do("POST", f.api("/ssh-shell"), map[string]any{"enable": true}, "Authorization: Bearer "+created.Token)
	h.check(t, "R-4.5: a token cannot turn a site's shell on", fmt.Sprint(c), "403")
	_, sb := h.api.get(f.api(""))
	h.assert(t, "R-4.5: the token left the site's shell off", !strings.Contains(string(sb), `"shell_enabled":true`), "site: %s", head(sb, 400))
	kdir, err := os.MkdirTemp("", "cg-e2e-tokkey-")
	if err == nil {
		h.undo("remove the token's SSH key fixture", func() { _ = os.RemoveAll(kdir) })
		h.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "cg-e2e-token", "-f", kdir+"/k")
		pub := readTrim(kdir + "/k.pub")
		c, _ = h.api.do("POST", f.api("/ssh-keys"), map[string]any{"public_key": pub, "access": "shell"}, "Authorization: Bearer "+created.Token)
		h.check(t, "R-4.5: a token cannot add a shell key", fmt.Sprint(c), "403")
		_, kb := h.api.get(f.api("/ssh-keys"))
		h.assert(t, "R-4.5: no key from the token's refused request", !strings.Contains(string(kb), "cg-e2e-token"), "keys: %s", head(kb, 400))
	}

	f.cgctlPasswordSession(t, name)
	f.tokenGovernance(t, created.Token)

	c, _ = h.api.del(fmt.Sprintf("/api/tokens/%d", created.APIToken.ID))
	h.check(t, "R-4.6: the token is revoked", fmt.Sprint(c), "200")
	revoke()
	r = cgctl("sites", "list")
	h.assert(t, "R-4.6: cgctl with a revoked token is refused", r.Code == 1 && strings.Contains(r.Err, "HTTP 401"), "%s", r)
	c, _ = h.api.do("GET", "/api/sites", nil, "Authorization: Bearer "+created.Token)
	h.check(t, "R-4.6: the API refuses the revoked token, session cookie or not", fmt.Sprint(c), "401")
}

// taskAlertCarriesKind (W3): the alert of a failed task names the job's
// kind and its error apart (task_kind, detail), for the dashboard to show
// in the reader's language; it parsed them out of the English message.
func (f *feat) taskAlertCarriesKind(t *testing.T, kind string) {
	h := f.h
	var found bool
	var last []byte
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline) && !found; time.Sleep(5 * time.Second) {
		_, b := h.api.do("GET", "/api/system/alerts", nil, "Accept-Language: it")
		last = b
		var alerts []struct {
			Kind     string `json:"kind"`
			SiteID   int64  `json:"site_id"`
			TaskKind string `json:"task_kind"`
			Detail   string `json:"detail"`
		}
		_ = json.Unmarshal(b, &alerts)
		for _, a := range alerts {
			found = found || (a.Kind == "task" && a.SiteID == f.fid && a.TaskKind == kind && a.Detail != "")
		}
	}
	h.assert(t, "W3: the failed task's alert names its kind and error apart", found, "alerts: %s", head(last, 600))
}

// cgctlPasswordSession (W3 fix): on the default transport (the panel's unix
// socket) an email and password sign-in carries its session to the next
// request. The session cookie is __Host-…; Secure, which the standard
// cookie jar kept off the socket's plain-HTTP URL, so every
// `cgctl tokens …` failed with 401 right after a successful sign-in.
func (f *feat) cgctlPasswordSession(t *testing.T, tokenName string) {
	h := f.h
	conf := fmt.Sprintf("/root/cg-e2e-cgctl-%d.env", h.pid)
	body := fmt.Sprintf("CLOUDGROUND_EMAIL=%s\nCLOUDGROUND_PASSWORD=%s\n", h.Email, h.PW)
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		h.fail(t, "W3: write the cgctl sign-in file", "%v", err)
		return
	}
	h.undo("remove the cgctl sign-in file", func() { _ = os.Remove(conf) })
	r := h.run("env", "-u", "CLOUDGROUND_URL", "-u", "CLOUDGROUND_EMAIL", "-u", "CLOUDGROUND_PASSWORD", "-u", "CLOUDGROUND_TOKEN",
		"-u", "CLOUDGROUND_TOKEN_FILE", "CLOUDGROUND_CONFIG="+conf, cgctlBinary, "tokens", "list")
	h.assert(t, "W3: cgctl tokens list with a password sign-in over the socket succeeds",
		r.OK() && strings.Contains(r.Out, fmt.Sprintf("%q", tokenName)), "%s", r)
}
