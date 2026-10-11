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
	// Disabled accounts cannot sign in, and their sessions are closed.
	Disabled bool `json:"disabled"`
	// SiteIDs are an operator's assigned sites.
	SiteIDs     []int64   `json:"site_ids,omitempty"`
	TOTPEnabled bool      `json:"totp_enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// Session is a signed-in browser session.
type Session struct {
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	LastSeen  time.Time `json:"last_seen"`
	UserAgent string    `json:"user_agent"`
	IP        string    `json:"ip"`
}
