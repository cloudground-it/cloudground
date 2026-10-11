package state

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Panel accounts (SPEC.md, Accounts; the rules are internal/api/SPEC.md §2).

var (
	// ErrLastAdmin refuses a change that would leave no enabled admin.
	ErrLastAdmin = errors.New("the panel needs at least one enabled administrator")
	// ErrEmailTaken refuses an account whose e-mail another one has.
	ErrEmailTaken = errors.New("a user with this email already exists")
	// ErrBadSite refuses a site assignment naming no site.
	ErrBadSite = errors.New("invalid site assignment")
)

// Locales are the interface languages an account can choose; the first is
// the panel's default.
var Locales = []string{"it", "en", "fr", "es", "de"}

// ValidLocale reports whether l is one of Locales.
func ValidLocale(l string) bool { return slices.Contains(Locales, l) }

// userCols are the columns scanUser reads, from users aliased u.
const userCols = `u.id, u.email, u.password_hash, u.role, u.first_name, u.last_name, u.locale, u.disabled, u.totp_enabled, u.created_at`

// scanUser reads userCols, then whatever extra columns the query adds.
func scanUser(row interface{ Scan(...any) error }, extra ...any) (User, error) {
	var u User
	var created string
	dest := append([]any{&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.FirstName, &u.LastName, &u.Locale,
		&u.Disabled, &u.TOTPEnabled, &created}, extra...)
	if err := row.Scan(dest...); err != nil {
		return User{}, err
	}
	u.CreatedAt = parseTime(created)
	return u, nil
}

func (s *Store) userWhere(cond string, arg any) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users u WHERE `+cond, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// GetUser returns an account by id, with its assigned sites.
func (s *Store) GetUser(id int64) (User, error) {
	u, err := s.userWhere(`u.id = ?`, id)
	if err != nil {
		return User{}, err
	}
	assigned, err := s.assignments(`WHERE user_id = ?`, id)
	u.SiteIDs = assigned[id]
	return u, err
}

// GetUserByEmail returns the account signing in with email.
func (s *Store) GetUserByEmail(email string) (User, error) {
	return s.userWhere(`u.email = ?`, email)
}

// CountUsers counts accounts; none means setup has not run.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// ListUsers returns every account by id, operators with their sites.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users u ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	var list []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, u)
	}
	// The store has one connection: this result set must be closed before
	// the next query can run.
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	assigned, err := s.assignments(``)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].SiteIDs = assigned[list[i].ID]
	}
	return list, nil
}

// assignments maps user ids to their sites, in site order.
func (s *Store) assignments(where string, args ...any) (map[int64][]int64, error) {
	rows, err := s.db.Query(`SELECT user_id, site_id FROM user_sites `+where+` ORDER BY user_id, site_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var user, site int64
		if err := rows.Scan(&user, &site); err != nil {
			return nil, err
		}
		out[user] = append(out[user], site)
	}
	return out, rows.Err()
}

// NewUser is an account to create.
type NewUser struct {
	Email, PasswordHash, Role string
	FirstName, LastName       string
	SiteIDs                   []int64
}

// CreateUser creates an account with no name and no sites.
func (s *Store) CreateUser(email, passwordHash, role string) (User, error) {
	return s.CreateAccount(NewUser{Email: email, PasswordHash: passwordHash, Role: role})
}

// CreateAccount creates an account, its name and its sites in one
// transaction: ErrEmailTaken or ErrBadSite leave nothing behind.
func (s *Store) CreateAccount(nu NewUser) (User, error) {
	at := now()
	var id int64
	err := s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO users (email, password_hash, role, first_name, last_name, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			nu.Email, nu.PasswordHash, nu.Role, nu.FirstName, nu.LastName, at)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrEmailTaken
			}
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return assignSites(tx, id, nu.SiteIDs)
	})
	if err != nil {
		return User{}, err
	}
	s.publishUser(id)
	return User{ID: id, Email: nu.Email, PasswordHash: nu.PasswordHash, Role: nu.Role,
		FirstName: nu.FirstName, LastName: nu.LastName, SiteIDs: nu.SiteIDs, CreatedAt: parseTime(at)}, nil
}

// UserUpdate is the whole of what an admin edits on an account.
type UserUpdate struct {
	Role                string
	Disabled            bool
	FirstName, LastName string
	SiteIDs             []int64
}

// UpdateUser applies an admin's edit in one transaction: ErrLastAdmin,
// ErrNotFound and ErrBadSite change nothing. Disabling ends the account's
// sessions; disabling or demoting it revokes its API tokens.
func (s *Store) UpdateUser(id int64, up UserUpdate) error {
	err := s.inTx(func(tx *sql.Tx) error {
		if err := setAccess(tx, id, up.Role, up.Disabled); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET first_name = ?, last_name = ? WHERE id = ?`, up.FirstName, up.LastName, id); err != nil {
			return err
		}
		return assignSites(tx, id, up.SiteIDs)
	})
	if err == nil {
		s.publishUser(id)
	}
	return err
}

// UpdateUserAccess sets an account's role and disabled flag (UpdateUser's
// rules, names and sites left alone).
func (s *Store) UpdateUserAccess(id int64, role string, disabled bool) error {
	err := s.inTx(func(tx *sql.Tx) error { return setAccess(tx, id, role, disabled) })
	if err == nil {
		s.publishUser(id)
	}
	return err
}

func setAccess(tx *sql.Tx, id int64, role string, disabled bool) error {
	if last, err := wouldLeaveNoAdmin(tx, id, role, disabled, false); err != nil {
		return err
	} else if last {
		return ErrLastAdmin
	}
	flag := 0
	if disabled {
		flag = 1
	}
	res, err := tx.Exec(`UPDATE users SET role = ?, disabled = ? WHERE id = ?`, role, flag, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(ErrNotFound, err)
	}
	if disabled {
		if err := endSessions(tx, id); err != nil {
			return err
		}
	}
	// Tokens are an admin's: refused per request once the account is not
	// one, and deleted here so promoting or enabling it again does not bring
	// them back.
	if disabled || role != "admin" {
		return revokeAPITokens(tx, id)
	}
	return nil
}

// SetUserSites replaces an account's site assignments.
func (s *Store) SetUserSites(userID int64, siteIDs []int64) error {
	err := s.inTx(func(tx *sql.Tx) error { return assignSites(tx, userID, siteIDs) })
	if err == nil {
		s.publishUser(userID)
	}
	return err
}

func assignSites(tx *sql.Tx, userID int64, siteIDs []int64) error {
	if _, err := tx.Exec(`DELETE FROM user_sites WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, site := range siteIDs {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sites WHERE id = ?`, site).Scan(&n); err != nil {
			return err
		}
		if site <= 0 || n == 0 {
			return ErrBadSite
		}
		// A site named twice is refused, as one that does not exist is.
		if _, err := tx.Exec(`INSERT INTO user_sites (user_id, site_id) VALUES (?, ?)`, userID, site); err != nil {
			return ErrBadSite
		}
	}
	return nil
}

// UserCanAccessSite reports whether an operator is assigned the site, or
// the site whose staging copy it is.
func (s *Store) UserCanAccessSite(userID, siteID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_sites WHERE user_id = ?
		AND site_id IN (?, (SELECT staging_of FROM sites WHERE id = ?))`, userID, siteID, siteID).Scan(&n)
	return n > 0, err
}

// UpdateUserProfile sets an account's first and last name.
func (s *Store) UpdateUserProfile(id int64, first, last string) error {
	if _, err := s.db.Exec(`UPDATE users SET first_name = ?, last_name = ? WHERE id = ?`, first, last, id); err != nil {
		return err
	}
	s.publish(events.UserChanged, 0, map[string]any{"user_id": id})
	return nil
}

// UpdateUserLocale sets an account's interface language, one of Locales.
func (s *Store) UpdateUserLocale(id int64, locale string) error {
	if !ValidLocale(locale) {
		return fmt.Errorf("unsupported locale %q", locale)
	}
	_, err := s.db.Exec(`UPDATE users SET locale = ? WHERE id = ?`, locale, id)
	return err
}

// SetUserPassword replaces an account's password hash, and ends its
// sessions and API tokens: whoever had the old password keeps nothing.
func (s *Store) SetUserPassword(id int64, hash string) error {
	err := s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(ErrNotFound, err)
		}
		return errors.Join(endSessions(tx, id), revokeAPITokens(tx, id))
	})
	if err == nil {
		s.publishUser(id)
	}
	return err
}

// UpdatePassword is SetUserPassword, for the account's own change.
func (s *Store) UpdatePassword(userID int64, hash string) error {
	return s.SetUserPassword(userID, hash)
}

// DeleteUser removes an account with its sessions, sites, API tokens and
// two-factor secrets, unless it is the last enabled admin (ErrLastAdmin).
func (s *Store) DeleteUser(id int64) error {
	err := s.inTx(func(tx *sql.Tx) error {
		if last, err := wouldLeaveNoAdmin(tx, id, "", false, true); err != nil {
			return err
		} else if last {
			return ErrLastAdmin
		}
		if err := dropTOTP(tx, id); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(ErrNotFound, err)
		}
		return nil
	})
	if err == nil {
		s.publishUser(id)
	}
	return err
}

// enabledAdmin is the one definition of an admin for the "at least one
// enabled admin" rule.
const enabledAdmin = `role = 'admin' AND disabled = 0`

// CountActiveAdmins counts enabled admins.
func (s *Store) CountActiveAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE ` + enabledAdmin).Scan(&n)
	return n, err
}

// wouldLeaveNoAdmin decides the admin rule for every change to an account:
// whether giving account id this role and disabled flag (or deleting it)
// removes the last enabled admin. Run it in the change's transaction.
func wouldLeaveNoAdmin(tx *sql.Tx, id int64, role string, disabled, deleting bool) (bool, error) {
	stillAdmin := !deleting && role == "admin" && !disabled
	var isAdmin, others int
	err := tx.QueryRow(`SELECT
		(SELECT COUNT(*) FROM users WHERE id = ? AND `+enabledAdmin+`),
		(SELECT COUNT(*) FROM users WHERE id <> ? AND `+enabledAdmin+`)`, id, id).Scan(&isAdmin, &others)
	if err != nil {
		return false, err
	}
	return isAdmin > 0 && !stillAdmin && others == 0, nil
}

// inTx runs fn in a transaction, committed when fn returns nil.
func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// publishUser announces a change to an account. Its open event streams
// re-check their session (session.revoked): the change may be a role, a
// disabled flag or a set of sites.
func (s *Store) publishUser(id int64) {
	s.publish(events.UserChanged, 0, map[string]any{"user_id": id})
	s.publish(events.SessionRevoked, 0, map[string]any{"user_id": id})
}
