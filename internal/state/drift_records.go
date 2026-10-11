package state

import (
	"database/sql"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Managed files and drift (SPEC.md, Records): the hash of every file the
// panel rendered, and the events of a file found different from it.

// RecordManagedFile sets the hash the panel expects at path.
func (s *Store) RecordManagedFile(path, sha string) error {
	_, err := s.db.Exec(`INSERT INTO managed_files (path, sha256, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256, updated_at=excluded.updated_at`, path, sha, now())
	return err
}

// RemoveManagedFile stops tracking path; an untracked path is not an error.
func (s *Store) RemoveManagedFile(path string) error {
	_, err := s.db.Exec(`DELETE FROM managed_files WHERE path=?`, path)
	return err
}

// ManagedFiles maps every tracked path to the hash the panel expects.
func (s *Store) ManagedFiles() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT path, sha256 FROM managed_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make(map[string]string)
	for rows.Next() {
		var path, sha string
		if err := rows.Scan(&path, &sha); err != nil {
			return nil, err
		}
		files[path] = sha
	}
	return files, rows.Err()
}

// driftChanged announces that the open drift set changed.
func (s *Store) driftChanged(id int64) {
	s.publish(events.DriftChanged, 0, map[string]any{"id": id})
}

// CreateDriftEvent opens an event for a file found different from what the
// panel wrote, and returns it with its id, status and time.
func (s *Store) CreateDriftEvent(d DriftEvent) (DriftEvent, error) {
	at := now()
	err := s.db.QueryRow(`INSERT INTO drift_events (path, expected_hash, actual_hash, status, detected_at)
		VALUES (?, ?, ?, ?, ?) RETURNING id`, d.Path, d.ExpectedHash, d.ActualHash, string(DriftOpen), at).Scan(&d.ID)
	if err != nil {
		return DriftEvent{}, err
	}
	d.Status, d.DetectedAt, d.ResolvedAt = DriftOpen, parseTime(at), nil
	s.driftChanged(d.ID)
	return d, nil
}

// ResolveDriftEvent closes an event as restored (the panel's file is back)
// or accepted (the change is kept). It announces only a row it changed.
func (s *Store) ResolveDriftEvent(id int64, status DriftStatus) error {
	res, err := s.db.Exec(`UPDATE drift_events SET status=?, resolved_at=? WHERE id=?`, string(status), now(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		s.driftChanged(id)
	}
	return nil
}

// ListDriftEvents returns the newest 200 events, only the open ones unless
// includeResolved.
func (s *Store) ListDriftEvents(includeResolved bool) ([]DriftEvent, error) {
	where := `WHERE status = 'open'`
	if includeResolved {
		where = ``
	}
	rows, err := s.db.Query(`SELECT id, path, expected_hash, actual_hash, status, detected_at, resolved_at
		FROM drift_events ` + where + ` ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriftEvent
	for rows.Next() {
		var (
			d        DriftEvent
			status   string
			detected string
			resolved sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.Path, &d.ExpectedHash, &d.ActualHash, &status, &detected, &resolved); err != nil {
			return nil, err
		}
		d.Status, d.DetectedAt, d.ResolvedAt = DriftStatus(status), parseTime(detected), parseTimePtr(resolved)
		out = append(out, d)
	}
	return out, rows.Err()
}
