package state

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Two-factor state (SPEC.md, Accounts). The users table keeps only whether
// TOTP is on and the replay watermark; the secrets are sealed in the secret
// store, under the names below.

const (
	totpActive  = "totp"
	totpPending = "totp_pending"
)

// UserSecret names one of an account's secrets: user.<id>.<kind>.
func UserSecret(userID int64, kind string) string {
	return userSecretPrefix(userID) + kind
}

func userSecretPrefix(userID int64) string {
	return "user." + strconv.FormatInt(userID, 10) + "."
}

// errNoTOTPSecret is 2FA on with no secret to check codes against.
var errNoTOTPSecret = errors.New("two-factor authentication is on but its secret is missing")

// SetPendingTOTP seals a secret being enrolled, beside the active one.
func (s *Store) SetPendingTOTP(userID int64, secret string) error {
	if secret == "" {
		return errors.New("empty TOTP secret")
	}
	return s.SetSecret(UserSecret(userID, totpPending), secret)
}

// PendingTOTP returns the secret being enrolled, "" when there is none.
func (s *Store) PendingTOTP(userID int64) (string, error) {
	return s.Secret(UserSecret(userID, totpPending))
}

// ErrPendingChanged refuses an activation whose pending secret is no
// longer the one the caller checked a code against.
var ErrPendingChanged = errors.New("the pending TOTP secret changed")

// ActivatePendingTOTP makes the pending secret the active one and turns 2FA
// on with a fresh replay watermark, if the pending secret is still checked:
// the one the caller verified a code against. A second enrolment begun in
// between replaced it, and switching that one on would enable a secret no
// code was ever checked against: ErrPendingChanged. ErrNotFound when nothing
// is pending. Read, compare and write are one transaction.
func (s *Store) ActivatePendingTOTP(userID int64, checked string) error {
	return s.ActivatePendingTOTPAt(userID, checked, 0)
}

// ActivatePendingTOTPAt is ActivatePendingTOTP with the watermark set to
// usedStep, the step of the code that confirmed the enrolment, in the same
// transaction: that code is spent, and cannot sign in afterwards.
func (s *Store) ActivatePendingTOTPAt(userID int64, checked string, usedStep uint64) error {
	pendingName := UserSecret(userID, totpPending)
	err := s.inTx(func(tx *sql.Tx) error {
		var sealed []byte
		err := tx.QueryRow(`SELECT sealed FROM secrets WHERE name = ?`, pendingName).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		pending, err := s.open(pendingName, sealed)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(pending), []byte(checked)) != 1 {
			return ErrPendingChanged
		}
		res, err := tx.Exec(`UPDATE users SET totp_enabled = 1, totp_last_step = ? WHERE id = ?`, int64(usedStep), userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(ErrNotFound, err)
		}
		if err := s.setSecret(tx, UserSecret(userID, totpActive), pending); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM secrets WHERE name = ?`, pendingName)
		return err
	})
	if err == nil {
		s.publishUser(userID)
	}
	return err
}

// UserTOTP returns the active secret and whether 2FA is on. On with no
// secret that opens is an error, never "off": a second factor that cannot be
// checked must not be skipped. The secret is "" when 2FA is off.
func (s *Store) UserTOTP(userID int64) (secret string, enabled bool, err error) {
	if err := s.db.QueryRow(`SELECT totp_enabled FROM users WHERE id = ?`, userID).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return "", false, err
	}
	if !enabled {
		return "", false, nil
	}
	secret, err = s.Secret(UserSecret(userID, totpActive))
	if err == nil && secret == "" {
		err = errNoTOTPSecret
	}
	if err != nil {
		return "", true, fmt.Errorf("user %d: %w", userID, err)
	}
	return secret, true, nil
}

// DisableUserTOTP turns 2FA off and deletes both secrets, and the account's
// API tokens: what was issued under the second factor goes with it. It
// needs no key, so host recovery can run it.
func (s *Store) DisableUserTOTP(userID int64) error {
	err := s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE users SET totp_enabled = 0 WHERE id = ?`, userID); err != nil {
			return err
		}
		return errors.Join(dropTOTP(tx, userID), revokeAPITokens(tx, userID))
	})
	if err == nil {
		s.publishUser(userID)
	}
	return err
}

// dropTOTP deletes an account's secrets, and any it still has waiting in
// migration 16's holding table, so a later Open does not seal them again.
func dropTOTP(tx *sql.Tx, userID int64) error {
	if err := deleteSecretsWithPrefix(tx, userSecretPrefix(userID)); err != nil {
		return err
	}
	held, err := hasTable(tx, totpImportTable)
	if err != nil || !held {
		return err
	}
	_, err = tx.Exec(`DELETE FROM `+totpImportTable+` WHERE user_id = ?`, userID)
	return err
}

// MarkTOTPStepUsed records an accepted time step, and reports whether it
// was later than every step accepted before. The watermark only moves
// forward, so two sign-ins with one code cannot both pass.
func (s *Store) MarkTOTPStepUsed(userID int64, step uint64) (bool, error) {
	res, err := s.db.Exec(`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, int64(step), userID, int64(step))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// totpImportTable holds the plaintext secrets migration 16 took out of
// users until an Open with the key seals them.
const totpImportTable = "totp_import"

func hasTable(q interface {
	QueryRow(string, ...any) *sql.Row
}, name string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

// moveLegacyTOTP seals the secrets migration 16 held back and deletes them
// from its holding table, in one transaction; a sealed value already under
// the name wins. It reports whether the holding table exists, so Open
// compacts the file and only then drops it (dropTOTPImport): a crash between
// the move and the compaction leaves the table, and the next Open compacts.
func (s *Store) moveLegacyTOTP() (held bool, err error) {
	held, err = hasTable(s.db, totpImportTable)
	if err != nil || !held {
		return false, err
	}
	err = s.inTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT user_id, slot, secret FROM ` + totpImportTable + ` ORDER BY user_id, slot`)
		if err != nil {
			return err
		}
		type heldSecret struct {
			user         int64
			slot, secret string
		}
		var all []heldSecret
		for rows.Next() {
			var h heldSecret
			if err := rows.Scan(&h.user, &h.slot, &h.secret); err != nil {
				rows.Close()
				return err
			}
			all = append(all, h)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, h := range all {
			kind := totpActive
			if h.slot == "pending" {
				kind = totpPending
			}
			name := UserSecret(h.user, kind)
			var sealed int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name = ?`, name).Scan(&sealed); err != nil {
				return err
			}
			if sealed == 0 && h.secret != "" {
				if err := s.setSecret(tx, name, h.secret); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(`DELETE FROM ` + totpImportTable)
		return err
	})
	return held, err
}

// dropTOTPImport drops migration 16's emptied holding table, after Open has
// compacted the file.
func (s *Store) dropTOTPImport() error {
	_, err := s.db.Exec(`DROP TABLE IF EXISTS ` + totpImportTable)
	return err
}
