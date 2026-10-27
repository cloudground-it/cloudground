package state

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func newPasskeyUser(t *testing.T, s *Store, email string) User {
	t.Helper()
	u, err := s.CreateUser(email, "argon2id$x$y", "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func addKey(t *testing.T, s *Store, userID int64, cred string) Passkey {
	t.Helper()
	p, err := s.AddPasskey(Passkey{UserID: userID, CredentialID: []byte(cred), PublicKey: []byte("pk"), RPID: "panel.example.com",
		Name: "key " + cred, Transports: []string{"usb", "nfc"}, BackupEligible: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWebAuthnIDIsMadeOnceAndNamesTheAccount(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	first, err := s.WebAuthnID(u.ID)
	if err != nil || len(first) != webAuthnIDLen {
		t.Fatalf("WebAuthnID = %x, %v", first, err)
	}
	again, err := s.WebAuthnID(u.ID)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("second WebAuthnID = %x, %v; want %x", again, err, first)
	}
	got, err := s.UserByWebAuthnID(first)
	if err != nil || got.ID != u.ID {
		t.Fatalf("UserByWebAuthnID = %+v, %v", got, err)
	}
	if _, err := s.UserByWebAuthnID([]byte("nobody")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown handle: %v", err)
	}
	if _, err := s.WebAuthnID(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
}

func TestPasskeysAreCountedWithTheAccount(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	if !u.PasswordSignIn || u.Passkeys != 0 || u.HasSecondFactor() {
		t.Fatalf("a new account: %+v", u)
	}
	p := addKey(t, s, u.ID, "c1")
	if _, err := s.AddPasskey(Passkey{UserID: u.ID, CredentialID: []byte("c1"), PublicKey: []byte("pk"), RPID: "x", Name: "dup"}); !errors.Is(err, ErrPasskeyTaken) {
		t.Fatalf("a credential id twice: %v", err)
	}
	got, _ := s.GetUser(u.ID)
	if got.Passkeys != 1 || !got.HasSecondFactor() {
		t.Fatalf("after one passkey: %+v", got)
	}
	byID, err := s.PasskeyByCredentialID([]byte("c1"))
	if err != nil || byID.ID != p.ID || byID.RPID != "panel.example.com" || len(byID.Transports) != 2 || !byID.BackupEligible {
		t.Fatalf("PasskeyByCredentialID = %+v, %v", byID, err)
	}
	if err := s.RecordPasskeyUse(p.ID, 7, true); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Passkeys(u.ID)
	if len(list) != 1 || list[0].SignCount != 7 || !list[0].BackupState || list[0].LastUsedAt.IsZero() {
		t.Fatalf("after a use: %+v", list)
	}
}

func TestTooManyPasskeys(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	for i := 0; i < MaxPasskeysPerUser; i++ {
		addKey(t, s, u.ID, string(rune('A'+i)))
	}
	if _, err := s.AddPasskey(Passkey{UserID: u.ID, CredentialID: []byte("one more"), PublicKey: []byte("pk"), RPID: "x", Name: "n"}); !errors.Is(err, ErrTooManyPasskeys) {
		t.Fatalf("the 21st passkey: %v", err)
	}
}

func TestPasswordSignInNeedsAPasskeyAndTheLastOneStays(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	if err := s.SetPasswordSignIn(u.ID, false); !errors.Is(err, ErrNoPasskey) {
		t.Fatalf("off without a passkey: %v", err)
	}
	p := addKey(t, s, u.ID, "c1")
	if err := s.SetPasswordSignIn(u.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(u.ID); got.PasswordSignIn {
		t.Fatal("password sign-in still on")
	}
	if _, _, err := s.RemovePasskey(u.ID, p.ID); !errors.Is(err, ErrLastPasskey) {
		t.Fatalf("removing the last passkey with the password off: %v", err)
	}
	// A password reset turns it back on.
	if err := s.SetUserPassword(u.ID, "argon2id$a$b"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(u.ID); !got.PasswordSignIn {
		t.Fatal("a password reset left password sign-in off")
	}
}

func TestRemovingTheLastFactorTakesCodesAndTokens(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	other := newPasskeyUser(t, s, "b@example.com")
	p1, p2 := addKey(t, s, u.ID, "c1"), addKey(t, s, u.ID, "c2")
	foreign := addKey(t, s, other.ID, "c3")
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(u.ID, "t", "cgt_secret", "cgt_se", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RemovePasskey(u.ID, foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account's passkey: %v", err)
	}
	if _, last, err := s.RemovePasskey(u.ID, p1.ID); err != nil || last {
		t.Fatalf("first of two: last=%v err=%v", last, err)
	}
	if got, _ := s.GetUser(u.ID); got.RecoveryCodesLeft != 2 {
		t.Fatalf("codes after the first: %d", got.RecoveryCodesLeft)
	}
	if _, last, err := s.RemovePasskey(u.ID, p2.ID); err != nil || !last {
		t.Fatalf("the last: last=%v err=%v", last, err)
	}
	got, _ := s.GetUser(u.ID)
	if got.RecoveryCodesLeft != 0 || got.HasSecondFactor() {
		t.Fatalf("after the last factor: %+v", got)
	}
	if toks, _ := s.ListAPITokens(); len(toks) != 0 {
		t.Fatalf("tokens survived the last factor: %+v", toks)
	}
}

func TestRecoveryCodesAreSpentOnce(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	other := newPasskeyUser(t, s, "b@example.com")
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SpendRecoveryCode(other.ID, "h1"); err != nil || ok {
		t.Fatalf("another account's code: %v %v", ok, err)
	}
	if ok, err := s.SpendRecoveryCode(u.ID, "h1"); err != nil || !ok {
		t.Fatalf("first use: %v %v", ok, err)
	}
	if ok, _ := s.SpendRecoveryCode(u.ID, "h1"); ok {
		t.Fatal("a code spent twice")
	}
	if got, _ := s.GetUser(u.ID); got.RecoveryCodesLeft != 1 {
		t.Fatalf("left = %d", got.RecoveryCodesLeft)
	}
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h3"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SpendRecoveryCode(u.ID, "h2"); ok {
		t.Fatal("an old code still works after new ones were made")
	}
}

func TestDisablingTOTPKeepsCodesOnlyWithAPasskey(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	addKey(t, s, u.ID, "c1")
	if err := s.DisableUserTOTP(u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(u.ID); got.RecoveryCodesLeft != 1 {
		t.Fatal("codes went though a passkey is left")
	}
	v := newPasskeyUser(t, s, "b@example.com")
	if err := s.ReplaceRecoveryCodes(v.ID, []string{"h2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableUserTOTP(v.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(v.ID); got.RecoveryCodesLeft != 0 {
		t.Fatal("codes outlived the last factor")
	}
}

func TestResetSecondFactorsClearsEverything(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	addKey(t, s, u.ID, "c1")
	if err := s.SetPasswordSignIn(u.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingTOTP(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivatePendingTOTP(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("tok", u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetSecondFactors(u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUser(u.ID)
	if got.HasSecondFactor() || got.RecoveryCodesLeft != 0 || !got.PasswordSignIn {
		t.Fatalf("after the reset: %+v", got)
	}
	if _, err := s.UserForSession("tok"); err == nil {
		t.Fatal("a session survived the reset")
	}
	if sec, _ := s.Secret(UserSecret(u.ID, totpActive)); sec != "" {
		t.Fatal("the TOTP secret survived the reset")
	}
	if err := s.ResetSecondFactors(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
}

func TestDeletingAnAccountTakesItsPasskeysAndCodes(t *testing.T) {
	s, _ := newStore(t)
	newPasskeyUser(t, s, "keep@example.com")
	u := newPasskeyUser(t, s, "a@example.com")
	addKey(t, s, u.ID, "c1")
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM user_passkeys) + (SELECT COUNT(*) FROM user_recovery_codes)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left: %d %v", n, err)
	}
}
