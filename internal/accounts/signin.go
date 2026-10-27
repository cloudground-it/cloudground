package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Setup creates the first admin with the installer's one-time token (SPEC
// §2.3). Everything is checked before the token is spent, so a typo does
// not burn it.
func (sv *Service) Setup(ctx context.Context, email, password, token string) (state.User, error) {
	if n, err := sv.Store.CountUsers(); err != nil {
		return state.User{}, err
	} else if n > 0 {
		return state.User{}, refuse(Conflict, "setup already complete")
	}
	addr, err := NormalizeEmail(email)
	if err != nil {
		return state.User{}, refuse(Invalid, err.Error())
	}
	if err := CheckNewPassword(password); err != nil {
		return state.User{}, refuse(Invalid, err.Error())
	}
	if err := sv.Store.ConsumeSetupToken(token); err != nil {
		return state.User{}, refuse(Forbidden, "invalid or expired setup token")
	}
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return state.User{}, busy(err)
	}
	u, err := sv.Store.CreateUser(addr, hash, "admin")
	if err != nil {
		return state.User{}, err
	}
	markWhatsNewSeen(sv.Store, &u)
	sv.audit(u.Email, "setup.complete", "panel", "")
	return u, nil
}

// SignIn is a sign-in attempt.
type SignIn struct {
	Email, Password string
	// Code, RecoveryCode and Passkey (an assertion, as JSON) are the second
	// factor; at most one is given, each empty when absent.
	Code, RecoveryCode string
	Passkey            []byte
	// RP is the relying party the request reached the panel as, for a
	// passkey (passkeys.go).
	RP RP
	// Address is the client's (SPEC §2.7), for the log; Network is the
	// address's network (the address for IPv4, its /64 for IPv6), which
	// the per-address limits count. An empty Network counts Address. Wide
	// is an IPv6 address's /48, counted at a higher bound; empty for IPv4.
	Address, Network, Wide string
}

func (in SignIn) origin() Origin {
	return Origin{Address: in.Address, Network: in.Network, Wide: in.Wide}
}

const (
	msgMalformed    = "malformed request"
	msgBadLogin     = "invalid credentials"
	msgTooMany      = "too many attempts — wait a minute and try again"
	msgCodeRequired = "two-factor code required"
	msgCodeWrong    = "invalid two-factor code"
	msgCodeReused   = "that two-factor code has already been used — wait for the next one"
	msgRecoveryBad  = "invalid recovery code"
	msgDisabled     = "this account is disabled — ask an administrator"
	msgPasswordOff  = "password sign-in is turned off for this account — sign in with a passkey"
)

// factorsGiven counts the second factors a sign-in carries.
func (in SignIn) factorsGiven() int {
	return Proof{Code: in.Code, RecoveryCode: in.RecoveryCode, Passkey: in.Passkey}.factors()
}

func secondFactor(msg string) error {
	return &Error{Kind: Unauthorized, Msg: msg, SecondFactor: true}
}

// Login decides a sign-in (SPEC §2.3, steps 1 to 8) and returns the account
// to start a session for.
func (sv *Service) Login(ctx context.Context, in SignIn) (state.User, error) {
	if len(in.Email) > MaxEmailLen || len(in.Password) > MaxPasswordLen {
		return state.User{}, refuse(Invalid, msgMalformed)
	}
	if in.factorsGiven() > 1 {
		return state.User{}, refuse(Invalid, msgOneFactor)
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	o := in.origin()
	pair := o.network() + "|" + email
	if !sv.allowFrom(o) || !sv.Limits.AddressAccount.Allow(pair) {
		return state.User{}, refuse(Throttled, msgTooMany)
	}
	// Every e-mail, known or not, with or without a second factor, waits
	// the same for its recent failures, then is checked one at a time: the
	// delay says nothing about the account, and it slows a guesser's
	// network without locking the owner out.
	a, err := sv.begin(ctx, o, email)
	if err != nil {
		return state.User{}, err
	}
	defer a.end()

	u, lookup := sv.Store.GetUserByEmail(email)
	unknown := errors.Is(lookup, state.ErrNotFound)
	if lookup != nil && !unknown {
		return state.User{}, lookup
	}
	hash := u.PasswordHash
	if unknown {
		hash = decoy() // as long as a wrong password takes
	}
	ok, err := verifyPassword(ctx, in.Password, hash)
	if err != nil {
		return state.User{}, busy(err)
	}
	if unknown || !ok {
		a.Fail()
		sv.audit(printable(email), "login.failed", "panel", in.Address)
		sv.failed(in.Address)
		return state.User{}, refuse(Unauthorized, msgBadLogin)
	}
	// Only now, so that the answer does not tell which addresses exist.
	if u.Disabled {
		sv.audit(u.Email, "login.disabled", "panel", in.Address)
		return state.User{}, refuse(Forbidden, msgDisabled)
	}
	if !u.PasswordSignIn {
		sv.audit(u.Email, "login.password_off", "panel", in.Address)
		return state.User{}, &Error{Kind: Forbidden, Msg: msgPasswordOff, PasswordOff: true}
	}
	if u.HasSecondFactor() {
		if err := sv.checkSecondFactor(a, u, in); err != nil {
			return state.User{}, sv.withFactors(err, u, in.RP)
		}
	}
	sv.Limits.AddressAccount.Reset(pair)
	a.Succeed()
	sv.audit(u.Email, "login.success", "panel", in.Address)
	return u, nil
}

// checkSecondFactor is step 7, reached only with the right password: the
// answer then says a factor is needed, which the UI must know to ask for
// one.
func (sv *Service) checkSecondFactor(a *attempt, u state.User, in SignIn) error {
	if sv.Limits.Codes.Blocked(u.Email) {
		return &Error{Kind: Throttled, Msg: msgCodesCapped, SecondFactor: true}
	}
	switch {
	case len(in.Passkey) > 0:
		switch _, res, err := sv.verifyPasskey(u, in.RP, in.Passkey, purposeSecondFactor, false); {
		case err != nil:
			return err
		case res != passkeyOK:
			action := "login.passkey_failed"
			if res == passkeyCloned {
				action = "login.passkey_cloned"
			}
			sv.wrongFactor(a, u, in.Address)
			sv.audit(u.Email, action, "panel", in.Address)
			return secondFactor(msgPasskeyFailed)
		}
		return nil
	case in.RecoveryCode != "":
		left, ok, err := sv.spendRecoveryCode(u, in.RecoveryCode)
		if err != nil {
			return err
		}
		if !ok {
			sv.wrongFactor(a, u, in.Address)
			sv.audit(u.Email, "login.recovery_failed", "panel", in.Address)
			sv.failed(in.Address)
			return secondFactor(msgRecoveryBad)
		}
		sv.audit(u.Email, "login.recovery_code", "panel", fmt.Sprintf("from %s; %s", in.Address, codesLeft(left)))
		return nil
	case in.Code == "":
		return secondFactor(msgCodeRequired)
	case !u.TOTPEnabled:
		// A code for an account without TOTP is a wrong code.
		sv.wrongFactor(a, u, in.Address)
		sv.audit(u.Email, "login.totp_failed", "panel", in.Address)
		sv.failed(in.Address)
		return secondFactor(msgCodeWrong)
	}
	// A secret that cannot be read is a failure, never a skipped factor.
	secret, _, err := sv.Store.UserTOTP(u.ID)
	if err != nil {
		return err
	}
	res, err := sv.checkCode(a, u, secret, in.Code, in.Address)
	switch {
	case err != nil:
		return err
	case res == codeWrong:
		sv.audit(u.Email, "login.totp_failed", "panel", in.Address)
		sv.failed(in.Address)
		return secondFactor(msgCodeWrong)
	case res == codeReused:
		// The watermark only moves forward: of two sign-ins with one code,
		// one passes.
		sv.audit(u.Email, "login.totp_replay", "panel", in.Address)
		return secondFactor(msgCodeReused)
	}
	return nil
}

// withFactors adds to a second-factor refusal the factors the account can
// answer with and, when it has a passkey at rp, a fresh challenge for one.
func (sv *Service) withFactors(err error, u state.User, rp RP) error {
	ref, ok := IsRefusal(err)
	if !ok || !ref.SecondFactor {
		return err
	}
	fresh, gerr := sv.Store.GetUser(u.ID)
	if gerr != nil {
		fresh = u
	}
	var factors []string
	if fresh.TOTPEnabled {
		factors = append(factors, "totp")
	}
	// A challenge that cannot be issued (none at rp, too many waiting)
	// leaves the other factors: it never fails the sign-in.
	if opts, cerr := sv.assertionChallenge(fresh, rp, purposeSecondFactor, "preferred"); cerr == nil && opts != nil {
		factors = append(factors, "passkey")
		ref.PasskeyOptions = opts
	}
	if fresh.RecoveryCodesLeft > 0 {
		factors = append(factors, "recovery_code")
	}
	ref.Factors = factors
	return ref
}
