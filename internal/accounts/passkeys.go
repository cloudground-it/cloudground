package accounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Passkeys (SPEC §2.10): WebAuthn credentials as a second factor, or as the
// whole sign-in. github.com/go-webauthn/webauthn parses the ceremonies and
// checks their signatures; the records and the rules are the panel's.

// RP is the relying party a request reaches the panel as: its host name and
// the origin the browser is on. The API derives it from the request; an
// empty ID means the panel was reached by an address passkeys cannot work
// at (an IP).
type RP struct {
	ID, Origin string
}

func (rp RP) ok() bool { return rp.ID != "" && rp.Origin != "" }

const msgNoRelyingParty = "passkeys need the panel reached by its domain name over HTTPS"

// ceremonyTTL is how long a challenge waits for its answer.
const ceremonyTTL = 5 * time.Minute

const (
	// MaxCeremonies is how many challenges may wait across the panel.
	MaxCeremonies = 1024
	// perAccountCeremonies is how many may wait per account and purpose; a
	// new one past it drops the oldest.
	perAccountCeremonies = 5
)

// purpose is what a challenge was issued for; an answer is accepted only
// for the purpose, account and relying party it was issued for.
type purpose uint8

const (
	purposeSignIn       purpose = iota + 1 // a passkey alone, no account yet
	purposeSecondFactor                    // after the password
	purposeProof                           // confirming it is you (§2.4)
	purposeRegister                        // adding a passkey
)

type ceremony struct {
	purpose purpose
	userID  int64
	rp      RP
	session webauthn.SessionData
	expires time.Time
	seq     uint64
}

// ceremonies are the challenges waiting, by challenge. They live in the
// API's memory: a restart forgets them, which costs a retry at most.
type ceremonies struct {
	mu  sync.Mutex
	m   map[string]*ceremony
	seq uint64
}

func newCeremonies() *ceremonies { return &ceremonies{m: map[string]*ceremony{}} }

var errTooManyCeremonies = errors.New("too many passkey sign-ins at once — try again in a minute")

// put keeps a challenge for its answer. Registration keeps one per account;
// any other purpose at most perAccountCeremonies per account.
func (c *ceremonies) put(cer *ceremony, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if !now.Before(v.expires) {
			delete(c.m, k)
		}
	}
	if cer.userID != 0 {
		var mine []string
		for k, v := range c.m {
			if v.userID == cer.userID && v.purpose == cer.purpose {
				mine = append(mine, k)
			}
		}
		slices.SortFunc(mine, func(a, b string) int { return int(c.m[a].seq) - int(c.m[b].seq) })
		keep := perAccountCeremonies - 1
		if cer.purpose == purposeRegister {
			keep = 0
		}
		for len(mine) > keep {
			delete(c.m, mine[0])
			mine = mine[1:]
		}
	}
	if len(c.m) >= MaxCeremonies {
		return errTooManyCeremonies
	}
	c.seq++
	cer.seq, cer.expires = c.seq, now.Add(ceremonyTTL)
	c.m[cer.session.Challenge] = cer
	return nil
}

// take spends a challenge, right or wrong: nil when there is none waiting.
func (c *ceremonies) take(challenge string, now time.Time) *ceremony {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[challenge]
	if !ok {
		return nil
	}
	delete(c.m, challenge)
	if !now.Before(cer.expires) {
		return nil
	}
	return cer
}

// relyingParty is the go-webauthn instance for one relying party.
func (rp RP) relyingParty() (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:                  rp.ID,
		RPDisplayName:         Issuer,
		RPOrigins:             []string{rp.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL},
		},
	})
}

// waUser is an account as go-webauthn sees it: its user handle and its
// passkeys for one relying party.
type waUser struct {
	handle        []byte
	name, display string
	creds         []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.handle }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.display }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// credential is a stored passkey as go-webauthn checks it.
func credential(p state.Passkey) webauthn.Credential {
	transports := make([]protocol.AuthenticatorTransport, len(p.Transports))
	for i, t := range p.Transports {
		transports[i] = protocol.AuthenticatorTransport(t)
	}
	return webauthn.Credential{
		ID: p.CredentialID, PublicKey: p.PublicKey, Transport: transports,
		Flags:         webauthn.CredentialFlags{UserPresent: true, BackupEligible: p.BackupEligible, BackupState: p.BackupState},
		Authenticator: webauthn.Authenticator{AAGUID: p.AAGUID, SignCount: p.SignCount},
	}
}

// webauthnUser loads the account's handle and its passkeys for rp (all of
// them with rp zero), with the stored records by credential id.
func (sv *Service) webauthnUser(u state.User, rp RP) (*waUser, []state.Passkey, error) {
	handle, err := sv.Store.WebAuthnID(u.ID)
	if err != nil {
		return nil, nil, err
	}
	all, err := sv.Store.Passkeys(u.ID)
	if err != nil {
		return nil, nil, err
	}
	display := u.Email
	if n := fullName(u); n != "" {
		display = n
	}
	wu := &waUser{handle: handle, name: u.Email, display: display}
	var mine []state.Passkey
	for _, p := range all {
		if rp.ID == "" || p.RPID == rp.ID {
			wu.creds = append(wu.creds, credential(p))
			mine = append(mine, p)
		}
	}
	return wu, mine, nil
}

func fullName(u state.User) string {
	switch {
	case u.FirstName != "" && u.LastName != "":
		return u.FirstName + " " + u.LastName
	case u.FirstName != "":
		return u.FirstName
	}
	return u.LastName
}

// assertionChallenge issues request options for the account's passkeys at
// rp, for purpose p; nil options when it has none there.
func (sv *Service) assertionChallenge(u state.User, rp RP, p purpose, uv protocol.UserVerificationRequirement) (*protocol.CredentialAssertion, error) {
	if !rp.ok() {
		return nil, nil
	}
	wu, mine, err := sv.webauthnUser(u, rp)
	if err != nil || len(mine) == 0 {
		return nil, err
	}
	wa, err := rp.relyingParty()
	if err != nil {
		return nil, err
	}
	opts, session, err := wa.BeginLogin(wu, webauthn.WithUserVerification(uv))
	if err != nil {
		return nil, err
	}
	if err := sv.ceremonies.put(&ceremony{purpose: p, userID: u.ID, rp: rp, session: *session}, sv.now()); err != nil {
		return nil, err
	}
	return opts, nil
}

// passkeyResult is what checking an assertion found.
type passkeyResult uint8

const (
	passkeyOK passkeyResult = iota
	passkeyBad
	passkeyNoUV
	passkeyCloned
)

// verifyPasskey checks an assertion of one of u's passkeys at rp against a
// challenge issued for purpose p, spending the challenge; uv requires user
// verification. A passkey that passes has its counter and use recorded.
// The error is the store's.
func (sv *Service) verifyPasskey(u state.User, rp RP, raw []byte, p purpose, uv bool) (state.Passkey, passkeyResult, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return state.Passkey{}, passkeyBad, nil
	}
	cer := sv.ceremonies.take(parsed.Response.CollectedClientData.Challenge, sv.now())
	if cer == nil || cer.purpose != p || cer.userID != u.ID || cer.rp != rp {
		return state.Passkey{}, passkeyBad, nil
	}
	wu, mine, err := sv.webauthnUser(u, rp)
	if err != nil {
		return state.Passkey{}, passkeyBad, err
	}
	wa, err := rp.relyingParty()
	if err != nil {
		return state.Passkey{}, passkeyBad, err
	}
	cred, err := wa.ValidateLogin(wu, cer.session, parsed)
	if err != nil {
		return state.Passkey{}, passkeyBad, nil
	}
	return sv.acceptPasskey(mine, cred, parsed, uv)
}

// acceptPasskey finishes a valid assertion: the counter must have moved
// forward, user verification be there when required; then the use is
// recorded.
func (sv *Service) acceptPasskey(mine []state.Passkey, cred *webauthn.Credential, parsed *protocol.ParsedCredentialAssertionData, uv bool) (state.Passkey, passkeyResult, error) {
	i := slices.IndexFunc(mine, func(p state.Passkey) bool { return bytes.Equal(p.CredentialID, cred.ID) })
	if i < 0 {
		return state.Passkey{}, passkeyBad, nil
	}
	pk := mine[i]
	if cred.Authenticator.CloneWarning {
		return pk, passkeyCloned, nil
	}
	if uv && !parsed.Response.AuthenticatorData.Flags.HasUserVerified() {
		return pk, passkeyNoUV, nil
	}
	if err := sv.Store.RecordPasskeyUse(pk.ID, cred.Authenticator.SignCount, parsed.Response.AuthenticatorData.Flags.HasBackupState()); err != nil {
		return pk, passkeyBad, err
	}
	return pk, passkeyOK, nil
}

// BeginPasskeySignIn issues the challenge of a sign-in with a passkey alone
// (SPEC §2.3): no account named, user verification required.
func (sv *Service) BeginPasskeySignIn(from Origin, rp RP) (*protocol.CredentialAssertion, error) {
	if !sv.allowFrom(from) {
		return nil, refuse(Throttled, msgTooMany)
	}
	if !rp.ok() {
		return nil, refuse(Invalid, msgNoRelyingParty)
	}
	wa, err := rp.relyingParty()
	if err != nil {
		return nil, err
	}
	opts, session, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, err
	}
	if err := sv.ceremonies.put(&ceremony{purpose: purposeSignIn, rp: rp, session: *session}, sv.now()); err != nil {
		return nil, refuse(Throttled, err.Error())
	}
	return opts, nil
}

const msgPasskeyFailed = "the passkey could not be verified"

// PasskeySignIn decides a sign-in with a passkey alone and returns the
// account to start a session for. Its failures count towards no e-mail's
// delay: an assertion cannot be guessed.
func (sv *Service) PasskeySignIn(from Origin, rp RP, raw []byte) (state.User, error) {
	if !sv.allowFrom(from) {
		return state.User{}, refuse(Throttled, msgTooMany)
	}
	if !rp.ok() {
		return state.User{}, refuse(Invalid, msgNoRelyingParty)
	}
	fail := func(actor, action string) (state.User, error) {
		sv.audit(actor, action, "panel", from.Address)
		return state.User{}, refuse(Unauthorized, msgPasskeyFailed)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return fail("unknown passkey", "login.passkey_failed")
	}
	// Spent first, whatever follows.
	cer := sv.ceremonies.take(parsed.Response.CollectedClientData.Challenge, sv.now())
	pk, err := sv.Store.PasskeyByCredentialID(parsed.RawID)
	if errors.Is(err, state.ErrNotFound) {
		return fail("unknown passkey", "login.passkey_failed")
	}
	if err != nil {
		return state.User{}, err
	}
	u, err := sv.Store.GetUser(pk.UserID)
	if err != nil {
		return state.User{}, err
	}
	if cer == nil || cer.purpose != purposeSignIn || cer.rp != rp {
		return fail(u.Email, "login.passkey_failed")
	}
	wu, mine, err := sv.webauthnUser(u, rp)
	if err != nil {
		return state.User{}, err
	}
	wa, err := rp.relyingParty()
	if err != nil {
		return state.User{}, err
	}
	// The assertion's user handle must be this account's: the handler
	// refuses any other.
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		if !bytes.Equal(userHandle, wu.handle) {
			return nil, errors.New("the user handle is not the passkey's account")
		}
		return wu, nil
	}
	cred, err := wa.ValidateDiscoverableLogin(handler, cer.session, parsed)
	if err != nil {
		return fail(u.Email, "login.passkey_failed")
	}
	pk, res, err := sv.acceptPasskey(mine, cred, parsed, true)
	switch {
	case err != nil:
		return state.User{}, err
	case res == passkeyCloned:
		return fail(u.Email, "login.passkey_cloned")
	case res != passkeyOK:
		return fail(u.Email, "login.passkey_failed")
	}
	if u.Disabled {
		sv.audit(u.Email, "login.disabled", "panel", from.Address)
		return state.User{}, refuse(Forbidden, msgDisabled)
	}
	sv.audit(u.Email, "login.passkey", "panel", fmt.Sprintf("passkey %q from %s", pk.Name, from.Address))
	return u, nil
}

// PasskeyView is one of the account's passkeys as the list shows it.
type PasskeyView struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	RPID       string     `json:"rp_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	BackedUp   bool       `json:"backed_up"`
}

func passkeyView(p state.Passkey) PasskeyView {
	v := PasskeyView{ID: p.ID, Name: p.Name, RPID: p.RPID, CreatedAt: p.CreatedAt, BackedUp: p.BackupState}
	if !p.LastUsedAt.IsZero() {
		used := p.LastUsedAt
		v.LastUsedAt = &used
	}
	return v
}

// ListPasskeys lists the account's passkeys, oldest first.
func (sv *Service) ListPasskeys(u state.User) ([]PasskeyView, error) {
	all, err := sv.Store.Passkeys(u.ID)
	if err != nil {
		return nil, err
	}
	views := make([]PasskeyView, 0, len(all))
	for _, p := range all {
		views = append(views, passkeyView(p))
	}
	return views, nil
}

// ProofChallenge issues request options for confirming it is you with a
// passkey (SPEC §2.10): user verification required while password sign-in
// is off.
func (sv *Service) ProofChallenge(u state.User, rp RP) (*protocol.CredentialAssertion, error) {
	if !rp.ok() {
		return nil, refuse(Invalid, msgNoRelyingParty)
	}
	uv := protocol.VerificationPreferred
	if !u.PasswordSignIn {
		uv = protocol.VerificationRequired
	}
	opts, err := sv.assertionChallenge(u, rp, purposeProof, uv)
	switch {
	case errors.Is(err, errTooManyCeremonies):
		return nil, refuse(Throttled, err.Error())
	case err != nil:
		return nil, err
	case opts == nil:
		return nil, refuse(Invalid, "you have no passkey for this address")
	}
	return opts, nil
}

// BeginPasskey starts adding a passkey once the proof checks out, and
// returns the creation options.
func (sv *Service) BeginPasskey(ctx context.Context, from Origin, u state.User, rp RP, actor string, p Proof) (*protocol.CredentialCreation, error) {
	if !rp.ok() {
		return nil, refuse(Invalid, msgNoRelyingParty)
	}
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return nil, err
	}
	defer a.end()
	if err := sv.confirm(ctx, a, u, rp, p, confirmation{refusal: msgConfirmFactor, purpose: "add a passkey", actor: actor}); err != nil {
		return nil, err
	}
	if u.Passkeys >= state.MaxPasskeysPerUser {
		return nil, refuse(Invalid, state.ErrTooManyPasskeys.Error())
	}
	wu, mine, err := sv.webauthnUser(u, rp)
	if err != nil {
		return nil, err
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(mine))
	for _, c := range wu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	wa, err := rp.relyingParty()
	if err != nil {
		return nil, err
	}
	opts, session, err := wa.BeginRegistration(wu,
		webauthn.WithExclusions(exclude),
		webauthn.WithCredentialParameters(webauthn.CredentialParametersRecommendedL3()),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired))
	if err != nil {
		return nil, err
	}
	if err := sv.ceremonies.put(&ceremony{purpose: purposeRegister, userID: u.ID, rp: rp, session: *session}, sv.now()); err != nil {
		return nil, refuse(Throttled, err.Error())
	}
	return opts, nil
}

// FinishPasskey stores the passkey a registration made, ends the account's
// other sessions, and returns it with fresh recovery codes when it is the
// account's first factor.
func (sv *Service) FinishPasskey(u state.User, rp RP, actor, name string, raw []byte, keepToken string) (PasskeyView, []string, error) {
	if !rp.ok() {
		return PasskeyView{}, nil, refuse(Invalid, msgNoRelyingParty)
	}
	name, ok := CleanName(name)
	if !ok || name == "" {
		return PasskeyView{}, nil, refuse(Invalid, "name the passkey: up to 64 characters")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return PasskeyView{}, nil, refuse(Invalid, msgPasskeyFailed)
	}
	cer := sv.ceremonies.take(parsed.Response.CollectedClientData.Challenge, sv.now())
	if cer == nil || cer.purpose != purposeRegister || cer.userID != u.ID || cer.rp != rp {
		return PasskeyView{}, nil, refuse(Invalid, "start adding the passkey again")
	}
	wu, _, err := sv.webauthnUser(u, rp)
	if err != nil {
		return PasskeyView{}, nil, err
	}
	wa, err := rp.relyingParty()
	if err != nil {
		return PasskeyView{}, nil, err
	}
	cred, err := wa.CreateCredential(wu, cer.session, parsed)
	if err != nil {
		return PasskeyView{}, nil, refuse(Invalid, msgPasskeyFailed)
	}
	before, err := sv.Store.GetUser(u.ID)
	if err != nil {
		return PasskeyView{}, nil, err
	}
	transports := make([]string, len(cred.Transport))
	for i, t := range cred.Transport {
		transports[i] = string(t)
	}
	pk, err := sv.Store.AddPasskey(state.Passkey{
		UserID: u.ID, CredentialID: cred.ID, PublicKey: cred.PublicKey, SignCount: cred.Authenticator.SignCount,
		AAGUID: cred.Authenticator.AAGUID, Transports: transports,
		BackupEligible: cred.Flags.BackupEligible, BackupState: cred.Flags.BackupState, RPID: rp.ID, Name: name,
	})
	switch {
	case errors.Is(err, state.ErrPasskeyTaken):
		return PasskeyView{}, nil, refuse(Conflict, err.Error())
	case errors.Is(err, state.ErrTooManyPasskeys):
		return PasskeyView{}, nil, refuse(Invalid, err.Error())
	case err != nil:
		return PasskeyView{}, nil, err
	}
	_ = sv.Store.RevokeOtherSessions(u.ID, keepToken)
	sv.audit(actor, "passkey.added", "self", name)
	var codes []string
	if !before.HasSecondFactor() {
		if codes, err = sv.issueRecoveryCodes(u.ID); err != nil {
			return passkeyView(pk), nil, err
		}
	}
	return passkeyView(pk), codes, nil
}

// RemovePasskey removes one of the account's passkeys once the proof checks
// out, and ends its other sessions.
func (sv *Service) RemovePasskey(ctx context.Context, from Origin, u state.User, rp RP, actor string, id int64, p Proof, keepToken string) error {
	all, err := sv.Store.Passkeys(u.ID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(all, func(pk state.Passkey) bool { return pk.ID == id }) {
		return refuse(NotFound, "passkey not found")
	}
	if len(all) == 1 && !u.PasswordSignIn {
		return refuse(Invalid, state.ErrLastPasskey.Error())
	}
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return err
	}
	defer a.end()
	if err := sv.confirm(ctx, a, u, rp, p, confirmation{refusal: msgConfirmFactor, purpose: "remove a passkey", actor: actor}); err != nil {
		return err
	}
	removed, _, err := sv.Store.RemovePasskey(u.ID, id)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return refuse(NotFound, "passkey not found")
	case errors.Is(err, state.ErrLastPasskey):
		return refuse(Invalid, err.Error())
	case err != nil:
		return err
	}
	_ = sv.Store.RevokeOtherSessions(u.ID, keepToken)
	sv.audit(actor, "passkey.removed", "self", removed.Name)
	return nil
}

// SetPasswordSignIn turns the account's password sign-in on or off once the
// proof checks out; off takes a passkey with user verification. It returns
// the account as it is now.
func (sv *Service) SetPasswordSignIn(ctx context.Context, from Origin, u state.User, rp RP, actor string, on bool, p Proof, keepToken string) (state.User, error) {
	if on == u.PasswordSignIn {
		return u, nil
	}
	if !on && u.Passkeys == 0 {
		return u, refuse(Invalid, state.ErrNoPasskey.Error())
	}
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return u, err
	}
	defer a.end()
	if err := sv.confirm(ctx, a, u, rp, p, confirmation{refusal: msgConfirmFactor, purpose: "change password sign-in", actor: actor, passkeyUV: !on}); err != nil {
		return u, err
	}
	switch err := sv.Store.SetPasswordSignIn(u.ID, on); {
	case errors.Is(err, state.ErrNoPasskey):
		return u, refuse(Invalid, err.Error())
	case err != nil:
		return u, err
	}
	_ = sv.Store.RevokeOtherSessions(u.ID, keepToken)
	action := "password_signin.off"
	if on {
		action = "password_signin.on"
	}
	sv.audit(actor, action, "self", "")
	u.PasswordSignIn = on
	return u, nil
}
