package state

import (
	"database/sql"
	"errors"
	"time"
)

// When each recurring job last ran, by key (internal/jobs: the scheduler's
// persisted keys; the API's alert and retention checks).

// LastScheduleRun returns when key last ran, the zero time if never.
func (s *Store) LastScheduleRun(key string) (time.Time, error) {
	var at string
	switch err := s.db.QueryRow(`SELECT last_run FROM schedule_state WHERE key = ?`, key).Scan(&at); {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, err
	}
	return parseTime(at), nil
}

// MarkScheduleRun records that key ran now.
func (s *Store) MarkScheduleRun(key string) error {
	_, err := s.db.Exec(`INSERT INTO schedule_state (key, last_run) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET last_run = excluded.last_run`, key, now())
	return err
}

// ClearScheduleRun forgets key, so it is due on the next tick (a failed
// scheduled backup is retried that way).
func (s *Store) ClearScheduleRun(key string) error {
	_, err := s.db.Exec(`DELETE FROM schedule_state WHERE key = ?`, key)
	return err
}
