package accounts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The caller's changes to their own account (SPEC §2.4). Each takes the
// account the session belongs to and the actor to audit.

// ChangePassword replaces the account's password once the current one
// checks out. The store ends every session and API token of the account;
// the caller starts a fresh session for the device that asked.
func (sv *Service) ChangePassword(ctx context.Context, from Origin, u state.User, actor, current, next string) error {
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return err
	}
	defer a.end()
	if err := sv.checkPassword(ctx, a, u, current, "current password is incorrect"); err != nil {
		return err
	}
	if err := CheckNewPassword(next); err != nil {
		return refuse(Invalid, "new "+err.Error())
	}
	hash, err := hashPassword(ctx, next)
	if err != nil {
		return busy(err)
	}
	if err := sv.Store.UpdatePassword(u.ID, hash); err != nil {
		return err
	}
	sv.audit(actor, "password.change", "self", "")
	return nil
}

// spendCurrentCode checks a code of the active secret for enrolment and
// disabling. Any code that does not pass counts as a failure, and a wrong
// one towards the code cap, as at sign-in.
func (sv *Service) spendCurrentCode(a *attempt, u state.User, secret, code, refusal string) error {
	res, err := sv.checkCode(a, u, secret, code, "the account's own session")
	switch {
	case err != nil:
		return err
	case res == codeReused:
		a.Fail()
		fallthrough
	case res == codeWrong:
		return refuse(Forbidden, refusal)
	}
	return nil
}

// Enrolment is a pending second factor to show the account's owner.
type Enrolment struct {
	Secret, URI string
}

// BeginTOTP makes a new pending secret once the proof checks out (SPEC
// §2.4): the password, and a current factor when the account has
// two-factor; a stolen session alone must not be able to put its own
// authenticator in the owner's place. The active secret keeps working until
// the new one is confirmed.
func (sv *Service) BeginTOTP(ctx context.Context, from Origin, u state.User, rp RP, actor string, p Proof) (Enrolment, error) {
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return Enrolment{}, err
	}
	defer a.end()
	refusal := msgConfirmFactor
	if fresh, err := sv.Store.GetUser(u.ID); err != nil {
		return Enrolment{}, err
	} else if fresh.TOTPEnabled {
		refusal = "enter a code from your current authenticator to replace it"
	}
	if err := sv.confirm(ctx, a, u, rp, p, confirmation{refusal: refusal, purpose: "set up an authenticator app", actor: actor}); err != nil {
		return Enrolment{}, err
	}
	secret := NewTOTPSecret()
	if err := sv.Store.SetPendingTOTP(u.ID, secret); err != nil {
		return Enrolment{}, err
	}
	return Enrolment{Secret: secret, URI: TOTPURI(secret, u.Email)}, nil
}

// ConfirmTOTP turns 2FA on with the pending secret once a code from it
// matches, and ends the account's sessions but keepToken's. The store
// activates exactly the secret the code was checked against: an enrolment
// begun in between is refused, not switched on unchecked. When it is the
// account's first factor, it returns its recovery codes, to show once.
func (sv *Service) ConfirmTOTP(u state.User, actor, code, keepToken string) ([]string, error) {
	pending, err := sv.Store.PendingTOTP(u.ID)
	if err != nil || pending == "" {
		return nil, refuse(Invalid, "start enrollment first")
	}
	step, ok := MatchTOTP(pending, code, sv.now())
	if !ok {
		return nil, refuse(Invalid, "code did not match — check your device clock")
	}
	before, err := sv.Store.GetUser(u.ID)
	if err != nil {
		return nil, err
	}
	// The confirming code is spent like a sign-in's: the watermark starts
	// at its step, so it cannot also sign in (it could, once, before).
	switch err := sv.Store.ActivatePendingTOTPAt(u.ID, pending, step); {
	case errors.Is(err, state.ErrPendingChanged):
		return nil, refuse(Conflict, "another enrolment was started meanwhile: confirm with a code from the newest secret")
	case errors.Is(err, state.ErrNotFound):
		return nil, refuse(Invalid, "start enrollment first")
	case err != nil:
		return nil, err
	}
	_ = sv.Store.RevokeOtherSessions(u.ID, keepToken)
	sv.audit(actor, "2fa.enabled", "self", "")
	if before.HasSecondFactor() {
		return nil, nil
	}
	return sv.issueRecoveryCodes(u.ID)
}

// DisableTOTP turns TOTP off once the proof checks out (SPEC §2.4), so a
// stolen session cannot strip the factor; failures count as at sign-in. The
// store also revokes the account's API tokens, and its recovery codes when
// no passkey is left.
func (sv *Service) DisableTOTP(ctx context.Context, from Origin, u state.User, rp RP, actor string, p Proof, keepToken string) error {
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return err
	}
	defer a.end()
	if err := sv.confirm(ctx, a, u, rp, p, confirmation{refusal: "enter a code from your authenticator to turn it off", purpose: "turn off the authenticator app", actor: actor}); err != nil {
		return err
	}
	if err := sv.Store.DisableUserTOTP(u.ID); err != nil {
		return err
	}
	_ = sv.Store.RevokeOtherSessions(u.ID, keepToken)
	sv.audit(actor, "2fa.disabled", "self", "")
	return nil
}

// SessionView is one of the account's sessions as the list shows it.
type SessionView struct {
	ID        string    `json:"id"` // the first 12 hex digits of the token's hash
	Current   bool      `json:"current"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	LastSeen  time.Time `json:"last_seen"`
	Browser   string    `json:"browser"`
	OS        string    `json:"os"`
	IP        string    `json:"ip"`
}

const sessionIDLen = 12

// Sessions lists the account's live sessions; currentToken's is marked.
func (sv *Service) Sessions(u state.User, currentToken string) ([]SessionView, error) {
	list, err := sv.Store.ListSessionsForUser(u.ID)
	if err != nil {
		return nil, err
	}
	mine := ""
	if currentToken != "" {
		mine = state.TokenHash(currentToken)
	}
	views := make([]SessionView, 0, len(list))
	for _, se := range list {
		browser, os := Client(se.UserAgent)
		views = append(views, SessionView{
			ID: se.TokenHash[:sessionIDLen], Current: se.TokenHash == mine,
			CreatedAt: se.CreatedAt, ExpiresAt: se.ExpiresAt, LastSeen: se.LastSeen,
			Browser: browser, OS: os, IP: se.IP,
		})
	}
	return views, nil
}

// RevokeSession ends the first of the account's sessions whose hash starts
// with prefix.
func (sv *Service) RevokeSession(u state.User, actor, prefix string) error {
	list, err := sv.Store.ListSessionsForUser(u.ID)
	if err != nil {
		return err
	}
	for _, se := range list {
		if !strings.HasPrefix(se.TokenHash, prefix) {
			continue
		}
		if err := sv.Store.DeleteSessionByHash(se.TokenHash); err != nil {
			return err
		}
		sv.audit(actor, "session.revoke", "self", "")
		return nil
	}
	return refuse(NotFound, "session not found")
}

// ProfileChange is an edit of one's own profile; nil leaves a field alone.
type ProfileChange struct {
	FirstName, LastName, Locale *string
}

// UpdateProfile applies a profile edit and returns the account as it is
// now.
func (sv *Service) UpdateProfile(u state.User, ch ProfileChange) (state.User, error) {
	if ch.Locale != nil && !state.ValidLocale(*ch.Locale) {
		return u, refuse(Invalid, "locale must be one of "+strings.Join(state.Locales, ", "))
	}
	if ch.FirstName != nil || ch.LastName != nil {
		first, last := u.FirstName, u.LastName
		ok := true
		if ch.FirstName != nil {
			first, ok = CleanName(*ch.FirstName)
		}
		if ch.LastName != nil && ok {
			last, ok = CleanName(*ch.LastName)
		}
		if !ok {
			return u, refuse(Invalid, msgBadName)
		}
		if err := sv.Store.UpdateUserProfile(u.ID, first, last); err != nil {
			return u, err
		}
		u.FirstName, u.LastName = first, last
	}
	if ch.Locale != nil {
		if err := sv.Store.UpdateUserLocale(u.ID, *ch.Locale); err != nil {
			return u, err
		}
		u.Locale = *ch.Locale
	}
	return u, nil
}

// Client names the browser and operating system of a User-Agent, coarsely:
// enough for someone to recognise their own sessions.
func Client(userAgent string) (browser, os string) {
	has := func(s string) bool { return strings.Contains(userAgent, s) }
	// Order matters: Edge and Opera say Chrome too, Chrome says Safari.
	switch {
	case has("Edg/"):
		browser = "Edge"
	case has("OPR/"):
		browser = "Opera"
	case has("Firefox/"):
		browser = "Firefox"
	case has("Chrome/"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	case strings.HasPrefix(userAgent, "curl/"):
		browser = "curl"
	}
	// iOS says "like Mac OS X", Android says Linux.
	switch {
	case has("iPhone"), has("iPad"):
		os = "iOS"
	case has("Android"):
		os = "Android"
	case has("Mac OS X"), has("Macintosh"):
		os = "macOS"
	case has("Windows"):
		os = "Windows"
	case has("Linux"):
		os = "Linux"
	}
	return browser, os
}
