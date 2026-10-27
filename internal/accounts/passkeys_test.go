package accounts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/accounts/softkey"
	"github.com/cloudground-it/cloudground/internal/state"
)

var testRP = RP{ID: "panel.example.com", Origin: "https://panel.example.com"}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// addPasskey registers a software passkey for u with the password as the
// proof, and returns it with the recovery codes the answer carried.
func (f *fixture) addPasskey(t *testing.T, u state.User, p Proof) (*softkey.Key, []string) {
	t.Helper()
	opts, err := f.sv.BeginPasskey(bg, here, u, testRP, u.Email, p)
	if err != nil {
		t.Fatalf("BeginPasskey: %v", err)
	}
	key := softkey.New()
	cred, err := key.Create(mustJSON(t, opts), testRP.Origin)
	if err != nil {
		t.Fatal(err)
	}
	_, codes, err := f.sv.FinishPasskey(u, testRP, u.Email, "Laptop", cred, "")
	if err != nil {
		t.Fatalf("FinishPasskey: %v", err)
	}
	return key, codes
}

func (f *fixture) fresh(t *testing.T, u state.User) state.User {
	t.Helper()
	got, err := f.st.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// secondFactorOptions signs in with the password alone and returns the
// passkey options the refusal carried.
func (f *fixture) secondFactorOptions(t *testing.T, u state.User) []byte {
	t.Helper()
	_, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Address: here.Address, RP: testRP})
	ref, ok := IsRefusal(err)
	if !ok || !ref.SecondFactor || ref.PasskeyOptions == nil {
		t.Fatalf("sign-in without a factor: %v (%+v)", err, ref)
	}
	return mustJSON(t, ref.PasskeyOptions)
}

func TestPasskeyRegistrationAndSecondFactor(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "admin")
	key, codes := f.addPasskey(t, u, Proof{Password: pw})
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("first factor: %d recovery codes", len(codes))
	}
	if !f.audited("passkey.added") {
		t.Fatal("passkey.added not audited")
	}
	u = f.fresh(t, u)
	if u.Passkeys != 1 || !u.HasSecondFactor() || u.RecoveryCodesLeft != RecoveryCodeCount {
		t.Fatalf("after the first passkey: %+v", u)
	}

	// The password alone is now refused, naming the factors.
	_, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Address: here.Address, RP: testRP})
	ref, _ := IsRefusal(err)
	if ref == nil || ref.Msg != msgCodeRequired || strings.Join(ref.Factors, ",") != "passkey,recovery_code" || ref.PasskeyOptions == nil {
		t.Fatalf("password alone: %v %+v", err, ref)
	}
	// At another address (an IP: no relying party) no passkey is offered.
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Address: here.Address})
	if ref, _ := IsRefusal(err); ref == nil || ref.PasskeyOptions != nil || strings.Join(ref.Factors, ",") != "recovery_code" {
		t.Fatalf("without a relying party: %+v", ref)
	}

	opts := f.secondFactorOptions(t, u)
	assertion, err := key.Get(opts, testRP.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Passkey: assertion, Address: here.Address, RP: testRP}); err != nil {
		t.Fatalf("sign-in with the passkey: %v", err)
	}
	// The challenge is spent.
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Passkey: assertion, Address: here.Address, RP: testRP})
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)

	// Another key's signature is refused.
	opts = f.secondFactorOptions(t, u)
	stranger := softkey.New()
	stranger.CredentialID, stranger.UserHandle = key.CredentialID, key.UserHandle
	forged, _ := stranger.Get(opts, testRP.Origin)
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Passkey: forged, Address: here.Address, RP: testRP})
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)

	// An assertion made for another origin is refused.
	opts = f.secondFactorOptions(t, u)
	phished, _ := key.Get(opts, "https://panel.example.com.evil.test")
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Passkey: phished, Address: here.Address, RP: testRP})
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)

	// A counter that goes backwards is a cloned key.
	opts = f.secondFactorOptions(t, u)
	key.Counter = 0
	cloned, _ := key.Get(opts, testRP.Origin) // counter 1, below the stored one
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Passkey: cloned, Address: here.Address, RP: testRP})
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)
	if !f.audited("login.passkey_cloned") {
		t.Fatal("a cloned key is not audited")
	}

	// Two factors at once are refused before anything is checked.
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: "123456", RecoveryCode: codes[0], Address: here.Address})
	wantRefusal(t, err, Invalid, msgOneFactor)
}

func TestPasskeySignInAlone(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "operator")
	key, _ := f.addPasskey(t, u, Proof{Password: pw})

	signIn := func() (state.User, error) {
		opts, err := f.sv.BeginPasskeySignIn(here, testRP)
		if err != nil {
			t.Fatal(err)
		}
		a, err := key.Get(mustJSON(t, opts), testRP.Origin)
		if err != nil {
			t.Fatal(err)
		}
		return f.sv.PasskeySignIn(here, testRP, a)
	}
	got, err := signIn()
	if err != nil || got.ID != u.ID {
		t.Fatalf("passkey alone: %+v %v", got, err)
	}
	if !f.audited("login.passkey") {
		t.Fatal("login.passkey not audited")
	}

	// Without user verification the passkey is not the whole sign-in.
	key.Verify = false
	_, err = signIn()
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)
	key.Verify = true

	// A replayed answer is refused: the challenge was spent.
	opts, _ := f.sv.BeginPasskeySignIn(here, testRP)
	a, _ := key.Get(mustJSON(t, opts), testRP.Origin)
	if _, err := f.sv.PasskeySignIn(here, testRP, a); err != nil {
		t.Fatal(err)
	}
	_, err = f.sv.PasskeySignIn(here, testRP, a)
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)

	// Another account's user handle is refused.
	other := f.user(t, "b@example.com", "readonly")
	otherKey, _ := f.addPasskey(t, other, Proof{Password: pw})
	opts, _ = f.sv.BeginPasskeySignIn(here, testRP)
	key.UserHandle, otherKey.UserHandle = otherKey.UserHandle, key.UserHandle
	a, _ = key.Get(mustJSON(t, opts), testRP.Origin)
	_, err = f.sv.PasskeySignIn(here, testRP, a)
	wantRefusal(t, err, Unauthorized, msgPasskeyFailed)
	key.UserHandle = otherKey.UserHandle

	// A disabled account is refused even with its passkey.
	if err := f.st.UpdateUserAccess(u.ID, "operator", true); err != nil {
		t.Fatal(err)
	}
	_, err = signIn()
	wantRefusal(t, err, Forbidden, msgDisabled)

	// No relying party, no passkeys.
	if _, err := f.sv.BeginPasskeySignIn(here, RP{}); err == nil {
		t.Fatal("a challenge without a relying party")
	}
}

func TestRecoveryCodes(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "admin")
	secret := f.enrol(t, u)
	codes, err := f.sv.RegenerateRecoveryCodes(bg, here, f.fresh(t, u), RP{}, u.Email, Proof{Password: pw, Code: f.code(secret, 1)})
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatalf("regenerate: %d %v", len(codes), err)
	}
	for _, c := range codes {
		if len(c) != 19 || c[4] != '-' || c[9] != '-' || c[14] != '-' {
			t.Fatalf("code format %q", c)
		}
	}
	if !f.audited("recovery_codes.generated") {
		t.Fatal("regeneration not audited")
	}
	// Typed loosely: upper case, no dashes, O for 0.
	typed := strings.ToUpper(strings.ReplaceAll(codes[0], "-", ""))
	typed = strings.ReplaceAll(typed, "0", "O")
	if _, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, RecoveryCode: typed, Address: here.Address}); err != nil {
		t.Fatalf("sign-in with a recovery code: %v", err)
	}
	if !f.audited("login.recovery_code") {
		t.Fatal("the spent code is not audited")
	}
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, RecoveryCode: codes[0], Address: here.Address})
	wantRefusal(t, err, Unauthorized, msgRecoveryBad)
	if !f.audited("login.recovery_failed") {
		t.Fatal("a refused code is not audited")
	}
	if got := f.fresh(t, u).RecoveryCodesLeft; got != RecoveryCodeCount-1 {
		t.Fatalf("left %d", got)
	}
	// A recovery code is a proof: the lost phone is replaced with one.
	if _, err := f.sv.BeginTOTP(bg, here, f.fresh(t, u), RP{}, u.Email, Proof{Password: pw, RecoveryCode: codes[1]}); err != nil {
		t.Fatalf("replace TOTP with a recovery code: %v", err)
	}
	if !f.audited("recovery_code.used") {
		t.Fatal("a code spent in a proof is not audited")
	}
	// Without a factor there are no codes to make.
	plain := f.user(t, "b@example.com", "readonly")
	_, err = f.sv.RegenerateRecoveryCodes(bg, here, plain, RP{}, plain.Email, Proof{Password: pw})
	wantRefusal(t, err, Invalid, "turn on two-factor sign-in first")
}

func TestNormalizeRecoveryCode(t *testing.T) {
	for typed, ok := range map[string]bool{
		"abcd-efgh-jkmn-pqrs": true, "ABCD EFGH JKMN PQRS": true, "abcdefghjkmnpqrs": true,
		"abcd-efgh-jkmn-pqr": false, "abcd-efgh-jkmn-pqrsu": false, "abcd-efgh-jkmn-pqr!": false, "": false,
	} {
		if _, got := NormalizeRecoveryCode(typed); got != ok {
			t.Errorf("%q: %v, want %v", typed, got, ok)
		}
	}
	a, _ := NormalizeRecoveryCode("ilo0-1111-0000-aaaa")
	if a != "1100111100000aaaa"[:0]+"110011110000aaaa" {
		t.Errorf("i, l and o read as %q", a)
	}
}

func TestProofsForFactors(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "admin")
	key, codes := f.addPasskey(t, u, Proof{Password: pw})
	u = f.fresh(t, u)

	// A second passkey takes the password and a current factor.
	_, err := f.sv.BeginPasskey(bg, here, u, testRP, u.Email, Proof{Password: pw})
	wantRefusal(t, err, Forbidden, msgConfirmFactor)
	_, err = f.sv.BeginPasskey(bg, here, u, testRP, u.Email, Proof{Password: "wrong", RecoveryCode: codes[0]})
	wantRefusal(t, err, Forbidden, msgWrongPassword)
	proofOpts, err := f.sv.ProofChallenge(u, testRP)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := key.Get(mustJSON(t, proofOpts), testRP.Origin)
	if _, err := f.sv.BeginPasskey(bg, here, u, testRP, u.Email, Proof{Password: pw, Passkey: a}); err != nil {
		t.Fatalf("a proof with the passkey: %v", err)
	}
	// A sign-in challenge is not a proof's.
	opts := f.secondFactorOptions(t, u)
	a, _ = key.Get(opts, testRP.Origin)
	_, err = f.sv.BeginPasskey(bg, here, u, testRP, u.Email, Proof{Password: pw, Passkey: a})
	wantRefusal(t, err, Forbidden, msgConfirmFactor)

	// Password sign-in off takes a passkey with user verification.
	_, err = f.sv.SetPasswordSignIn(bg, here, u, testRP, u.Email, false, Proof{Password: pw, RecoveryCode: codes[1]}, "")
	wantRefusal(t, err, Forbidden, msgConfirmPasskey)
	key.Verify = false
	proofOpts, _ = f.sv.ProofChallenge(u, testRP)
	a, _ = key.Get(mustJSON(t, proofOpts), testRP.Origin)
	_, err = f.sv.SetPasswordSignIn(bg, here, u, testRP, u.Email, false, Proof{Password: pw, Passkey: a}, "")
	wantRefusal(t, err, Forbidden, msgConfirmPasskey)
	key.Verify = true
	proofOpts, _ = f.sv.ProofChallenge(u, testRP)
	a, _ = key.Get(mustJSON(t, proofOpts), testRP.Origin)
	if _, err := f.sv.SetPasswordSignIn(bg, here, u, testRP, u.Email, false, Proof{Password: pw, Passkey: a}, ""); err != nil {
		t.Fatalf("password sign-in off: %v", err)
	}
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Address: here.Address, RP: testRP})
	wantRefusal(t, err, Forbidden, msgPasswordOff)
	if !f.audited("login.password_off") {
		t.Fatal("login.password_off not audited")
	}
	// Its last passkey stays while the password is off.
	u = f.fresh(t, u)
	list, _ := f.sv.ListPasskeys(u)
	err = f.sv.RemovePasskey(bg, here, u, testRP, u.Email, list[0].ID, Proof{}, "")
	wantRefusal(t, err, Invalid, state.ErrLastPasskey.Error())
	// With the password off a proof is a verified passkey, password or not.
	proofOpts, _ = f.sv.ProofChallenge(u, testRP)
	a, _ = key.Get(mustJSON(t, proofOpts), testRP.Origin)
	if _, err := f.sv.SetPasswordSignIn(bg, here, u, testRP, u.Email, true, Proof{Passkey: a}, ""); err != nil {
		t.Fatalf("password sign-in on: %v", err)
	}
	u = f.fresh(t, u)
	if !u.PasswordSignIn {
		t.Fatal("password sign-in still off")
	}
	// The last factor removed: the codes go too.
	err = f.sv.RemovePasskey(bg, here, u, testRP, u.Email, list[0].ID, Proof{Password: pw, RecoveryCode: codes[2]}, "")
	if err != nil {
		t.Fatalf("remove the last passkey: %v", err)
	}
	if got := f.fresh(t, u); got.HasSecondFactor() || got.RecoveryCodesLeft != 0 {
		t.Fatalf("after the last factor went: %+v", got)
	}
}

func TestAdminResetsTwoFactorAndRecoveryTakesEveryFactor(t *testing.T) {
	f := newFixture(t)
	admin := f.user(t, "admin@example.com", "admin")
	u := f.user(t, "a@example.com", "operator")
	f.addPasskey(t, u, Proof{Password: pw})
	wantRefusal(t, f.sv.ResetTwoFactor(admin, admin.Email, admin.ID), Invalid, "manage your own two-factor from Security")
	wantRefusal(t, f.sv.ResetTwoFactor(admin, admin.Email, 9999), NotFound, msgNoUser)
	if err := f.sv.ResetTwoFactor(admin, admin.Email, u.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.fresh(t, u); got.HasSecondFactor() || got.RecoveryCodesLeft != 0 {
		t.Fatalf("after the reset: %+v", got)
	}
	if !f.audited("user.2fa_reset") {
		t.Fatal("user.2fa_reset not audited")
	}
	if _, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Address: here.Address}); err != nil {
		t.Fatalf("the password alone after the reset: %v", err)
	}

	// Host recovery with --disable-2fa takes passkeys and codes too, and
	// turns password sign-in back on.
	key, _ := f.addPasskey(t, admin, Proof{Password: pw})
	opts, _ := f.sv.ProofChallenge(f.fresh(t, admin), testRP)
	a, _ := key.Get(mustJSON(t, opts), testRP.Origin)
	if _, err := f.sv.SetPasswordSignIn(bg, here, f.fresh(t, admin), testRP, admin.Email, false, Proof{Password: pw, Passkey: a}, ""); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Recover(f.st, &out, Recovery{Email: admin.Email, DisableTOTP: true, Root: true}); err != nil {
		t.Fatal(err)
	}
	got := f.fresh(t, admin)
	if got.HasSecondFactor() || got.RecoveryCodesLeft != 0 || !got.PasswordSignIn {
		t.Fatalf("after host recovery: %+v", got)
	}
	for _, want := range []string{"password sign-in was off", "1 passkey", "10 recovery codes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("recovery output lacks %q:\n%s", want, out.String())
		}
	}
}
