package state

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Sessions and setup tokens (SPEC.md, Accounts). Neither token is stored:
// only its SHA-256, so a copy of the database signs nobody in.

// SessionIdleTimeout ends a session left unused this long, whatever its
// absolute expiry: a panel left open on a shared machine should not stay a
// root shell for the rest of the day.
const SessionIdleTimeout = 2 * time.Hour

// sessionTouchEvery bounds how often a session's last use is written.
const sessionTouchEvery = time.Minute

// maxUserAgent is how much of a browser's User-Agent a session keeps.
const maxUserAgent = 400

// MaxSessionsPerUser is how many sessions one account keeps: a new one past
// it ends the oldest, so a script signing in in a loop cannot grow the table
// (and the account's session list) without bound.
const MaxSessionsPerUser = 20

// TokenHash is how a session or API token is keyed in the store; the
// session list shows a prefix of it as the session's id.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string { return TokenHash(token) }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// CreateSession stores a new session of userID that expires after ttl.
// Every sign-in also tidies the table: the sessions of any account that
// have expired or gone idle are deleted (they could no longer be used), and
// past MaxSessionsPerUser the account's oldest sessions are ended.
func (s *Store) CreateSession(token string, userID int64, ttl time.Duration) error {
	if err := s.PruneSessions(); err != nil {
		return err
	}
	at := time.Now()
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, TokenHash(token), userID, stamp(at), stamp(at), stamp(at.Add(ttl))); err != nil {
		return err
	}
	// rowid is the order of insertion; created_at's text would misorder
	// two stamps whose fractions have different lengths.
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND rowid NOT IN
		(SELECT rowid FROM sessions WHERE user_id = ? ORDER BY rowid DESC LIMIT ?)`, userID, userID, MaxSessionsPerUser)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.publish(events.SessionRevoked, 0, map[string]any{"user_id": userID})
	}
	return nil
}

// sessionDead is the one rule for a session that can no longer be used: past
// its expiry, unused for over SessionIdleTimeout, or with a time that does
// not parse (failing open would keep it alive for ever). seen is "" for a
// session stored before last uses were recorded.
func sessionDead(at time.Time, expires, seen string) bool {
	end, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !at.Before(end) {
		return true
	}
	if seen == "" {
		return false
	}
	lastUse, err := time.Parse(time.RFC3339Nano, seen)
	return err != nil || at.Sub(lastUse) > SessionIdleTimeout
}

// PruneSessions deletes every session that has expired or gone idle. Such a
// session is refused when presented anyway; this keeps the table to the
// sessions that still work.
func (s *Store) PruneSessions() error {
	rows, err := s.db.Query(`SELECT token_hash, expires_at, last_seen_at FROM sessions`)
	if err != nil {
		return err
	}
	at := time.Now()
	var dead []string
	for rows.Next() {
		var hash, expires, seen string
		if err := rows.Scan(&hash, &expires, &seen); err != nil {
			rows.Close()
			return err
		}
		if sessionDead(at, expires, seen) {
			dead = append(dead, hash)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, hash := range dead {
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hash); err != nil {
			return err
		}
	}
	return nil
}

// SetSessionClient records the browser and the address a session signed in
// from.
func (s *Store) SetSessionClient(token, userAgent, ip string) error {
	if len(userAgent) > maxUserAgent {
		userAgent = userAgent[:maxUserAgent]
	}
	_, err := s.db.Exec(`UPDATE sessions SET user_agent = ?, ip = ? WHERE token_hash = ?`, userAgent, ip, TokenHash(token))
	return err
}

// UserForSession returns the account of a live session and records the use.
func (s *Store) UserForSession(token string) (User, error) {
	return s.resolveSession(token, true)
}

// PeekSessionUser is UserForSession without recording a use, for the event
// stream: it re-checks its session every few seconds, and counting that as
// use would keep an abandoned tab signed in for good.
func (s *Store) PeekSessionUser(token string) (User, error) {
	return s.resolveSession(token, false)
}

// resolveSession finds the session's account. A session that has expired,
// has been idle too long or belongs to a disabled account is deleted and
// reported as ErrNotFound.
func (s *Store) resolveSession(token string, touch bool) (User, error) {
	key := TokenHash(token)
	var expires, seen string
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+`, se.expires_at, se.last_seen_at
		FROM sessions se JOIN users u ON u.id = se.user_id WHERE se.token_hash = ?`, key), &expires, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	at := time.Now()
	if u.Disabled || sessionDead(at, expires, seen) {
		_ = s.DeleteSessionByHash(key)
		return User{}, ErrNotFound
	}
	lastUse, _ := time.Parse(time.RFC3339Nano, seen)
	if touch && (seen == "" || at.Sub(lastUse) > sessionTouchEvery) {
		_, _ = s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, stamp(at), key)
	}
	return u, nil
}

// DeleteSession ends the session of a token.
func (s *Store) DeleteSession(token string) error {
	return s.DeleteSessionByHash(TokenHash(token))
}

// DeleteSessionByHash ends the session stored under hash; one that does not
// exist is not an error.
func (s *Store) DeleteSessionByHash(hash string) error {
	var owner int64
	err := s.db.QueryRow(`DELETE FROM sessions WHERE token_hash = ? RETURNING user_id`, hash).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	s.publish(events.SessionRevoked, 0, map[string]any{"user_id": owner})
	return nil
}

// RevokeOtherSessions ends every session of userID but the one of
// keepToken: a change to how the account signs in must not leave a stolen
// session open.
func (s *Store) RevokeOtherSessions(userID int64, keepToken string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, TokenHash(keepToken)); err != nil {
		return err
	}
	s.publish(events.SessionRevoked, 0, map[string]any{"user_id": userID})
	return nil
}

// ListSessionsForUser returns the account's sessions that still work
// (neither expired nor idle past SessionIdleTimeout), newest first.
func (s *Store) ListSessionsForUser(userID int64) ([]Session, error) {
	rows, err := s.db.Query(`SELECT token_hash, created_at, expires_at, last_seen_at, user_agent, ip
		FROM sessions WHERE user_id = ? ORDER BY rowid DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := time.Now()
	var list []Session
	for rows.Next() {
		var se Session
		var created, expires, seen string
		if err := rows.Scan(&se.TokenHash, &created, &expires, &seen, &se.UserAgent, &se.IP); err != nil {
			return nil, err
		}
		if sessionDead(at, expires, seen) {
			continue
		}
		se.CreatedAt, se.ExpiresAt, se.LastSeen = parseTime(created), parseTime(expires), parseTime(seen)
		list = append(list, se)
	}
	return list, rows.Err()
}

// endSessions deletes every session of a user, inside tx.
func endSessions(tx execer, userID int64) error {
	_, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// CreateSetupToken stores the installer's one-time setup token. It is the
// only valid one from then on: every token minted before it is deleted in
// the same transaction, so a setup link printed by an earlier start (and
// left in a journal or a terminal's scrollback) stops working once a
// restart prints a new one.
func (s *Store) CreateSetupToken(token string, ttl time.Duration) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM setup_tokens`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO setup_tokens (token_hash, expires_at, used) VALUES (?, ?, 0)`,
			TokenHash(token), stamp(time.Now().Add(ttl)))
		return err
	})
}

// ConsumeSetupToken spends a setup token in one write, so two requests with
// it cannot both succeed. An unknown, expired or spent token is ErrNotFound.
func (s *Store) ConsumeSetupToken(token string) error {
	res, err := s.db.Exec(`UPDATE setup_tokens SET used = 1 WHERE token_hash = ? AND used = 0 AND expires_at > ?`,
		TokenHash(token), stamp(time.Now()))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return ErrNotFound
	}
	return nil
}
