package state

import (
	"database/sql"
	"errors"
	"time"
)

// APIToken is a bearer credential for cgctl and scripts (SPEC.md, API
// tokens). The secret itself is never stored, only its SHA-256.
type APIToken struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"user_id"`
	Email  string `json:"email"` // the owner's, for the list
	Name   string `json:"name"`
	// Hint is the token's first characters, so an operator can tell which
	// one a script uses.
	Hint       string     `json:"hint"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// CreateAPIToken stores a token by the SHA-256 of its secret. A zero
// expiry means it does not expire.
func (s *Store) CreateAPIToken(userID int64, name, secret, hint string, expires time.Time) (APIToken, error) {
	ts := now()
	exp := ""
	if !expires.IsZero() {
		exp = expires.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.Exec(`INSERT INTO api_tokens (user_id, name, token_hash, hint, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, name, hashToken(secret), hint, ts, exp)
	if err != nil {
		return APIToken{}, err
	}
	id, _ := res.LastInsertId()
	return s.apiToken(`t.id = ?`, id)
}

const apiTokenCols = `t.id, t.user_id, u.email, t.name, t.hint, t.created_at, t.expires_at, t.last_used_at`

func scanAPIToken(sc interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var created, expires, used string
	if err := sc.Scan(&t.ID, &t.UserID, &t.Email, &t.Name, &t.Hint, &created, &expires, &used); err != nil {
		return APIToken{}, err
	}
	t.CreatedAt = parseTime(created)
	if expires != "" {
		e := parseTime(expires)
		t.ExpiresAt = &e
	}
	if used != "" {
		u := parseTime(used)
		t.LastUsedAt = &u
	}
	return t, nil
}

func (s *Store) apiToken(where string, arg any) (APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRow(`SELECT `+apiTokenCols+` FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	return t, err
}

// ListAPITokens returns every token, newest first.
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT ` + apiTokenCols + ` FROM api_tokens t JOIN users u ON u.id = t.user_id ORDER BY t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// revokeAPITokens deletes every token of a user. It runs inside the writes
// that end the user's sessions (password change and reset, 2FA off,
// disabling, demotion); deleting the user cascades.
func revokeAPITokens(db interface {
	Exec(string, ...any) (sql.Result, error)
}, userID int64) error {
	if _, err := db.Exec(`DELETE FROM api_tokens WHERE user_id = ?`, userID); err != nil {
		return err
	}
	// An assistant's grant (api SPEC §16.5) is a token too: what ends the
	// account's API tokens ends its grants.
	_, err := db.Exec(`DELETE FROM mcp_grants WHERE user_id = ?`, userID)
	return err
}

// DeleteAPIToken revokes a token and returns what it was.
func (s *Store) DeleteAPIToken(id int64) (APIToken, error) {
	t, err := s.apiToken(`t.id = ?`, id)
	if err != nil {
		return APIToken{}, err
	}
	if _, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id); err != nil {
		return APIToken{}, err
	}
	return t, nil
}

// UserForAPIToken resolves a token's secret to its token and owner. It
// refuses an expired token, a disabled owner and an owner who is no longer
// an admin (tokens are admin credentials), and records the use at most once
// a minute.
func (s *Store) UserForAPIToken(secret string) (User, APIToken, error) {
	t, err := s.apiToken(`t.token_hash = ?`, hashToken(secret))
	if err != nil {
		return User{}, APIToken{}, err
	}
	nowT := time.Now().UTC()
	if t.ExpiresAt != nil && !t.ExpiresAt.After(nowT) {
		return User{}, APIToken{}, ErrNotFound
	}
	u, err := s.GetUserByEmail(t.Email)
	if err != nil {
		return User{}, APIToken{}, err
	}
	if u.Disabled || u.Role != "admin" {
		return User{}, APIToken{}, ErrNotFound
	}
	if t.LastUsedAt == nil || nowT.Sub(*t.LastUsedAt) > time.Minute {
		_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now(), t.ID)
	}
	return u, t, nil
}
