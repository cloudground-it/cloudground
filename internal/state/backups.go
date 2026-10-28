package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Backup records (SPEC.md "Backups"). A row is one restic snapshot of a
// site, as the panel knows it: in which repository, how big, and what its
// restore test found. It is created pending; only a restore test moves it
// to passed or failed.

const backupCols = `id, site_id, snapshot_id, repository, size_bytes, kind, verify_status, verified_at, restore_est_ms, created_at, databases`

func scanBackupRow(row scanner) (Backup, error) {
	var b Backup
	var verify, created string
	var verified, databases sql.NullString
	if err := row.Scan(&b.ID, &b.SiteID, &b.SnapshotID, &b.Repository, &b.SizeBytes, &b.Kind,
		&verify, &verified, &b.RestoreEstMS, &created, &databases); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Backup{}, ErrNotFound
		}
		return Backup{}, err
	}
	b.VerifyStatus, b.VerifiedAt, b.CreatedAt = BackupVerify(verify), parseTimePtr(verified), parseTime(created)
	b.Databases = splitDatabases(databases)
	return b, nil
}

func (s *Store) queryBackups(q string, args ...any) ([]Backup, error) {
	rows, err := s.db.Query(`SELECT `+backupCols+` FROM backups `+q, args...)
	if err != nil {
		return nil, err
	}
	var out []Backup
	for rows.Next() {
		b, err := scanBackupRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The repositories' passwords are opened once the rows are read: the
	// store has one connection, which the open rows hold.
	for i := range out {
		if out[i], err = s.openBackup(out[i], nil); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []Backup{}
	}
	return out, nil
}

// openBackup is b with its repository's password unsealed.
func (s *Store) openBackup(b Backup, err error) (Backup, error) {
	if err != nil {
		return b, err
	}
	b.Repository, err = s.openRepo(b.Repository)
	return b, err
}

// CreateBackup records a snapshot just taken. Whatever b says, it starts
// pending: a snapshot counts only once its restore test passed.
func (s *Store) CreateBackup(b Backup) (Backup, error) {
	ts := now()
	repo, err := s.storedRepo(s.db, b.Repository)
	if err != nil {
		return Backup{}, err
	}
	var databases any // NULL: not known
	if b.Databases != nil {
		databases = strings.Join(b.Databases, ",")
	}
	res, err := s.db.Exec(`INSERT INTO backups (site_id, snapshot_id, repository, size_bytes, kind, verify_status, restore_est_ms, created_at, databases)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.SiteID, b.SnapshotID, repo, b.SizeBytes, b.Kind, string(VerifyPending), b.RestoreEstMS, ts, databases)
	if err != nil {
		return Backup{}, err
	}
	b.ID, _ = res.LastInsertId()
	b.VerifyStatus, b.VerifiedAt, b.CreatedAt = VerifyPending, nil, parseTime(ts)
	s.publish(events.BackupFinished, b.SiteID, map[string]any{"id": b.ID, "kind": b.Kind})
	return b, nil
}

// SetBackupVerified records a restore test's verdict and the restore time
// it measured. A record removed meanwhile is not an error.
func (s *Store) SetBackupVerified(id int64, status BackupVerify, restoreMS int64) error {
	var siteID int64
	err := s.db.QueryRow(`UPDATE backups SET verify_status=?, verified_at=?, restore_est_ms=? WHERE id=? RETURNING site_id`,
		string(status), now(), restoreMS, id).Scan(&siteID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	s.publish(events.BackupVerified, siteID, map[string]any{"id": id, "status": status})
	return nil
}

// GetBackup reads one record.
func (s *Store) GetBackup(id int64) (Backup, error) {
	return s.openBackup(scanBackupRow(s.db.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id=?`, id)))
}

// ListBackups returns a site's records, newest first (at most limit; 50
// when limit is not positive).
func (s *Store) ListBackups(siteID int64, limit int) ([]Backup, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryBackups(`WHERE site_id=? ORDER BY id DESC LIMIT ?`, siteID, limit)
}

// UnverifiedBackups returns every record whose restore test has not run,
// oldest first.
func (s *Store) UnverifiedBackups() ([]Backup, error) {
	return s.queryBackups(`WHERE verify_status=? ORDER BY id`, string(VerifyPending))
}

// LatestVerifiedBackup returns a site's newest record in repository whose
// restore test passed: retention keeps it whatever its age, and it cannot
// be deleted.
func (s *Store) LatestVerifiedBackup(siteID int64, repository string) (Backup, error) {
	return s.openBackup(scanBackupRow(s.db.QueryRow(`SELECT `+backupCols+` FROM backups
		WHERE site_id=? AND repository=? AND verify_status=? ORDER BY id DESC LIMIT 1`,
		siteID, RepoKey(repository), string(VerifyPassed))))
}

// CountBackups counts a site's records in repository.
func (s *Store) CountBackups(siteID int64, repository string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM backups WHERE site_id=? AND repository=?`, siteID, RepoKey(repository)).Scan(&n)
	return n, err
}

// BackupRepositories lists every repository a backup record names, sorted:
// what `cgctl uninstall` tells the operator it kept (R-6.4).
func (s *Store) BackupRepositories() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT repository FROM backups WHERE repository != '' ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i], err = s.openRepo(out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ErrRepositoryChanged is a move of the backup repository that finds the
// setting no longer naming the repository it moves from.
var ErrRepositoryChanged = errors.New("the backup repository changed")

// BackupsInRepository is every record in repository, oldest first.
func (s *Store) BackupsInRepository(repository string) ([]Backup, error) {
	return s.queryBackups(`WHERE repository=? ORDER BY id`, RepoKey(repository))
}

// MoveBackupRepository points the setting and every record in from at the
// repository change sets (internal/api SPEC §5.9, #192), in one write
// transaction: change edits the settings (their repository, its
// credentials), and secrets are written with them (a value "" deletes
// one). The settings must still name from, else ErrRepositoryChanged and
// nothing changes. It returns how many records moved.
func (s *Store) MoveBackupRepository(from string, change func(*PanelSettings) error, secrets map[string]string) (int, error) {
	moved := 0
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		q := connExec{conn: conn, ctx: ctx}
		p, err := s.updatePanelTx(conn, ctx, func(p *PanelSettings) error {
			if RepoKey(p.BackupRepository) != RepoKey(from) {
				return ErrRepositoryChanged
			}
			return change(p)
		})
		if err != nil {
			return err
		}
		for name, v := range secrets {
			if err := s.setSecret(q, name, v); err != nil {
				return err
			}
		}
		to, err := s.storedRepo(q, p.BackupRepository)
		if err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, `UPDATE backups SET repository=? WHERE repository=?`, to, RepoKey(from))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		moved = int(n)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.publish(events.SettingsChanged, 0, nil)
	return moved, nil
}

// DeleteBackup removes one record (its snapshot has been forgotten).
func (s *Store) DeleteBackup(id int64) error {
	var siteID int64
	err := s.db.QueryRow(`DELETE FROM backups WHERE id=? RETURNING site_id`, id).Scan(&siteID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	}
	s.publish(events.BackupRemoved, siteID, map[string]any{"count": 1, "id": id})
	return nil
}

// DeleteBackupsBySnapshot removes a site's records in repository whose
// snapshot is one of ids, and returns how many went. Two IDs match when
// one is a prefix of the other (restic reports full IDs, a record may hold
// the short form), and only from eight hex digits on.
func (s *Store) DeleteBackupsBySnapshot(siteID int64, repository string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	records, err := s.queryBackups(`WHERE site_id=? AND repository=?`, siteID, RepoKey(repository))
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, b := range records {
		if !SnapshotMatches(b.SnapshotID, ids) {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM backups WHERE id=?`, b.ID); err != nil {
			return 0, err
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.publish(events.BackupRemoved, siteID, map[string]any{"count": n})
	return n, nil
}

// SnapshotMatches reports whether snapshot names the same snapshot as one
// of ids: one is a prefix of the other, both at least eight characters,
// case aside.
func SnapshotMatches(snapshot string, ids []string) bool {
	snapshot = strings.ToLower(snapshot)
	if len(snapshot) < 8 {
		return false
	}
	for _, id := range ids {
		id = strings.ToLower(id)
		if len(id) >= 8 && (strings.HasPrefix(id, snapshot) || strings.HasPrefix(snapshot, id)) {
			return true
		}
	}
	return false
}

// splitDatabases reads the databases column: NULL is not known (nil), ""
// none. A database name has no comma (letters, digits and _).
func splitDatabases(v sql.NullString) []string {
	switch {
	case !v.Valid:
		return nil
	case v.String == "":
		return []string{}
	}
	return strings.Split(v.String, ",")
}
