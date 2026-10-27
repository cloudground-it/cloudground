package state

import "time"

// User is a panel account.
type User struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
	// Role is admin, operator or readonly.
	Role      string `json:"role"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	// Locale is the interface language chosen (one of Locales); empty
	// when the account has not chosen.
	Locale string `json:"locale"`
	// WhatsNewSeen is the release whose changelog entry the account last
	// saw ("v0.3.2"); empty when it has seen none (#106).
	WhatsNewSeen string `json:"whats_new_seen"`
	// Disabled accounts cannot sign in, and their sessions are closed.
	Disabled bool `json:"disabled"`
	// SiteIDs are an operator's assigned sites.
	SiteIDs     []int64   `json:"site_ids,omitempty"`
	TOTPEnabled bool      `json:"totp_enabled"`
	CreatedAt   time.Time `json:"created_at"`
	// PasswordSignIn: the account's password signs in (api SPEC §2.10);
	// off only for an account with a passkey that turned it off.
	PasswordSignIn bool `json:"password_signin"`
	// Passkeys is how many passkeys the account has.
	Passkeys int `json:"passkeys"`
	// RecoveryCodesLeft is how many of its recovery codes are unused.
	RecoveryCodesLeft int `json:"recovery_codes_left"`
}

// HasSecondFactor reports whether the account has two-factor: TOTP on or a
// passkey (api SPEC §2.1). Recovery codes are not a factor of their own.
func (u User) HasSecondFactor() bool { return u.TOTPEnabled || u.Passkeys > 0 }

// Session is a signed-in browser session.
type Session struct {
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	LastSeen  time.Time `json:"last_seen"`
	UserAgent string    `json:"user_agent"`
	IP        string    `json:"ip"`
}
