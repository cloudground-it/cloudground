//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/accounts/softkey"
)

// The panel as a browser on its own domain sees it: passkeys need a domain
// name (api SPEC §2.10), which the API socket takes from Host as it takes
// it from nginx. The section's requests come from an address of its own
// (X-Real-IP), so its password checks do not count towards unix's.
const (
	pkHost   = "passkeys-e2e.example.com"
	pkOrigin = "https://" + pkHost
	pkIP     = "192.0.2.156"
)

// pk sends one request as a browser at pkOrigin would, with the client's
// cookies: Host and Origin the panel's domain.
func (a *apiClient) pk(method, path string, body any) (int, []byte) {
	return a.pkAt(pkHost, method, path, body)
}

// pkAt is pk as a browser that reached the panel at host (Origin
// https://host).
func (a *apiClient) pkAt(host, method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		enc, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		rd = bytes.NewReader(enc)
	}
	a.h.step(method + " " + path + " (as " + host + ")")
	req, err := http.NewRequest(method, a.h.Panel+path, rd)
	if err != nil {
		a.h.root.Logf("API %s %s: %v", method, path, err)
		return 0, nil
	}
	req.Host = host
	req.Header.Set("Origin", "https://"+host)
	req.Header.Set("X-Real-IP", pkIP)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		a.h.root.Logf("API %s %s: %v", method, path, err)
		return 0, nil
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// secondFactor is a sign-in refusal's list of factors and passkey options.
type secondFactor struct {
	Factors []string        `json:"factors"`
	Passkey json.RawMessage `json:"passkey"`
}

// addSoftPasskey registers a software passkey for the client's account with
// proof, and returns it with the answer's recovery codes.
func (h *harness) addSoftPasskey(t *testing.T, c *apiClient, proof map[string]any, check string) (*softkey.Key, []string, bool) {
	st, opts := c.pk("POST", "/api/account/passkeys/begin", proof)
	if st != 200 {
		h.fail(t, check, "passkeys/begin: HTTP %d %s", st, head(opts, 300))
		return nil, nil, false
	}
	key := softkey.New()
	cred, err := key.Create(opts, pkOrigin)
	if err != nil {
		h.fail(t, check, "the software authenticator: %v", err)
		return nil, nil, false
	}
	st, fb := c.pk("POST", "/api/account/passkeys/finish", map[string]any{"name": "e2e soft key", "credential": json.RawMessage(cred)})
	h.check(t, check, code(st, fb), "201")
	var out struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	_ = json.Unmarshal(fb, &out)
	return key, out.RecoveryCodes, st == 201
}

// panelPasskeys (#156): passkeys, recovery codes and required two-factor for
// panel accounts, with a software authenticator in the suite answering the
// panel's WebAuthn ceremonies as a browser would. The section's accounts
// are its own and are deleted; Require two-factor is put back to what it
// was.
func (h *harness) panelPasskeys(t *testing.T) {
	_, before := h.api.get("/api/settings")
	if req := jsonField(before, "require_2fa"); req != "" && req != "off" {
		t.Logf("(require_2fa is %q: the section needs it off to begin with)", req)
		return
	}
	email, pw := fmt.Sprintf("e2e-passkey-%d@example.com", h.pid), fmt.Sprintf("e2e-passkey-password-%d", h.pid)
	heldEmail, heldPw := fmt.Sprintf("e2e-held-%d@example.com", h.pid), fmt.Sprintf("e2e-held-password-%d", h.pid)
	_, ub := h.api.post("/api/users", map[string]any{"email": email, "password": pw, "role": "admin"})
	_, hb := h.api.post("/api/users", map[string]any{"email": heldEmail, "password": heldPw, "role": "readonly"})
	uid, heldID := jsonField(ub, "id"), jsonField(hb, "id")
	h.assert(t, "control: the section's accounts are created", uid != "" && heldID != "", "create: %s %s", head(ub, 200), head(hb, 200))
	if uid == "" || heldID == "" {
		return
	}
	defer h.undo("delete the passkey accounts", func() {
		h.api.relogin()
		h.api.del("/api/users/" + uid)
		h.api.del("/api/users/" + heldID)
	})()

	u := newAPIClient(h, email, pw)
	st, lb := u.pk("POST", "/api/login", map[string]string{"email": email, "password": pw})
	if st != 200 {
		h.fail(t, "control: the passkey account signs in with its password", "HTTP %d %s", st, head(lb, 200))
		return
	}

	// 1. A passkey added with the password, the first factor: ten codes.
	key, codes, ok := h.addSoftPasskey(t, u, map[string]any{"password": pw}, "#156: a passkey is added with the password")
	if !ok {
		return
	}
	h.check(t, "#156: the first factor comes with ten recovery codes", fmt.Sprint(len(codes)), "10")
	_, sb := u.pk("GET", "/api/session", nil)
	h.assert(t, "#156: the account shows its passkey", contains(sb, `"passkeys":1`) && contains(sb, `"recovery_codes_left":10`), "session: %s", head(sb, 400))

	// 2. After the password, the passkey is the second factor.
	fresh := newAPIClient(h, email, pw)
	st, nb := fresh.pk("POST", "/api/login", map[string]string{"email": email, "password": pw})
	var sf secondFactor
	_ = json.Unmarshal(nb, &sf)
	h.assert(t, "#156: the password alone is refused, offering the passkey",
		st == 401 && strings.Join(sf.Factors, ",") == "passkey,recovery_code" && len(sf.Passkey) > 0, "HTTP %d %s", st, head(nb, 400))
	if len(sf.Passkey) > 0 {
		a, _ := key.Get(sf.Passkey, pkOrigin)
		h.check(t, "#156: the password and the passkey sign in",
			code(fresh.pk("POST", "/api/login", map[string]any{"email": email, "password": pw, "passkey": json.RawMessage(a)})), "200")
		h.check(t, "#156: a passkey answer is not accepted twice",
			code(newAPIClient(h, email, pw).pk("POST", "/api/login", map[string]any{"email": email, "password": pw, "passkey": json.RawMessage(a)})), "401")
	}
	_, nb = newAPIClient(h, email, pw).pk("POST", "/api/login", map[string]string{"email": email, "password": pw})
	sf = secondFactor{}
	_ = json.Unmarshal(nb, &sf)
	if len(sf.Passkey) > 0 {
		stranger := softkey.New()
		stranger.CredentialID, stranger.UserHandle = key.CredentialID, key.UserHandle
		a, _ := stranger.Get(sf.Passkey, pkOrigin)
		h.check(t, "#156: an assertion signed by another key is refused",
			code(newAPIClient(h, email, pw).pk("POST", "/api/login", map[string]any{"email": email, "password": pw, "passkey": json.RawMessage(a)})), "401")
	}

	// 3. A passkey alone, with user verification.
	alone := newAPIClient(h, "", "")
	st, opts := alone.pk("POST", "/api/login/passkey/begin", map[string]string{})
	h.assert(t, "#156: a passkey sign-in asks for user verification", st == 200 && contains(opts, `"userVerification":"required"`), "HTTP %d %s", st, head(opts, 300))
	if st == 200 {
		a, _ := key.Get(opts, pkOrigin)
		h.check(t, "#156: a passkey alone signs in", code(alone.pk("POST", "/api/login/passkey", map[string]any{"credential": json.RawMessage(a)})), "200")
		h.check(t, "control: the passkey session works", code(alone.pk("GET", "/api/account/sessions", nil)), "200")
	}
	// Reached by its IP address the panel offers no passkey.
	// (The API socket's own Host, localhost, is a secure context a browser
	// reaches through an SSH tunnel: passkeys work there, by design.)
	st, ib := newAPIClient(h, "", "").pkAt(h.IP+":8443", "POST", "/api/login/passkey/begin", map[string]string{})
	h.assert(t, "#156: no passkey sign-in away from a domain name", st == 400 && contains(ib, "domain name"), "HTTP %d %s", st, head(ib, 200))

	// 4. A recovery code signs in once, and the use is in the audit log.
	if len(codes) > 0 {
		h.check(t, "#156: a recovery code signs in",
			code(newAPIClient(h, email, pw).pk("POST", "/api/login", map[string]string{"email": email, "password": pw, "recovery_code": codes[0]})), "200")
		st, rb := newAPIClient(h, email, pw).pk("POST", "/api/login", map[string]string{"email": email, "password": pw, "recovery_code": codes[0]})
		h.assert(t, "#156: a recovery code is refused the second time", st == 401 && contains(rb, "invalid recovery code"), "HTTP %d %s", st, head(rb, 200))
		_, ab := h.api.get("/api/audit")
		h.assert(t, "#156: the recovery code's use is audited", contains(ab, "login.recovery_code") && contains(ab, email), "no login.recovery_code for %s", email)
	}

	// 5. Password sign-in off: the right password is refused, the passkey
	// still signs in; then on again.
	setPasswordSignIn := func(on bool) int {
		_, ch := u.pk("POST", "/api/account/passkeys/challenge", map[string]string{})
		a, err := key.Get(ch, pkOrigin)
		if err != nil {
			return 0
		}
		st, _ := u.pk("PUT", "/api/account/password-signin", map[string]any{"enabled": on, "password": pw, "passkey": json.RawMessage(a)})
		return st
	}
	h.check(t, "#156: password sign-in turns off with a verified passkey", fmt.Sprint(setPasswordSignIn(false)), "200")
	st, ob := newAPIClient(h, email, pw).pk("POST", "/api/login", map[string]string{"email": email, "password": pw})
	h.assert(t, "#156: with password sign-in off the right password is refused", st == 403 && contains(ob, `"password_off":true`), "HTTP %d %s", st, head(ob, 200))
	_, opts = alone.pk("POST", "/api/login/passkey/begin", map[string]string{})
	if a, err := key.Get(opts, pkOrigin); err == nil {
		h.check(t, "#156: with password sign-in off the passkey signs in", code(newAPIClient(h, "", "").pk("POST", "/api/login/passkey", map[string]any{"credential": json.RawMessage(a)})), "200")
	}
	h.check(t, "control: password sign-in turns on again", fmt.Sprint(setPasswordSignIn(true)), "200")

	// 6. Required for every account: one without two-factor signs in held,
	// is sent to the setup, reaches only it, and is freed by a passkey.
	setRequire := func(v string) int {
		st, _ := u.pk("PUT", "/api/settings", map[string]string{"require_2fa": v})
		return st
	}
	if st := setRequire("all"); st != 200 {
		h.fail(t, "control: Require two-factor for every account is switched on", "HTTP %d", st)
		return
	}
	defer h.undo("switch Require two-factor off again", func() {
		if setRequire("off") != 200 {
			h.root.Logf("could not switch Require two-factor off: run panel-api recover-admin, or turn it off with an admin that has two-factor")
		}
	})()
	held := newAPIClient(h, heldEmail, heldPw)
	st, hl := held.pk("POST", "/api/login", map[string]string{"email": heldEmail, "password": heldPw})
	h.assert(t, "#156: an account required to set up two-factor is told so at sign-in", st == 200 && contains(hl, `"enrol_required":true`), "HTTP %d %s", st, head(hl, 300))
	st, sites := held.pk("GET", "/api/sites", nil)
	h.assert(t, "#156: a held account is refused everywhere else", st == 403 && contains(sites, "required for this account"), "HTTP %d %s", st, head(sites, 200))
	h.check(t, "#156: a held account reaches its passkeys", code(held.pk("GET", "/api/account/passkeys", nil)), "200")
	if _, _, ok := h.addSoftPasskey(t, held, map[string]any{"password": heldPw}, "#156: a held account adds a passkey"); ok {
		_, hs := held.pk("GET", "/api/session", nil)
		h.assert(t, "#156: a passkey frees the held account", contains(hs, `"enrol_required":false`), "session: %s", head(hs, 300))
		h.check(t, "#156: the freed account reaches the panel", code(held.pk("GET", "/api/sites", nil)), "200")
	}
	if setRequire("off") != 200 {
		h.fail(t, "control: Require two-factor is switched off", "")
	}

	// 7. Every factor lost: recover-admin --disable-2fa takes them all away
	// and the new password signs in alone.
	r := h.run("/usr/local/bin/panel-api", "recover-admin", "--email", email, "--disable-2fa")
	m := regexp.MustCompile(`password:\s+(\S+)`).FindStringSubmatch(r.Out)
	h.assert(t, "#156: recover-admin removes the passkey and the codes", r.OK() && len(m) == 2 && strings.Contains(r.Out, "1 passkey"), "%s", r)
	if len(m) == 2 {
		h.assert(t, "#156: after recovery the new password signs in alone", newAPIClient(h, email, m[1]).login(), "sign-in refused")
	}
}
