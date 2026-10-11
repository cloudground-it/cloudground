package accounts

import (
	"context"
	"errors"
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
	sv.audit(u.Email, "setup.complete", "panel", "")
	return u, nil
}

// SignIn is a sign-in attempt.
type SignIn struct {
	Email, Password string
	// Code is the second factor's, empty when none was given.
	Code string
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
)

func secondFactor(msg string) error {
	return &Error{Kind: Unauthorized, Msg: msg, SecondFactor: true}
}

// Login decides a sign-in (SPEC §2.3, steps 1 to 8) and returns the account
// to start a session for.
func (sv *Service) Login(ctx context.Context, in SignIn) (state.User, error) {
	if len(in.Email) > MaxEmailLen || len(in.Password) > MaxPasswordLen {
		return state.User{}, refuse(Invalid, msgMalformed)
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
		return state.User{}, refuse(Forbidden, "this account is disabled — ask an administrator")
	}
	if u.TOTPEnabled {
		if err := sv.checkSecondFactor(a, u, in); err != nil {
			return state.User{}, err
		}
	}
	sv.Limits.AddressAccount.Reset(pair)
	a.Succeed()
	sv.audit(u.Email, "login.success", "panel", in.Address)
	return u, nil
}

// checkSecondFactor is step 7, reached only with the right password: the
// answer then says a code is needed, which the UI must know to ask for one.
func (sv *Service) checkSecondFactor(a *attempt, u state.User, in SignIn) error {
	if sv.Limits.Codes.Blocked(u.Email) {
		return &Error{Kind: Throttled, Msg: msgCodesCapped, SecondFactor: true}
	}
	if in.Code == "" {
		return secondFactor(msgCodeRequired)
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
