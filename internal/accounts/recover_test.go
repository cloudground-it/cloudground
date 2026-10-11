package accounts

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// recoveryStore is a store opened as recover-admin opens it: no secret key.
// keyed is the same database as the panel opens it.
func recoveryStore(t *testing.T) (rec *state.Store, keyed func() *state.Store, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "state.db")
	panel, err := state.Open(path) // creates the key, as the installed panel has
	if err != nil {
		t.Fatal(err)
	}
	panel.Close()
	rec, err = state.OpenWith(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	return rec, func() *state.Store {
		s, err := state.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}, path
}

func addAccount(t *testing.T, st *state.Store, email, role string) state.User {
	t.Helper()
	h, _ := HashPassword("the-old-password")
	u, err := st.CreateUser(email, h, role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var printedPassword = regexp.MustCompile(`password: (\S+)`)

func recoverOK(t *testing.T, st *state.Store, req Recovery) string {
	t.Helper()
	req.Root = true
	var out bytes.Buffer
	if err := Recover(st, &out, req); err != nil {
		t.Fatalf("recover %+v: %v", req, err)
	}
	return out.String()
}

func TestRecoverNeedsRoot(t *testing.T) {
	rec, _, _ := recoveryStore(t)
	err := Recover(rec, &bytes.Buffer{}, Recovery{Email: "a@example.com"})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("not root = %v", err)
	}
	if n, _ := rec.CountUsers(); n != 0 {
		t.Fatal("it acted anyway")
	}
}

// A panel with no account: the installer's token is long gone, so recovery
// makes the first admin, and only with an address.
func TestRecoverCreatesTheFirstAdmin(t *testing.T) {
	rec, _, _ := recoveryStore(t)
	if err := Recover(rec, &bytes.Buffer{}, Recovery{Root: true}); err == nil || !strings.Contains(err.Error(), "--email") {
		t.Fatalf("no address on an empty panel = %v", err)
	}
	out := recoverOK(t, rec, Recovery{Email: " First@Example.COM "})
	u, err := rec.GetUserByEmail("first@example.com")
	if err != nil || u.Role != "admin" || !strings.HasPrefix(u.PasswordHash, "argon2id$") {
		t.Fatalf("created %+v %v", u, err)
	}
	m := printedPassword.FindStringSubmatch(out)
	if m == nil || !VerifyPassword(m[1], u.PasswordHash) {
		t.Fatalf("the printed password does not open the account:\n%s", out)
	}
	if !strings.Contains(out, "Password created.") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := rec.GetUserByEmail("First@Example.COM"); err == nil {
		t.Fatal("stored under the raw address")
	}
	recoverOK(t, rec, Recovery{Email: "FIRST@example.com"}) // finds it in any case
}

// More than one account and no address: it lists them, it never guesses.
func TestRecoverListsInsteadOfGuessing(t *testing.T) {
	rec, _, _ := recoveryStore(t)
	addAccount(t, rec, "admin@example.com", "admin")
	addAccount(t, rec, "operator@example.com", "operator")
	err := Recover(rec, &bytes.Buffer{}, Recovery{Root: true})
	for _, want := range []string{"admin@example.com", "operator@example.com", "operator", "--email"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("listing without %q: %v", want, err)
		}
	}
	for _, bad := range []string{"not an address", "Owner <admin@example.com>"} {
		if err := Recover(rec, &bytes.Buffer{}, Recovery{Email: bad, Root: true}); err == nil || !strings.Contains(err.Error(), "bare e-mail") {
			t.Fatalf("%q = %v", bad, err)
		}
	}
	if err := Recover(rec, &bytes.Buffer{}, Recovery{Email: "typo@example.com", Root: true}); err == nil || !strings.Contains(err.Error(), "typo@example.com") {
		t.Fatalf("unknown address = %v", err)
	}
}

// The reset ends what the old password opened, and is audited.
func TestRecoverResetsAndRevokes(t *testing.T) {
	rec, _, _ := recoveryStore(t)
	u := addAccount(t, rec, "owner@example.com", "admin")
	rec.CreateSession("live-session", u.ID, time.Hour)
	tok, err := rec.CreateAPIToken(u.ID, "script", "cgt_secret-of-the-test", "cgt_secret", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	out := recoverOK(t, rec, Recovery{Email: "owner@example.com"})
	after, _ := rec.GetUserByEmail("owner@example.com")
	if VerifyPassword("the-old-password", after.PasswordHash) {
		t.Fatal("the old password still works")
	}
	if m := printedPassword.FindStringSubmatch(out); m == nil || !VerifyPassword(m[1], after.PasswordHash) {
		t.Fatalf("printed password:\n%s", out)
	}
	if _, err := rec.UserForSession("live-session"); err == nil {
		t.Fatal("the session survived")
	}
	if _, _, err := rec.UserForAPIToken("cgt_secret-of-the-test"); err == nil {
		t.Fatalf("API token %d survived", tok.ID)
	}
	events, _ := rec.ListAuditEvents(50)
	found := false
	for _, e := range events {
		found = found || (e.Actor == "root (console)" && e.Action == "user.password.reset" && e.Subject == "owner@example.com")
	}
	if !found {
		t.Fatal("not audited")
	}
}

// The phone is gone: --disable-2fa, and only when asked. It works on the
// keyless store recovery uses, and the secret does not come back when the
// panel opens the database again.
func TestRecoverDisablesTOTPOnlyWhenAsked(t *testing.T) {
	rec, keyed, _ := recoveryStore(t)
	u := addAccount(t, rec, "twofactor@example.com", "admin")
	panel := keyed()
	if err := panel.SetPendingTOTP(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if err := panel.ActivatePendingTOTP(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	panel.Close()

	out := recoverOK(t, rec, Recovery{Email: "twofactor@example.com"})
	if !strings.Contains(out, "two-factor is still ON") {
		t.Fatalf("no warning:\n%s", out)
	}
	if after, _ := rec.GetUserByEmail("twofactor@example.com"); !after.TOTPEnabled {
		t.Fatal("2FA switched off without being asked")
	}
	out = recoverOK(t, rec, Recovery{Email: "twofactor@example.com", DisableTOTP: true})
	if !strings.Contains(out, "two-factor authentication was switched off") {
		t.Fatalf("output:\n%s", out)
	}
	out = recoverOK(t, rec, Recovery{Email: "twofactor@example.com", DisableTOTP: true})
	if !strings.Contains(out, "two-factor was already off") {
		t.Fatalf("output:\n%s", out)
	}
	panel = keyed()
	if secret, on, err := panel.UserTOTP(u.ID); on || secret != "" || err != nil {
		t.Fatalf("after recovery: %q %v %v", secret, on, err)
	}
}

// A disabled admin does not count as one: recovering the only admin when it
// is disabled enables it; with another admin enabled it stays as an admin
// left it.
func TestRecoverKeepsAnEnabledAdmin(t *testing.T) {
	rec, _, path := recoveryStore(t)
	owner := addAccount(t, rec, "owner@example.com", "admin")
	other := addAccount(t, rec, "second@example.com", "admin")
	if err := rec.UpdateUserAccess(owner.ID, "admin", true); err != nil {
		t.Fatal(err)
	}
	out := recoverOK(t, rec, Recovery{Email: "owner@example.com"})
	if got, _ := rec.GetUserByEmail("owner@example.com"); !got.Disabled {
		t.Fatal("re-enabled an account another admin had disabled")
	}
	if !strings.Contains(out, "this account is disabled") {
		t.Fatalf("output:\n%s", out)
	}
	if err := rec.UpdateUserAccess(other.ID, "admin", true); !errors.Is(err, state.ErrLastAdmin) {
		t.Fatalf("disabling the last enabled admin = %v", err)
	}
	// The state an older build or a hand edit could leave: no enabled admin.
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, other.ID); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	out = recoverOK(t, rec, Recovery{Email: "owner@example.com"})
	if got, _ := rec.GetUserByEmail("owner@example.com"); got.Disabled {
		t.Fatal("the only admin stayed disabled: the new password opens nothing")
	}
	if !strings.Contains(out, "it has been re-enabled") || strings.Contains(out, "no enabled admin account") {
		t.Fatalf("output:\n%s", out)
	}
}

// Every admin gone: resetting an operator gets someone in, and says that
// it does not get them out of trouble.
func TestRecoverWarnsWithoutAnAdmin(t *testing.T) {
	rec, _, _ := recoveryStore(t)
	addAccount(t, rec, "operator@example.com", "operator")
	out := recoverOK(t, rec, Recovery{Email: "operator@example.com"})
	for _, want := range []string{`this account is "operator", not an admin`, "there is no enabled admin account on this panel"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
