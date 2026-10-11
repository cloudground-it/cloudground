//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
)

const stateDB = "/var/lib/cloudground/state.db"

// usersTableProbe reads the panel's database read-only and reports the
// users table's columns, whether any of its values contains the secret, the
// secret-store names of the account and whether any sealed value contains
// the secret in the clear.
const usersTableProbe = `
import json, sqlite3, sys
db, uid, secret = sys.argv[1], int(sys.argv[2]), sys.argv[3]
c = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
cols = [r[1] for r in c.execute("PRAGMA table_info(users)")]
held = any(secret in str(v) for row in c.execute("SELECT * FROM users") for v in row)
names = sorted(r[0] for r in c.execute("SELECT name FROM secrets WHERE name LIKE ?", ("user.%d.%%" % uid,)))
plain = any(secret.encode() in bytes(r[0]) for r in c.execute("SELECT sealed FROM secrets"))
print(json.dumps({"columns": cols, "in_users": held, "names": names, "sealed_plain": plain}))
`

type usersProbe struct {
	Columns     []string `json:"columns"`
	InUsers     bool     `json:"in_users"`
	Names       []string `json:"names"`
	SealedPlain bool     `json:"sealed_plain"`
}

func (h *harness) probeUsers(t *testing.T, uid, secret string) (usersProbe, bool) {
	r := h.run("python3", "-I", "-c", usersTableProbe, stateDB, uid, secret)
	var p usersProbe
	if !r.OK() || json.Unmarshal([]byte(r.Trim()), &p) != nil {
		h.fail(t, "R-4.3: the users table can be read on the box", "python3 probe: %s", r)
		return p, false
	}
	return p, true
}

// accounts (R-4.3): two-factor secrets live in the encrypted secret store.
// A fresh read-only account turns 2FA on; the secret the panel showed is
// then nowhere in the database file or its WAL, nowhere in the users table
// (whose secret columns are gone), and sealed under the account's names;
// signing in asks for a code, takes one once, and turning 2FA off removes
// the sealed secrets. The account is the section's own and is deleted.
func (h *harness) accounts(t *testing.T) {
	email, pw := fmt.Sprintf("e2e-2fa-%d@example.com", h.pid), fmt.Sprintf("e2e-2fa-password-%d", h.pid)
	_, ub := h.api.post("/api/users", map[string]any{"email": email, "password": pw, "role": "readonly"})
	uid := jsonField(ub, "id")
	h.assert(t, "control: the section's account is created", uid != "", "create: %s", ub)
	if uid == "" {
		return
	}
	defer h.undo("delete the 2FA account", func() {
		h.api.relogin()
		h.api.del("/api/users/" + uid)
	})()

	u := newAPIClient(h, email, pw)
	h.assert(t, "control: the account signs in with its password", u.login(), "sign-in failed")
	_, eb := u.post("/api/account/2fa/begin", map[string]string{"password": pw})
	secret := jsonField(eb, "secret")
	h.assert(t, "control: enrolment answers a secret", len(secret) == 32, "begin: %s", eb)
	if len(secret) != 32 {
		return
	}

	// The pending secret is already sealed, before confirmation.
	if p, ok := h.probeUsers(t, uid, secret); ok {
		h.assert(t, "R-4.3: a pending TOTP secret is not in the users table", !p.InUsers, "users holds the pending secret")
		h.assert(t, "R-4.3: a pending TOTP secret is sealed in the secret store", slices.Contains(p.Names, "user."+uid+".totp_pending"), "secret names %v", p.Names)
	}
	// Codes by absolute step from here, so a step boundary between two
	// requests cannot turn a spent code into a fresh one.
	s0 := time.Now().Unix() / 30
	atStep := func(step int64) string {
		c, _ := accounts.TOTPCode(secret, uint64(step))
		return c
	}
	confirmCode := atStep(s0)
	h.check(t, "R-4.3: 2FA confirms with a code", code(u.post("/api/account/2fa/confirm", map[string]string{"code": confirmCode})), "200")

	plain := false
	for _, f := range []string{stateDB, stateDB + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && contains(b, secret) {
			plain = true
			h.root.Logf("%s holds the TOTP secret in the clear", f)
		}
	}
	h.assert(t, "R-4.3: the TOTP secret is not in state.db in plaintext", !plain, "the TOTP secret is stored in plaintext")
	if p, ok := h.probeUsers(t, uid, secret); ok {
		h.assert(t, "R-4.3: the users table holds no TOTP secret", !p.InUsers, "users holds the secret")
		h.assert(t, "R-4.3: the users table has no TOTP secret column",
			!slices.Contains(p.Columns, "totp_secret") && !slices.Contains(p.Columns, "totp_pending"), "columns %v", p.Columns)
		h.check(t, "R-4.3: the active TOTP secret is sealed under the account's name", strings.Join(p.Names, ","), "user."+uid+".totp")
		h.assert(t, "R-4.3: no sealed value holds the secret in the clear", !p.SealedPlain, "a sealed value contains the secret")
	}

	// Signing in with the second factor still works, once per code.
	fresh := newAPIClient(h, email, pw)
	st, nb := fresh.post("/api/login", map[string]string{"email": email, "password": pw})
	h.check(t, "R-4.3: sign-in without a code asks for one", code(st, nb), "401")
	h.assert(t, "R-4.3: the refusal says a code is required", contains(nb, `"totp_required":true`), "body %s", nb)
	// W3 (security): the code that confirmed the enrolment is spent. The
	// activation reset the replay watermark, so it signed in once more.
	st, cb := newAPIClient(h, email, pw).post("/api/login", map[string]string{"email": email, "password": pw, "totp": confirmCode})
	h.assert(t, "W3: the code that confirmed the enrolment cannot sign in", st == 401 && contains(cb, "already been used"), "HTTP %d: %s", st, cb)
	now := atStep(s0 + 1)
	h.check(t, "R-4.3: sign-in with a code from the sealed secret", code(fresh.post("/api/login", map[string]string{"email": email, "password": pw, "totp": now})), "200")
	h.check(t, "control: the 2FA session works", code(fresh.get("/api/account/sessions")), "200")
	replay := newAPIClient(h, email, pw)
	st, rb := replay.post("/api/login", map[string]string{"email": email, "password": pw, "totp": now})
	h.check(t, "R-4.3: a used code is refused", code(st, rb), "401")

	// Off again: the password and a fresh code; the sealed secrets go. The
	// steps up to s0+1 are spent: wait until s0+2 is within the window.
	if wait := time.Until(time.Unix((s0+1)*30+1, 0)); wait > 0 {
		time.Sleep(wait)
	}
	h.check(t, "R-4.3: 2FA turns off with the password and a fresh code",
		code(fresh.post("/api/account/2fa/disable", map[string]string{"password": pw, "code": atStep(s0 + 2)})), "200")
	if p, ok := h.probeUsers(t, uid, secret); ok {
		h.check(t, "R-4.3: turning 2FA off removes its sealed secrets", strings.Join(p.Names, ","), "")
	}
	h.assert(t, "R-4.3: without 2FA the password alone signs in again", newAPIClient(h, email, pw).login(), "sign-in refused")

	h.forcedEnrolment(t)
	h.secondFactorBruteForce(t, fresh, email, pw)
	h.signInDuringAttack(t)

	// W3: recover-admin by another user says to run it as root; it failed
	// opening the database, in the database's words.
	r := h.run("sudo", "-u", "nobody", "/usr/local/bin/panel-api", "recover-admin")
	h.assert(t, "W3: recover-admin as another user says to run it as root", r.Code == 1 && strings.Contains(r.Err, "run as root"), "%s", r)
}

// forcedEnrolment (W3): with "Require 2FA for admins" on, an admin without
// 2FA is held on the Security page to enrol. The session says so
// (enrol_required, which the UI follows), and the page's session list
// answers (it got 403); tokens stay refused. The requirement is switched
// on and off by an e2e admin that has 2FA: the suite's own admin may not,
// and is held meanwhile (this section runs alone).
func (h *harness) forcedEnrolment(t *testing.T) {
	_, before := h.api.get("/api/settings")
	if jsonField(before, "require_2fa_admin") == "1" {
		t.Logf("(Require 2FA is already on: the forced-enrolment checks need it off to begin with)")
		return
	}
	mk := func(kind string) (email, pw, id string) {
		email, pw = fmt.Sprintf("e2e-%s-%d@example.com", kind, h.pid), fmt.Sprintf("e2e-%s-password-%d", kind, h.pid)
		_, ub := h.api.post("/api/users", map[string]any{"email": email, "password": pw, "role": "admin"})
		return email, pw, jsonField(ub, "id")
	}
	keeperEmail, keeperPw, keeperID := mk("keeper")
	heldEmail, heldPw, heldID := mk("held")
	h.assert(t, "control: the forced-enrolment admins are created", keeperID != "" && heldID != "", "ids %q %q", keeperID, heldID)
	if keeperID == "" || heldID == "" {
		return
	}
	defer h.undo("delete the forced-enrolment admins", func() {
		h.api.relogin()
		h.api.del("/api/users/" + keeperID)
		h.api.del("/api/users/" + heldID)
	})()

	// The keeper enrols, so it can turn the requirement on and off.
	keeper := newAPIClient(h, keeperEmail, keeperPw)
	if !keeper.login() {
		h.fail(t, "control: the keeper signs in", "sign-in refused")
		return
	}
	_, eb := keeper.post("/api/account/2fa/begin", map[string]string{"password": keeperPw})
	secret := jsonField(eb, "secret")
	c, _ := accounts.TOTPCode(secret, uint64(time.Now().Unix()/30))
	if st, b := keeper.post("/api/account/2fa/confirm", map[string]string{"code": c}); st != 200 {
		h.fail(t, "control: the keeper enrols in 2FA", "HTTP %d: %s", st, b)
		return
	}
	setRequire := func(v string) int {
		st, _ := keeper.put("/api/settings", map[string]string{"require_2fa_admin": v})
		return st
	}
	if st := setRequire("1"); st != 200 {
		h.fail(t, "control: Require 2FA is switched on", "HTTP %d", st)
		return
	}
	defer h.undo("switch Require 2FA off again", func() {
		if setRequire("") != 200 {
			h.root.Logf("could not switch Require 2FA off: run panel-api recover-admin, or turn it off with an admin that has 2FA")
		}
	})()

	held := newAPIClient(h, heldEmail, heldPw)
	h.assert(t, "control: the admin without 2FA signs in (held)", held.login(), "sign-in refused")
	_, sb := held.get("/api/session")
	h.assert(t, "W3: the session says the admin must enrol", contains(sb, `"enrol_required":true`), "session: %s", head(sb, 300))
	h.check(t, "W3: the enrolment page's session list answers", code(held.get("/api/account/sessions")), "200")
	h.check(t, "W3 control: API tokens stay refused until it enrols", code(held.get("/api/tokens")), "403")
}

// secondFactorBruteForce: wrong codes for one account wait longer and longer
// and stop at ten an hour, everywhere the account's code is checked; the
// tenth is in the audit log as login.2fa_brute. Ten wrong codes go through
// "turn 2FA off" (a session route: the per-address-and-e-mail sign-in limit
// does not get in the way; the per-address one, 30 a minute, counts them), then the right code is refused there and at sign-in.
func (h *harness) secondFactorBruteForce(t *testing.T, u *apiClient, email, pw string) {
	_, eb := u.post("/api/account/2fa/begin", map[string]string{"password": pw})
	secret := jsonField(eb, "secret")
	if len(secret) != 32 {
		h.fail(t, "control: 2FA turns on again for the code cap", "begin: %s", eb)
		return
	}
	at := func(offset int64) string {
		c, _ := accounts.TOTPCode(secret, uint64(time.Now().Unix()/30+offset))
		return c
	}
	h.check(t, "control: 2FA confirms again", code(u.post("/api/account/2fa/confirm", map[string]string{"code": at(0)})), "200")
	wrong := func() string {
		for _, c := range []string{"000000", "111111", "222222", "333333"} {
			if c != at(-1) && c != at(0) && c != at(1) && c != at(2) {
				return c
			}
		}
		return "444444"
	}
	var took []time.Duration
	for i := 0; i < accounts.CodeCap; i++ {
		start := time.Now()
		st, b := u.post("/api/account/2fa/disable", map[string]string{"password": pw, "code": wrong()})
		took = append(took, time.Since(start))
		if st != 403 {
			h.fail(t, "R-4.3: a wrong code does not turn 2FA off", "attempt %d: %d %s", i+1, st, b)
			return
		}
	}
	h.root.Logf("wrong-code attempts took %v", took)
	h.assert(t, "R-4.3: after five failures, an attempt for the account waits (1 s, then doubling)",
		took[5] >= time.Second && took[9] >= 16*time.Second, "attempt times %v", took)
	st, b := u.post("/api/account/2fa/disable", map[string]string{"password": pw, "code": at(0)})
	h.check(t, "R-4.3: at ten wrong codes in an hour the right one is refused", code(st, b), "429")
	h.assert(t, "R-4.3: the refusal names the code cap", contains(b, "too many wrong two-factor codes"), "body %s", b)
	st, b = newAPIClient(h, email, pw).post("/api/login", map[string]string{"email": email, "password": pw, "totp": at(0)})
	h.check(t, "R-4.3: sign-in with the right code is refused at the cap", code(st, b), "429")
	_, ab := h.api.get("/api/audit")
	h.assert(t, "R-4.3: the code cap is audited as login.2fa_brute",
		contains(ab, "login.2fa_brute") && contains(ab, email), "audit log has no login.2fa_brute for %s", email)
}

// Addresses the attack checks give themselves (documentation ranges). On
// the box every e2e request comes over the API socket, whose X-Real-IP is
// the client (internal/api/SPEC.md §2.7): that is how one run stands for
// several networks.
const (
	attackSeeder = "192.0.2.67"
	attackerAddr = "192.0.2.66"
	attackedAddr = "198.51.100.66"
)

func fromAddr(addr string) string { return "X-Real-IP: " + addr }

// signInDuringAttack (S-H3 review): someone failing an account's password
// from one network, with as many attempts in flight as the limits let
// them, slows their own guesses and does not keep the owner out. Five
// failures are on record (from a third address), the attacker has four
// wrong passwords in flight; the owner, from another network, signs in
// within 8 s, never 429. The build before slept every delay inside the
// account's queue of four, so the owner got 429 for the attacker's backlog
// (or waited behind it some 15 s). The attacker's own attempts are still
// delayed after the owner's sign-in.
func (h *harness) signInDuringAttack(t *testing.T) {
	email, pw := fmt.Sprintf("e2e-attacked-%d@example.com", h.pid), fmt.Sprintf("e2e-attacked-password-%d", h.pid)
	_, ub := h.api.post("/api/users", map[string]any{"email": email, "password": pw, "role": "readonly"})
	uid := jsonField(ub, "id")
	h.assert(t, "control: the attacked account is created", uid != "", "create: %s", ub)
	if uid == "" {
		return
	}
	defer h.undo("delete the attacked account", func() {
		h.api.relogin()
		h.api.del("/api/users/" + uid)
	})()
	wrong := map[string]string{"email": email, "password": "wrong-password-e2e"}
	for i := range 5 {
		if c, b := newAPIClient(h, "", "").do("POST", "/api/login", wrong, fromAddr(attackSeeder)); c != 401 {
			h.fail(t, "control: a wrong password is refused", "attempt %d: %d %s", i+1, c, head(b, 200))
			return
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = newAPIClient(h, "", "").do("POST", "/api/login", wrong, fromAddr(attackerAddr))
		}()
	}
	time.Sleep(500 * time.Millisecond) // the four are in flight
	start := time.Now()
	c, b := newAPIClient(h, email, pw).do("POST", "/api/login", map[string]string{"email": email, "password": pw}, fromAddr(attackedAddr))
	took := time.Since(start)
	h.assert(t, "S-H3: four wrong passwords in flight from one network do not keep the owner out: it signs in from another within 8 s",
		c == 200 && took < 8*time.Second, "HTTP %d after %v: %s", c, took, head(b, 200))
	wg.Wait()
	h.check(t, "control: the attacker's four wrong passwords are refused", fmt.Sprint(codes), "[401 401 401 401]")
	// Its network's failures are its own: the owner's sign-in cleared the
	// account's, not those. Four on record, so the second attempt waits.
	newAPIClient(h, "", "").do("POST", "/api/login", wrong, fromAddr(attackerAddr))
	start = time.Now()
	c, _ = newAPIClient(h, "", "").do("POST", "/api/login", wrong, fromAddr(attackerAddr))
	took = time.Since(start)
	h.assert(t, "S-H3: after the owner's sign-in the attacker's network still waits its own delay (1 s from its fifth failure)",
		c == 401 && took >= time.Second, "HTTP %d after %v", c, took)
}
