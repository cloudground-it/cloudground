package state

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Passkeys and recovery codes of panel accounts (#156; SPEC.md, Accounts;
// the rules are internal/api/SPEC.md §2.10–2.11). Nothing here is secret: a
// passkey's public key opens nothing, and a recovery code is stored as its
// hash. So none of it needs the secret key, and host recovery can clear it.

var (
	// ErrPasskeyTaken refuses a credential id another passkey already has.
	ErrPasskeyTaken = errors.New("this passkey is already registered")
	// ErrTooManyPasskeys refuses a passkey past MaxPasskeysPerUser.
	ErrTooManyPasskeys = errors.New("an account can have at most 20 passkeys")
	// ErrLastPasskey refuses removing the last passkey of an account whose
	// password does not sign in: it would have no way in.
	ErrLastPasskey = errors.New("this is your last passkey and password sign-in is off: turn password sign-in on first")
	// ErrNoPasskey refuses turning password sign-in off for an account
	// without a passkey.
	ErrNoPasskey = errors.New("add a passkey before turning password sign-in off")
)

// MaxPasskeysPerUser is how many passkeys one account may have.
const MaxPasskeysPerUser = 20

// webAuthnIDLen is the size of an account's WebAuthn user handle.
const webAuthnIDLen = 32

// Passkey is one WebAuthn credential of an account.
type Passkey struct {
	ID             int64
	UserID         int64
	CredentialID   []byte
	PublicKey      []byte // COSE
	SignCount      uint32
	AAGUID         []byte
	Transports     []string
	BackupEligible bool
	BackupState    bool
	// RPID is the relying party (the panel's host name) it was made for.
	RPID      string
	Name      string
	CreatedAt time.Time
	// LastUsedAt is the zero time before the first use.
	LastUsedAt time.Time
}

const passkeyCols = `id, user_id, credential_id, public_key, sign_count, aaguid, transports, backup_eligible, backup_state, rp_id, name, created_at, last_used_at`

func scanPasskey(row interface{ Scan(...any) error }) (Passkey, error) {
	var p Passkey
	var transports, created, used string
	var count int64
	if err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.PublicKey, &count, &p.AAGUID, &transports,
		&p.BackupEligible, &p.BackupState, &p.RPID, &p.Name, &created, &used); err != nil {
		return Passkey{}, err
	}
	p.SignCount = uint32(count)
	if transports != "" {
		p.Transports = strings.Split(transports, ",")
	}
	p.CreatedAt, p.LastUsedAt = parseTime(created), parseTime(used)
	return p, nil
}

// WebAuthnID returns the account's WebAuthn user handle, making it (32
// random bytes, kept from then on) when the account has none.
func (s *Store) WebAuthnID(userID int64) ([]byte, error) {
	var id []byte
	read := func() error {
		err := s.db.QueryRow(`SELECT webauthn_id FROM users WHERE id = ?`, userID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := read(); err != nil || len(id) > 0 {
		return id, err
	}
	fresh := make([]byte, webAuthnIDLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	// Only where there is none: of two first uses at once, one handle wins
	// and both read it back.
	if _, err := s.db.Exec(`UPDATE users SET webauthn_id = ? WHERE id = ? AND webauthn_id IS NULL`, fresh, userID); err != nil {
		return nil, err
	}
	if err := read(); err != nil {
		return nil, err
	}
	return id, nil
}

// UserByWebAuthnID returns the account a user handle names.
func (s *Store) UserByWebAuthnID(handle []byte) (User, error) {
	if len(handle) == 0 {
		return User{}, ErrNotFound
	}
	return s.userWhere(`u.webauthn_id = ?`, handle)
}

// AddPasskey stores a passkey for p.UserID: ErrPasskeyTaken for a credential
// id already stored, ErrTooManyPasskeys past MaxPasskeysPerUser.
func (s *Store) AddPasskey(p Passkey) (Passkey, error) {
	at := now()
	err := s.inTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM user_passkeys WHERE user_id = ?`, p.UserID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPasskeysPerUser {
			return ErrTooManyPasskeys
		}
		res, err := tx.Exec(`INSERT INTO user_passkeys (user_id, credential_id, public_key, sign_count, aaguid, transports,
			backup_eligible, backup_state, rp_id, name, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.UserID, p.CredentialID, p.PublicKey, int64(p.SignCount), p.AAGUID, strings.Join(p.Transports, ","),
			p.BackupEligible, p.BackupState, p.RPID, p.Name, at)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrPasskeyTaken
			}
			return err
		}
		p.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Passkey{}, err
	}
	p.CreatedAt = parseTime(at)
	s.publishUser(p.UserID)
	return p, nil
}

// Passkeys returns the account's passkeys, oldest first.
func (s *Store) Passkeys(userID int64) ([]Passkey, error) {
	rows, err := s.db.Query(`SELECT `+passkeyCols+` FROM user_passkeys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// PasskeyByCredentialID returns the passkey with a credential id.
func (s *Store) PasskeyByCredentialID(credentialID []byte) (Passkey, error) {
	p, err := scanPasskey(s.db.QueryRow(`SELECT `+passkeyCols+` FROM user_passkeys WHERE credential_id = ?`, credentialID))
	if errors.Is(err, sql.ErrNoRows) {
		return Passkey{}, ErrNotFound
	}
	return p, err
}

// RecordPasskeyUse records a passkey's use: its signature counter and
// backup state as the authenticator last reported them.
func (s *Store) RecordPasskeyUse(id int64, counter uint32, backupState bool) error {
	_, err := s.db.Exec(`UPDATE user_passkeys SET sign_count = ?, backup_state = ?, last_used_at = ? WHERE id = ?`,
		int64(counter), backupState, now(), id)
	return err
}

// RemovePasskey removes one of the account's passkeys and returns it, with
// whether it was the account's last second factor; then its recovery codes
// and API tokens go too, in the same transaction. ErrNotFound for a passkey
// of another account, ErrLastPasskey for the last one while password
// sign-in is off.
func (s *Store) RemovePasskey(userID, id int64) (Passkey, bool, error) {
	var p Passkey
	var last bool
	err := s.inTx(func(tx *sql.Tx) error {
		var err error
		p, err = scanPasskey(tx.QueryRow(`SELECT `+passkeyCols+` FROM user_passkeys WHERE id = ? AND user_id = ?`, id, userID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var count int
		var password, totp bool
		if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM user_passkeys WHERE user_id = u.id), u.password_signin, u.totp_enabled
			FROM users u WHERE u.id = ?`, userID).Scan(&count, &password, &totp); err != nil {
			return err
		}
		if count == 1 && !password {
			return ErrLastPasskey
		}
		if _, err := tx.Exec(`DELETE FROM user_passkeys WHERE id = ?`, id); err != nil {
			return err
		}
		last = count == 1 && !totp
		if !last {
			return nil
		}
		return errors.Join(dropRecoveryCodes(tx, userID), revokeAPITokens(tx, userID))
	})
	if err != nil {
		return Passkey{}, false, err
	}
	s.publishUser(userID)
	return p, last, nil
}

// ReplaceRecoveryCodes makes hashes (hex SHA-256) the account's recovery
// codes, in place of every earlier one, in one transaction.
func (s *Store) ReplaceRecoveryCodes(userID int64, hashes []string) error {
	err := s.inTx(func(tx *sql.Tx) error {
		if err := dropRecoveryCodes(tx, userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.Exec(`INSERT INTO user_recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.publish(events.UserChanged, 0, map[string]any{"user_id": userID})
	}
	return err
}

// SpendRecoveryCode spends the account's unused code with this hash, and
// reports whether there was one. One update of a row still unused: of two
// requests with one code, one spends it.
func (s *Store) SpendRecoveryCode(userID int64, hash string) (bool, error) {
	res, err := s.db.Exec(`UPDATE user_recovery_codes SET used_at = ? WHERE id =
		(SELECT id FROM user_recovery_codes WHERE user_id = ? AND code_hash = ? AND used_at IS NULL LIMIT 1)
		AND used_at IS NULL`, now(), userID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func dropRecoveryCodes(tx execer, userID int64) error {
	_, err := tx.Exec(`DELETE FROM user_recovery_codes WHERE user_id = ?`, userID)
	return err
}

// SetPasswordSignIn turns the account's password sign-in on or off. Off
// refuses an account without a passkey (ErrNoPasskey), in the transaction
// that writes it.
func (s *Store) SetPasswordSignIn(userID int64, on bool) error {
	err := s.inTx(func(tx *sql.Tx) error {
		if !on {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM user_passkeys WHERE user_id = ?`, userID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrNoPasskey
			}
		}
		res, err := tx.Exec(`UPDATE users SET password_signin = ? WHERE id = ?`, on, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(ErrNotFound, err)
		}
		return nil
	})
	if err == nil {
		s.publishUser(userID)
	}
	return err
}

// ResetSecondFactors removes every second factor of the account (TOTP and
// its secrets, passkeys, recovery codes), turns its password sign-in on and
// ends its sessions and API tokens, in one transaction. It needs no key:
// host recovery runs it.
func (s *Store) ResetSecondFactors(userID int64) error {
	err := s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET totp_enabled = 0, password_signin = 1 WHERE id = ?`, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(ErrNotFound, err)
		}
		if _, err := tx.Exec(`DELETE FROM user_passkeys WHERE user_id = ?`, userID); err != nil {
			return err
		}
		return errors.Join(dropTOTP(tx, userID), dropRecoveryCodes(tx, userID), endSessions(tx, userID), revokeAPITokens(tx, userID))
	})
	if err == nil {
		s.publishUser(userID)
	}
	return err
}
