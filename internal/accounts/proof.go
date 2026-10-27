package accounts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Confirming it is you (SPEC §2.4): what the routes that change how an
// account signs in take, so that a stolen session alone cannot add, replace
// or remove a factor.

// Proof is a confirmation: the password while it signs in, and one current
// factor when the account has two-factor.
type Proof struct {
	Password     string          `json:"password"`
	Code         string          `json:"code"`
	RecoveryCode string          `json:"recovery_code"`
	Passkey      json.RawMessage `json:"passkey"`
}

// hasPasskey reports whether the proof carries a passkey assertion (a JSON
// null is none).
func (p Proof) hasPasskey() bool { return len(p.Passkey) > 0 && string(p.Passkey) != "null" }

// factors counts the second factors the proof gives.
func (p Proof) factors() int {
	n := 0
	for _, given := range []bool{p.Code != "", p.RecoveryCode != "", p.hasPasskey()} {
		if given {
			n++
		}
	}
	return n
}

const (
	msgOneFactor      = "give one second factor at a time"
	msgConfirmFactor  = "confirm with a code, a passkey or a recovery code"
	msgConfirmPasskey = "confirm with one of your passkeys"
)

// confirmation is what one route asks of a proof.
type confirmation struct {
	// refusal is the route's answer when the factor is missing or wrong.
	refusal string
	// purpose names the route in the audit of a spent recovery code.
	purpose string
	actor   string
	// passkeyUV: the factor must be a passkey with user verification
	// (turning password sign-in off).
	passkeyUV bool
}

// confirm checks a proof in attempt a (gate): the password while it signs
// in (with password sign-in off, a passkey with user verification instead),
// then one current factor when the account has two-factor, which is spent.
// Wrong passwords, codes and recovery codes count as at sign-in.
func (sv *Service) confirm(ctx context.Context, a *attempt, u state.User, rp RP, p Proof, c confirmation) error {
	if p.factors() > 1 {
		return refuse(Invalid, msgOneFactor)
	}
	// The account as it is now, not as the request found it: a factor
	// added or removed in between changes what is asked.
	fresh, err := sv.Store.GetUser(u.ID)
	if err != nil {
		return err
	}
	u = fresh
	if !u.PasswordSignIn {
		// The passkey is the whole credential here: it must check who holds
		// it (a PIN or biometrics).
		if !p.hasPasskey() {
			a.Fail()
			return refuse(Forbidden, msgConfirmPasskey)
		}
		return sv.confirmPasskey(a, u, rp, p.Passkey, true, msgConfirmPasskey)
	}
	if err := sv.checkPassword(ctx, a, u, p.Password, msgWrongPassword); err != nil {
		return err
	}
	if c.passkeyUV {
		if !p.hasPasskey() {
			return refuse(Forbidden, msgConfirmPasskey)
		}
		return sv.confirmPasskey(a, u, rp, p.Passkey, true, msgConfirmPasskey)
	}
	if !u.HasSecondFactor() {
		return nil
	}
	if sv.Limits.Codes.Blocked(u.Email) {
		return refuse(Throttled, msgCodesCapped)
	}
	switch {
	case p.hasPasskey():
		return sv.confirmPasskey(a, u, rp, p.Passkey, false, c.refusal)
	case p.RecoveryCode != "":
		left, ok, err := sv.spendRecoveryCode(u, p.RecoveryCode)
		if err != nil {
			return err
		}
		if !ok {
			sv.wrongFactor(a, u, "the account's own session")
			return refuse(Forbidden, c.refusal)
		}
		sv.audit(c.actor, "recovery_code.used", "self", fmt.Sprintf("to %s; %s", c.purpose, codesLeft(left)))
		return nil
	case p.Code != "" && u.TOTPEnabled:
		secret, _, err := sv.Store.UserTOTP(u.ID)
		if err != nil {
			return err
		}
		return sv.spendCurrentCode(a, u, secret, p.Code, c.refusal)
	case p.Code != "":
		// A code for an account without TOTP is a wrong code.
		sv.wrongFactor(a, u, "the account's own session")
		return refuse(Forbidden, c.refusal)
	}
	a.Fail()
	return refuse(Forbidden, c.refusal)
}

// confirmPasskey checks a proof's passkey assertion against a challenge
// from ProofChallenge. A failure counts as one; it cannot be guessed, so it
// does not count towards the code cap.
func (sv *Service) confirmPasskey(a *attempt, u state.User, rp RP, raw []byte, uv bool, refusal string) error {
	if !rp.ok() {
		return refuse(Invalid, msgNoRelyingParty)
	}
	switch _, res, err := sv.verifyPasskey(u, rp, raw, purposeProof, uv); {
	case err != nil:
		return err
	case res == passkeyNoUV:
		a.Fail()
		return refuse(Forbidden, msgConfirmPasskey)
	case res != passkeyOK:
		a.Fail()
		if res == passkeyCloned {
			sv.audit(u.Email, "login.passkey_cloned", "panel", "the passkey's signature counter went backwards")
		}
		return refuse(Forbidden, refusal)
	}
	return nil
}
