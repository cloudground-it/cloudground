package state

import (
	"context"
	"database/sql"
	"regexp"
)

// A backup repository's URL can carry a password (rest:https://user:pw@host/),
// and it is kept in panel_settings and in every backups row. The password is
// sealed like the other secrets (SPEC.md "Secrets"): the column holds the URL
// with the password replaced by sealedRepoPassword, and the password is the
// secret repoSecretName(that form). Reads put it back, so callers see the URL
// they stored; a store opened without a key sees the sealed form.

// sealedRepoPassword stands for a sealed password in a stored URL.
const sealedRepoPassword = "{sealed}"

// repoPasswordRe finds the password of a URL's userinfo.
var repoPasswordRe = regexp.MustCompile(`(://[^/@:\s]*:)([^/@\s]+)@`)

// splitRepo returns repo with its password replaced by sealedRepoPassword,
// and the password ("" when it has none, or holds only the placeholder).
func splitRepo(repo string) (stored, password string) {
	m := repoPasswordRe.FindStringSubmatchIndex(repo)
	if m == nil || repo[m[4]:m[5]] == sealedRepoPassword {
		return repo, ""
	}
	return repo[:m[4]] + sealedRepoPassword + repo[m[5]:], repo[m[4]:m[5]]
}

// repoSecretName names the sealed password of the stored URL: two URLs that
// differ only in the password are one repository, with its latest password.
func repoSecretName(stored string) string {
	return "backup_repository:" + stored
}

// storedRepo is how repo is written and looked up: its password, if any,
// sealed into the secrets table through q.
func (s *Store) storedRepo(q execer, repo string) (string, error) {
	stored, password := splitRepo(repo)
	if password == "" {
		return stored, nil
	}
	if err := s.setSecret(q, repoSecretName(stored), password); err != nil {
		return "", err
	}
	return stored, nil
}

// RepoKey is the stored form repo is looked up by, sealing nothing: the URL
// with its password, if any, replaced by a placeholder. It names the
// repository whatever its password.
func RepoKey(repo string) string {
	stored, _ := splitRepo(repo)
	return stored
}

// openRepo is the URL a stored one stands for, its password unsealed.
// Without a key, or with the secret gone, it is the stored form: restic then
// fails to authenticate, which says what is missing.
func (s *Store) openRepo(stored string) (string, error) {
	return s.openRepoFrom(s.db, stored)
}

// openRepoFrom is openRepo reading the secret through q.
func (s *Store) openRepoFrom(q rowQuerier, stored string) (string, error) {
	m := repoPasswordRe.FindStringSubmatchIndex(stored)
	if m == nil || stored[m[4]:m[5]] != sealedRepoPassword || s.key == nil {
		return stored, nil
	}
	password, err := s.secretFrom(q, repoSecretName(stored))
	if err != nil || password == "" {
		return stored, err
	}
	return stored[:m[4]] + password + stored[m[5]:], nil
}

// sealRepoCredentials seals the repository passwords still in plaintext in
// panel_settings and backups (written before they were sealed). It reports
// whether it changed anything, so the file is compacted.
func (s *Store) sealRepoCredentials() (moved bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var repos []string
	// The settings last: where an old record and the setting name one
	// repository with two passwords, the setting's is the current one.
	for _, q := range []string{
		`SELECT DISTINCT repository FROM backups WHERE repository LIKE '%@%'`,
		`SELECT backup_repository FROM panel_settings WHERE backup_repository LIKE '%@%'`,
	} {
		rows, err := tx.Query(q)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return false, err
			}
			repos = append(repos, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
	}
	for _, repo := range repos {
		stored, err := s.storedRepo(tx, repo)
		if err != nil {
			return false, err
		}
		if stored == repo {
			continue
		}
		for _, q := range []string{
			`UPDATE panel_settings SET backup_repository=? WHERE backup_repository=?`,
			`UPDATE backups SET repository=? WHERE repository=?`,
		} {
			if _, err := tx.Exec(q, stored, repo); err != nil {
				return false, err
			}
		}
		moved = true
	}
	if !moved {
		return false, nil
	}
	return true, tx.Commit()
}

// connExec runs a statement on a dedicated connection (writeTx's).
type connExec struct {
	conn *sql.Conn
	ctx  context.Context
}

func (c connExec) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(c.ctx, query, args...)
}

func (c connExec) QueryRow(query string, args ...any) *sql.Row {
	return c.conn.QueryRowContext(c.ctx, query, args...)
}
