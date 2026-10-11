package accounts

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

type auditLine struct{ actor, action, subject, detail string }

type fixture struct {
	sv    *Service
	st    *state.Store
	at    time.Time
	mu    sync.Mutex
	log   []auditLine
	slept []time.Duration
}

var bg = context.Background()

// here is the origin of the account routes' calls in these tests.
var here = Origin{Address: "198.51.100.7"}

const pw = "a-long-password!"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{st: st, at: time.Unix(1_800_000_015, 0)}
	f.sv = New(st, func(a, b, c, d string) {
		f.mu.Lock()
		f.log = append(f.log, auditLine{a, b, c, d})
		f.mu.Unlock()
	})
	f.sv.Now = func() time.Time { return f.at }
	f.sv.Sleep = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.slept = append(f.slept, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	// The limits read the same frozen clock: thirty argon2 checks under the
	// race detector can take longer than the window.
	for _, w := range []*throttle.Window{f.sv.Limits.Address, f.sv.Limits.Wide, f.sv.Limits.AddressAccount, f.sv.Limits.Codes} {
		w.WithClock(f.sv.Now)
	}
	f.sv.Limits.Delays.WithClock(f.sv.Now)
	return f
}

func (f *fixture) user(t *testing.T, email, role string) state.User {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.st.CreateUser(email, h, role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// enrol turns 2FA on for u and returns its secret.
func (f *fixture) enrol(t *testing.T, u state.User) string {
	t.Helper()
	en, err := f.sv.BeginTOTP(bg, here, u, pw, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sv.ConfirmTOTP(u, u.Email, f.code(en.Secret, 0), ""); err != nil {
		t.Fatal(err)
	}
	return en.Secret
}

func (f *fixture) code(secret string, offset int64) string {
	c, _ := TOTPCode(secret, uint64(int64(TOTPStep(f.at))+offset))
	return c
}

func (f *fixture) audited(action string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.log {
		if l.action == action {
			return true
		}
	}
	return false
}

func wantRefusal(t *testing.T, err error, kind Kind, msg string) {
	t.Helper()
	ref, ok := IsRefusal(err)
	if !ok || ref.Kind != kind || (msg != "" && ref.Msg != msg) {
		t.Fatalf("got %v, want refusal %d %q", err, kind, msg)
	}
}

func TestSetup(t *testing.T) {
	f := newFixture(t)
	f.st.CreateSetupToken("boot", time.Hour)
	_, err := f.sv.Setup(bg, "Admin <a@example.com>", pw, "boot")
	wantRefusal(t, err, Invalid, errBadEmail.Error())
	_, err = f.sv.Setup(bg, "a@example.com", "short", "boot")
	wantRefusal(t, err, Invalid, errPasswordBounds.Error())
	_, err = f.sv.Setup(bg, "a@example.com", pw, "wrong")
	wantRefusal(t, err, Forbidden, "invalid or expired setup token")
	// The refusals above did not spend the token.
	u, err := f.sv.Setup(bg, " A@Example.com ", pw, "boot")
	if err != nil || u.Email != "a@example.com" || u.Role != "admin" {
		t.Fatalf("setup = %+v %v", u, err)
	}
	if !f.audited("setup.complete") {
		t.Fatal("not audited")
	}
	f.st.CreateSetupToken("boot2", time.Hour)
	_, err = f.sv.Setup(bg, "b@example.com", pw, "boot2")
	wantRefusal(t, err, Conflict, "setup already complete")
}

func TestLoginSteps(t *testing.T) {
	f := newFixture(t)
	f.user(t, "a@example.com", "admin")
	in := func(email, password string) SignIn {
		return SignIn{Email: email, Password: password, Address: "198.51.100.7"}
	}
	_, err := f.sv.Login(bg, SignIn{Email: strings.Repeat("a", 255), Password: pw})
	wantRefusal(t, err, Invalid, msgMalformed)
	_, err = f.sv.Login(bg, SignIn{Email: "a@example.com", Password: strings.Repeat("p", 1025)})
	wantRefusal(t, err, Invalid, msgMalformed)

	_, err = f.sv.Login(bg, in("nobody@example.com", pw))
	wantRefusal(t, err, Unauthorized, msgBadLogin)
	_, err = f.sv.Login(bg, in("a@example.com", "wrong-password-1"))
	wantRefusal(t, err, Unauthorized, msgBadLogin)
	if !f.audited("login.failed") {
		t.Fatal("failure not audited")
	}
	u, err := f.sv.Login(bg, in(" A@EXAMPLE.com ", pw))
	if err != nil || u.Email != "a@example.com" || !f.audited("login.success") {
		t.Fatalf("login = %+v %v", u, err)
	}

	d := f.user(t, "d@example.com", "readonly")
	f.st.UpdateUserAccess(d.ID, "readonly", true)
	_, err = f.sv.Login(bg, in("d@example.com", "wrong-password-1"))
	wantRefusal(t, err, Unauthorized, msgBadLogin) // the wrong password says nothing more
	_, err = f.sv.Login(bg, in("d@example.com", pw))
	wantRefusal(t, err, Forbidden, "this account is disabled — ask an administrator")
}

func TestLoginSecondFactor(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "admin")
	secret := f.enrol(t, u)
	in := SignIn{Email: "a@example.com", Password: pw, Address: "x"}

	_, err := f.sv.Login(bg, in)
	if ref, _ := IsRefusal(err); ref == nil || ref.Msg != msgCodeRequired || !ref.SecondFactor || ref.Kind != Unauthorized {
		t.Fatalf("no code = %v", err)
	}
	in.Code = "000000"
	if f.code(secret, 0) == "000000" {
		in.Code = "111111"
	}
	_, err = f.sv.Login(bg, in)
	if ref, _ := IsRefusal(err); ref == nil || ref.Msg != msgCodeWrong || !ref.SecondFactor {
		t.Fatalf("wrong code = %v", err)
	}
	in.Code = f.code(secret, 1)
	got, err := f.sv.Login(bg, in)
	if err != nil || !got.TOTPEnabled {
		t.Fatalf("right code = %+v %v", got, err)
	}
	// The same code again, and an older one, are refused.
	for _, c := range []string{f.code(secret, 1), f.code(secret, 0)} {
		in.Code = c
		_, err = f.sv.Login(bg, in)
		if ref, _ := IsRefusal(err); ref == nil || ref.Msg != msgCodeReused || !ref.SecondFactor {
			t.Fatalf("replay = %v", err)
		}
	}
	// A secret that cannot be read never lets anyone in.
	f.st.DeleteSecret(state.UserSecret(u.ID, "totp"))
	f.at = f.at.Add(time.Minute)
	in.Code = f.code(secret, 0)
	_, err = f.sv.Login(bg, in)
	if _, refused := IsRefusal(err); err == nil || refused {
		t.Fatalf("unreadable secret = %v, want a store failure", err)
	}
}

// S-H2: per network, and per network and e-mail.
func TestLoginAddressLimits(t *testing.T) {
	f := newFixture(t)
	for i := range 30 {
		f.sv.Login(bg, SignIn{Email: fmt.Sprintf("nobody%d@example.com", i), Password: "x", Address: "1.1.1.1"})
	}
	_, err := f.sv.Login(bg, SignIn{Email: "fresh@example.com", Password: "x", Address: "1.1.1.1"})
	wantRefusal(t, err, Throttled, msgTooMany)
	// Network, when given, is what is counted: another address of the
	// same /64 is the same network.
	for i := range 30 {
		f.sv.Login(bg, SignIn{Email: fmt.Sprintf("v6-%d@example.com", i), Password: "x", Address: fmt.Sprintf("2001:db8::%x", i+1), Network: "2001:db8::/64"})
	}
	_, err = f.sv.Login(bg, SignIn{Email: "fresh@example.com", Password: "x", Address: "2001:db8::ffff", Network: "2001:db8::/64"})
	wantRefusal(t, err, Throttled, msgTooMany)

	f = newFixture(t)
	for range 8 {
		f.sv.Login(bg, SignIn{Email: "pair@example.com", Password: "x", Address: "2.2.2.2"})
	}
	_, err = f.sv.Login(bg, SignIn{Email: "pair@example.com", Password: "x", Address: "2.2.2.2"})
	wantRefusal(t, err, Throttled, msgTooMany)
	_, err = f.sv.Login(bg, SignIn{Email: "pair@example.com", Password: "x", Address: "3.3.3.3"})
	wantRefusal(t, err, Unauthorized, msgBadLogin)
}

func TestDelaySchedules(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 0, 4: 0, 5: time.Second, 6: 2 * time.Second, 10: 32 * time.Second, 11: time.Minute, 500: time.Minute} {
		if got := NetworkDelay.Delay(n); got != want {
			t.Errorf("NetworkDelay(%d) = %v, want %v", n, got, want)
		}
	}
	for n, want := range map[int]time.Duration{0: 0, 4: 0, 5: time.Second, 6: 2 * time.Second, 10: 2 * time.Second, 500: 2 * time.Second} {
		if got := NameDelay.Delay(n); got != want {
			t.Errorf("NameDelay(%d) = %v, want %v", n, got, want)
		}
	}
}

// S-H3: failures delay every later attempt for the e-mail, the same for an
// unknown e-mail, an account without 2FA and one with it: from the failing
// network up to a minute, from any other network at most NameMaxDelay;
// nobody is locked out, the right credentials still get in after the wait.
func TestLoginFailureDelay(t *testing.T) {
	f := newFixture(t)
	f.user(t, "plain@example.com", "admin")
	tfa := f.user(t, "tfa@example.com", "admin")
	secret := f.enrol(t, tfa)
	for _, email := range []string{"ghost@example.com", "plain@example.com", "tfa@example.com"} {
		for i := range 7 {
			_, err := f.sv.Login(bg, SignIn{Email: email, Password: "wrong-password", Address: "10.0.0.1"})
			wantRefusal(t, err, Unauthorized, msgBadLogin)
			if i == 5 && (len(f.slept) == 0 || f.slept[len(f.slept)-1] != time.Second) {
				t.Fatalf("%s: the sixth failing attempt waited %v, want 1s", email, f.slept)
			}
		}
		// Seven failures from 10.0.0.1: another network waits the
		// e-mail's capped delay, 10.0.0.1 its own, 4s.
		f.slept = nil
		_, err := f.sv.Login(bg, SignIn{Email: email, Password: "wrong-password", Address: "10.9.9.9"})
		wantRefusal(t, err, Unauthorized, msgBadLogin)
		_, err = f.sv.Login(bg, SignIn{Email: email, Password: "wrong-password", Address: "10.0.0.1"})
		wantRefusal(t, err, Unauthorized, msgBadLogin)
		if fmt.Sprint(f.slept) != fmt.Sprint([]time.Duration{NameMaxDelay, 4 * time.Second}) {
			t.Fatalf("%s: the attempts waited %v, want 2s from another network and 4s from the failing one", email, f.slept)
		}
	}
	if _, err := f.sv.Login(bg, SignIn{Email: "plain@example.com", Password: pw, Address: "z"}); err != nil {
		t.Fatalf("the owner was kept out: %v", err)
	}
	if n := f.sv.Limits.Delays.Failures("plain@example.com"); n != 0 {
		t.Fatalf("success left %d failures", n)
	}
	if n := f.sv.Limits.Delays.ScopedFailures("plain@example.com", "10.0.0.1"); n != 8 {
		t.Fatalf("the owner's success from elsewhere left the failing network %d failures, want its 8", n)
	}
	if _, err := f.sv.Login(bg, SignIn{Email: "tfa@example.com", Password: pw, Code: f.code(secret, 1), Address: "z"}); err != nil {
		t.Fatalf("2FA owner kept out: %v", err)
	}
	// A request that goes away while it waits ends there, refused as
	// throttled (#43: never an internal error).
	for range 6 {
		f.sv.Limits.Delays.Fail("plain@example.com", "z")
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_, err := f.sv.Login(ctx, SignIn{Email: "plain@example.com", Password: pw, Address: "z"})
	wantRefusal(t, err, Throttled, msgTooMany)
	// The e-mail of a failed sign-in reaches the audit log printable.
	f.sv.Login(bg, SignIn{Email: "evil\r\nlogin.success@x", Password: "x", Address: "q"})
	f.mu.Lock()
	last := f.log[len(f.log)-1]
	f.mu.Unlock()
	if strings.ContainsAny(last.actor, "\r\n") || last.action != "login.failed" {
		t.Fatalf("audited %+v", last)
	}
}

// Wrong second-factor codes for an account are capped: ten an hour, from
// anywhere, and then no code is checked (429) until the hour passes. The
// tenth is audited as login.2fa_brute. A wrong password says nothing of it.
func TestSecondFactorCap(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "tfa@example.com", "admin")
	secret := f.enrol(t, u)
	wrong := func() string {
		for _, c := range []string{"000000", "111111", "222222"} {
			if c != f.code(secret, -1) && c != f.code(secret, 0) && c != f.code(secret, 1) {
				return c
			}
		}
		t.Fatal("no wrong code")
		return ""
	}
	for i := range CodeCap {
		_, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: wrong(), Address: fmt.Sprintf("10.1.%d.1", i)})
		if ref, _ := IsRefusal(err); ref == nil || ref.Msg != msgCodeWrong {
			t.Fatalf("wrong code %d = %v", i+1, err)
		}
		if f.audited("login.2fa_brute") != (i == CodeCap-1) {
			t.Fatalf("login.2fa_brute after %d wrong codes: %v", i+1, f.audited("login.2fa_brute"))
		}
	}
	_, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: f.code(secret, 0), Address: "y"})
	if ref, _ := IsRefusal(err); ref == nil || ref.Kind != Throttled || ref.Msg != msgCodesCapped || !ref.SecondFactor {
		t.Fatalf("right code at the cap = %v", err)
	}
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: "wrong-password", Address: "y"})
	wantRefusal(t, err, Unauthorized, msgBadLogin)
	// Disabling 2FA is capped the same way.
	err = f.sv.DisableTOTP(bg, here, u, u.Email, pw, f.code(secret, 0), "")
	wantRefusal(t, err, Throttled, msgCodesCapped)
	f.at = f.at.Add(61 * time.Minute)
	if _, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: f.code(secret, 0), Address: "y"}); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
}

// A wrong current code when replacing the authenticator counts like one
// when disabling: a failure, and towards the cap.
func TestBeginCountsAWrongCode(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "tfa@example.com", "admin")
	f.enrol(t, u)
	before := f.sv.Limits.Codes.Count(u.Email)
	_, err := f.sv.BeginTOTP(bg, here, u, pw, "000000")
	wantRefusal(t, err, Forbidden, "enter a code from your current authenticator to replace it")
	if f.sv.Limits.Codes.Count(u.Email) != before+1 || f.sv.Limits.Delays.Failures(u.Email) != 1 {
		t.Fatalf("codes %d, failures %d", f.sv.Limits.Codes.Count(u.Email), f.sv.Limits.Delays.Failures(u.Email))
	}
}

// The code that confirms an enrolment is spent: it cannot also sign in.
// Activation reset the replay watermark to 0, so that code worked once
// more, at the next sign-in within its window.
func TestConfirmingCodeIsSpent(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "spent@example.com", "admin")
	secret := f.enrol(t, u) // confirmed with the code at offset 0
	_, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: f.code(secret, 0), Address: here.Address})
	wantRefusal(t, err, Unauthorized, msgCodeReused)
	// An earlier step's code is no better; the next one signs in.
	_, err = f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: f.code(secret, -1), Address: here.Address})
	wantRefusal(t, err, Unauthorized, msgCodeReused)
	if _, err := f.sv.Login(bg, SignIn{Email: u.Email, Password: pw, Code: f.code(secret, 1), Address: here.Address}); err != nil {
		t.Fatalf("a fresh code: %v", err)
	}
}

// Confirming checks a code against the pending secret, then activates it;
// an enrolment begun in between must not be switched on unchecked.
func TestConfirmActivatesTheCheckedSecret(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "admin")
	first, err := f.sv.BeginTOTP(bg, here, u, pw, "")
	if err != nil {
		t.Fatal(err)
	}
	second := NewTOTPSecret()
	if err := f.st.SetPendingTOTP(u.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := f.st.ActivatePendingTOTP(u.ID, first.Secret); !errors.Is(err, state.ErrPendingChanged) {
		t.Fatalf("activating a replaced secret = %v", err)
	}
	if _, on, _ := f.st.UserTOTP(u.ID); on {
		t.Fatal("2FA was switched on with a secret no code was checked against")
	}
	// The race through the service: the check passes on the first secret,
	// a second enrolment lands before the activation.
	f.st.SetPendingTOTP(u.ID, first.Secret)
	f.sv.Now = func() time.Time {
		f.st.SetPendingTOTP(u.ID, second) // the second begin, mid-confirm
		return f.at
	}
	err = f.sv.ConfirmTOTP(u, u.Email, f.code(first.Secret, 0), "")
	wantRefusal(t, err, Conflict, "")
	if _, on, _ := f.st.UserTOTP(u.ID); on {
		t.Fatal("the racing enrolment was switched on")
	}
}

// The argon2 queue: past 32 waiting, a sign-in answers busy at once; a
// waiter gives up when its request goes away.
func TestPasswordQueueIsBounded(t *testing.T) {
	f := newFixture(t)
	f.user(t, "a@example.com", "admin")
	argonGate <- struct{}{}
	argonGate <- struct{}{}
	defer func() { <-argonGate; <-argonGate }()
	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	for range argonQueue {
		wg.Add(1)
		go func() { defer wg.Done(); verifyPassword(ctx, "x", decoy()) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for argonWaiting.Load() < argonQueue {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting", argonWaiting.Load())
		}
		time.Sleep(time.Millisecond)
	}
	_, err := f.sv.Login(bg, SignIn{Email: "a@example.com", Password: pw, Address: "x"})
	wantRefusal(t, err, Unavailable, ErrBusy.Error())
	cancel()
	wg.Wait()
	if argonWaiting.Load() != 0 {
		t.Fatalf("%d still waiting after their requests ended", argonWaiting.Load())
	}
}

func TestOwnAccountChanges(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "admin")
	f.st.CreateSession("other-device", u.ID, time.Hour)

	err := f.sv.ChangePassword(bg, here, u, u.Email, "wrong-password", "brand-new-password")
	wantRefusal(t, err, Forbidden, "current password is incorrect")
	err = f.sv.ChangePassword(bg, here, u, u.Email, pw, "short")
	wantRefusal(t, err, Invalid, "new password must be 12 to 1024 characters")
	if err := f.sv.ChangePassword(bg, here, u, u.Email, pw, "brand-new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UserForSession("other-device"); err == nil {
		t.Fatal("a password change left a session open")
	}
	u, _ = f.st.GetUserByEmail("a@example.com")
	if !VerifyPassword("brand-new-password", u.PasswordHash) {
		t.Fatal("password not changed")
	}

	// Enrolment needs the password, and keeps 2FA off until confirmed.
	_, err = f.sv.BeginTOTP(bg, here, u, "wrong-password", "")
	wantRefusal(t, err, Forbidden, msgWrongPassword)
	err = f.sv.ConfirmTOTP(u, u.Email, "123456", "")
	wantRefusal(t, err, Invalid, "start enrollment first")
	en, err := f.sv.BeginTOTP(bg, here, u, "brand-new-password", "")
	if err != nil || !strings.HasPrefix(en.URI, "otpauth://totp/CloudGround:a@example.com?") || !strings.HasSuffix(en.URI, "&secret="+en.Secret) {
		t.Fatalf("begin = %+v %v", en, err)
	}
	wrong := "000000"
	if f.code(en.Secret, 0) == wrong {
		wrong = "111111"
	}
	err = f.sv.ConfirmTOTP(u, u.Email, wrong, "")
	wantRefusal(t, err, Invalid, "code did not match — check your device clock")
	f.st.CreateSession("this-device", u.ID, time.Hour)
	f.st.CreateSession("stolen", u.ID, time.Hour)
	if err := f.sv.ConfirmTOTP(u, u.Email, f.code(en.Secret, 0), "this-device"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UserForSession("stolen"); err == nil {
		t.Fatal("turning 2FA on left another session open")
	}
	if _, err := f.st.UserForSession("this-device"); err != nil {
		t.Fatal("turning 2FA on ended the caller's own session")
	}

	// Replacing the authenticator needs a current code, spent once.
	_, err = f.sv.BeginTOTP(bg, here, u, "brand-new-password", "")
	wantRefusal(t, err, Forbidden, "enter a code from your current authenticator to replace it")
	// The confirming code (offset 0) is spent: the next step's code does.
	_, err = f.sv.BeginTOTP(bg, here, u, "brand-new-password", f.code(en.Secret, 0))
	wantRefusal(t, err, Forbidden, "enter a code from your current authenticator to replace it")
	next, err := f.sv.BeginTOTP(bg, here, u, "brand-new-password", f.code(en.Secret, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, on, _ := f.st.UserTOTP(u.ID); !on {
		t.Fatal("starting a new enrolment turned 2FA off")
	}
	if active, _, _ := f.st.UserTOTP(u.ID); active != en.Secret || next.Secret == en.Secret {
		t.Fatal("the active secret changed before the new one was confirmed")
	}

	// Disabling needs password and a fresh code; the spent one does not do.
	err = f.sv.DisableTOTP(bg, here, u, u.Email, "brand-new-password", "", "this-device")
	wantRefusal(t, err, Forbidden, "enter a code from your authenticator to turn it off")
	err = f.sv.DisableTOTP(bg, here, u, u.Email, "brand-new-password", f.code(en.Secret, 1), "this-device")
	wantRefusal(t, err, Forbidden, "enter a code from your authenticator to turn it off")
	f.at = f.at.Add(30 * time.Second) // a step later: offset 1 is fresh again
	err = f.sv.DisableTOTP(bg, here, u, u.Email, "wrong-password", f.code(en.Secret, 1), "this-device")
	wantRefusal(t, err, Forbidden, msgWrongPassword)
	if err := f.sv.DisableTOTP(bg, here, u, u.Email, "brand-new-password", f.code(en.Secret, 1), "this-device"); err != nil {
		t.Fatal(err)
	}
	if _, on, _ := f.st.UserTOTP(u.ID); on {
		t.Fatal("2FA still on")
	}
	if p, _ := f.st.PendingTOTP(u.ID); p != "" {
		t.Fatal("the pending secret survived disabling")
	}
	if !f.audited("2fa.enabled") || !f.audited("2fa.disabled") || !f.audited("password.change") {
		t.Fatal("not audited")
	}

	// The wrong passwords and codes above are failures of the account:
	// past five, each further check waits, and none is refused for it.
	if n := f.sv.Limits.Delays.Failures(u.Email); n < 5 {
		t.Fatalf("%d failures counted", n)
	}
	f.slept = nil
	if err := f.sv.ChangePassword(bg, here, u, u.Email, "brand-new-password", "another-new-password"); err != nil {
		t.Fatal(err)
	}
	if len(f.slept) != 1 || f.slept[0] < time.Second {
		t.Fatalf("own-account check waited %v", f.slept)
	}
}

func TestSessionsAndProfile(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "a@example.com", "readonly")
	f.st.CreateSession("mine", u.ID, time.Hour)
	f.st.SetSessionClient("mine", "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Firefox/120.0", "203.0.113.1")
	f.st.CreateSession("other", u.ID, time.Hour)
	list, err := f.sv.Sessions(u, "mine")
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	var current int
	for _, v := range list {
		if len(v.ID) != 12 {
			t.Fatalf("id %q", v.ID)
		}
		if v.Current {
			current++
			if v.Browser != "Firefox" || v.OS != "Linux" || v.IP != "203.0.113.1" {
				t.Fatalf("%+v", v)
			}
		}
	}
	if current != 1 {
		t.Fatalf("%d sessions marked current", current)
	}
	other := state.TokenHash("other")[:12]
	if err := f.sv.RevokeSession(u, u.Email, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UserForSession("other"); err == nil {
		t.Fatal("not revoked")
	}
	wantRefusal(t, f.sv.RevokeSession(u, u.Email, other), NotFound, "session not found")

	first, locale, bad := "  Grace ", "fr", "a\x01"
	got, err := f.sv.UpdateProfile(u, ProfileChange{FirstName: &first, Locale: &locale})
	if err != nil || got.FirstName != "Grace" || got.Locale != "fr" || got.LastName != "" {
		t.Fatalf("%+v %v", got, err)
	}
	_, err = f.sv.UpdateProfile(got, ProfileChange{LastName: &bad})
	wantRefusal(t, err, Invalid, msgBadName)
	xx := "xx"
	_, err = f.sv.UpdateProfile(got, ProfileChange{Locale: &xx})
	wantRefusal(t, err, Invalid, "locale must be one of it, en, fr, es, de")
	stored, _ := f.st.GetUser(u.ID)
	if stored.FirstName != "Grace" || stored.Locale != "fr" {
		t.Fatalf("%+v", stored)
	}
}

func TestUserManagement(t *testing.T) {
	f := newFixture(t)
	me := f.user(t, "admin@example.com", "admin")
	site, err := f.st.CreateSite(state.Site{Domain: "s.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/s.example.com", Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	form := UserForm{Email: "Op@Example.com", Password: pw, Role: "operator", SiteIDs: []int64{site.ID}, FirstName: " Ada "}
	for _, tc := range []struct {
		edit func(*UserForm)
		msg  string
	}{
		{func(u *UserForm) { u.Email = "Op <op@example.com>" }, errBadEmail.Error()},
		{func(u *UserForm) { u.Password = "short" }, errPasswordBounds.Error()},
		{func(u *UserForm) { u.Role = "" }, "role required: admin, operator or readonly"},
		{func(u *UserForm) { u.Role = "viewer" }, `unknown role "viewer": use admin, operator or readonly`},
		{func(u *UserForm) { u.SiteIDs = nil }, "assign at least one site to an operator"},
		{func(u *UserForm) { u.LastName = "x\x00" }, msgBadName},
		{func(u *UserForm) { u.SiteIDs = []int64{site.ID + 100} }, msgBadSites},
	} {
		bad := form
		tc.edit(&bad)
		_, err := f.sv.CreateUser(bg, me.Email, bad)
		wantRefusal(t, err, Invalid, tc.msg)
	}
	if n, _ := f.st.CountUsers(); n != 1 {
		t.Fatalf("refused creations left %d accounts", n)
	}
	op, err := f.sv.CreateUser(bg, me.Email, form)
	if err != nil || op.Email != "op@example.com" || op.FirstName != "Ada" || len(op.SiteIDs) != 1 {
		t.Fatalf("%+v %v", op, err)
	}
	_, err = f.sv.CreateUser(bg, me.Email, form)
	wantRefusal(t, err, Conflict, "a user with this email already exists")

	_, err = f.sv.UpdateUser(me, me.Email, op.ID, UserForm{Role: "boss"})
	wantRefusal(t, err, Invalid, "unknown role: use admin, operator or readonly")
	_, err = f.sv.UpdateUser(me, me.Email, 999, UserForm{Role: "readonly"})
	wantRefusal(t, err, NotFound, msgNoUser)
	_, err = f.sv.UpdateUser(me, me.Email, me.ID, UserForm{Role: "readonly"})
	wantRefusal(t, err, Invalid, "you cannot change your own role or disable your own account")
	_, err = f.sv.UpdateUser(me, me.Email, me.ID, UserForm{Role: "admin", Disabled: true})
	wantRefusal(t, err, Invalid, "you cannot change your own role or disable your own account")
	_, err = f.sv.UpdateUser(me, me.Email, op.ID, UserForm{Role: "operator", SiteIDs: []int64{site.ID, 777}, FirstName: "Changed"})
	wantRefusal(t, err, Invalid, msgBadSites)
	if got, _ := f.st.GetUser(op.ID); got.FirstName != "Ada" {
		t.Fatal("a refused edit changed the name")
	}
	got, err := f.sv.UpdateUser(me, me.Email, op.ID, UserForm{Role: "readonly", SiteIDs: []int64{site.ID}, FirstName: "Ada", LastName: "King", Disabled: true})
	if err != nil || got.Role != "readonly" || !got.Disabled || got.SiteIDs != nil || got.LastName != "King" {
		t.Fatalf("%+v %v", got, err)
	}

	wantRefusal(t, f.sv.ResetPassword(bg, me, me.Email, op.ID, "short"), Invalid, errPasswordBounds.Error())
	wantRefusal(t, f.sv.ResetPassword(bg, me, me.Email, me.ID, "a-new-password!"), Invalid, "change your own password from Security")
	wantRefusal(t, f.sv.ResetPassword(bg, me, me.Email, 999, "a-new-password!"), NotFound, msgNoUser)
	if err := f.sv.ResetPassword(bg, me, me.Email, op.ID, "a-new-password!"); err != nil {
		t.Fatal(err)
	}

	wantRefusal(t, f.sv.DeleteUser(me, me.Email, me.ID), Invalid, "you cannot delete your own account")
	wantRefusal(t, f.sv.DeleteUser(me, me.Email, 999), NotFound, msgNoUser)
	if err := f.sv.DeleteUser(me, me.Email, op.ID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.log[len(f.log)-1]
	f.mu.Unlock()
	if last.action != "user.delete" || last.subject != "op@example.com" {
		t.Fatalf("delete audited as %+v", last)
	}
	// The last enabled admin, from another admin's hands.
	other := f.user(t, "other@example.com", "admin")
	f.st.UpdateUserAccess(other.ID, "admin", true)
	_, err = f.sv.UpdateUser(other, other.Email, me.ID, UserForm{Role: "readonly"})
	wantRefusal(t, err, Invalid, state.ErrLastAdmin.Error())
	wantRefusal(t, f.sv.DeleteUser(other, other.Email, me.ID), Invalid, state.ErrLastAdmin.Error())
	if list, _ := f.sv.ListUsers(); len(list) != 2 {
		t.Fatalf("%d accounts", len(list))
	}
}
