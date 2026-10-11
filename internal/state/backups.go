package state

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Backup records (SPEC.md "Backups"). A row is one restic snapshot of a
// site, as the panel knows it: in which repository, how big, and what its
// restore test found. It is created pending; only a restore test moves it
// to passed or failed.

const backupCols = `id, site_id, snapshot_id, repository, size_bytes, kind, verify_status, verified_at, restore_est_ms, created_at`

func scanBackupRow(row scanner) (Backup, error) {
	var b Backup
	var verify, created string
	var verified sql.NullString
	if err := row.Scan(&b.ID, &b.SiteID, &b.SnapshotID, &b.Repository, &b.SizeBytes, &b.Kind,
		&verify, &verified, &b.RestoreEstMS, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Backup{}, ErrNotFound
		}
		return Backup{}, err
	}
	b.VerifyStatus, b.VerifiedAt, b.CreatedAt = BackupVerify(verify), parseTimePtr(verified), parseTime(created)
	return b, nil
}

func (s *Store) queryBackups(q string, args ...any) ([]Backup, error) {
	rows, err := s.db.Query(`SELECT `+backupCols+` FROM backups `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Backup{}
	for rows.Next() {
		b, err := scanBackupRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateBackup records a snapshot just taken. Whatever b says, it starts
// pending: a snapshot counts only once its restore test passed.
func (s *Store) CreateBackup(b Backup) (Backup, error) {
	ts := now()
	res, err := s.db.Exec(`INSERT INTO backups (site_id, snapshot_id, repository, size_bytes, kind, verify_status, restore_est_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.SiteID, b.SnapshotID, b.Repository, b.SizeBytes, b.Kind, string(VerifyPending), b.RestoreEstMS, ts)
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
	return scanBackupRow(s.db.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id=?`, id))
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
	return scanBackupRow(s.db.QueryRow(`SELECT `+backupCols+` FROM backups
		WHERE site_id=? AND repository=? AND verify_status=? ORDER BY id DESC LIMIT 1`,
		siteID, repository, string(VerifyPassed)))
}

// CountBackups counts a site's records in repository.
func (s *Store) CountBackups(siteID int64, repository string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM backups WHERE site_id=? AND repository=?`, siteID, repository).Scan(&n)
	return n, err
}

// BackupRepositories lists every repository a backup record names, sorted:
// what `cgctl uninstall` tells the operator it kept (R-6.4).
func (s *Store) BackupRepositories() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT repository FROM backups WHERE repository != '' ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
	records, err := s.queryBackups(`WHERE site_id=? AND repository=?`, siteID, repository)
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
