package state

import (
	"database/sql"
	"errors"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// A site's cron jobs (SPEC.md, Records). Each write announces cron.changed
// for the job's site, so an open Cron page refreshes.

const cronSelect = `SELECT id, site_id, schedule, command, description, enabled, last_run, last_status, created_at FROM cron_jobs`

// readCron fills a CronJob from one row of cronSelect.
func readCron(sc interface{ Scan(...any) error }) (CronJob, error) {
	var (
		j       CronJob
		on      bool
		ran     sql.NullString
		created string
	)
	switch err := sc.Scan(&j.ID, &j.SiteID, &j.Schedule, &j.Command, &j.Description, &on, &ran, &j.LastStatus, &created); {
	case errors.Is(err, sql.ErrNoRows):
		return CronJob{}, ErrNotFound
	case err != nil:
		return CronJob{}, err
	}
	j.Enabled, j.LastRun, j.CreatedAt = on, parseTimePtr(ran), parseTime(created)
	return j, nil
}

// cronChanged announces a write to one of site's jobs.
func (s *Store) cronChanged(site, job int64) {
	s.publish(events.CronChanged, site, map[string]any{"id": job})
}

// CreateCronJob stores a new job and returns it with its id and creation
// time.
func (s *Store) CreateCronJob(j CronJob) (CronJob, error) {
	at := now()
	err := s.db.QueryRow(`INSERT INTO cron_jobs (site_id, schedule, command, description, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, j.SiteID, j.Schedule, j.Command, j.Description, j.Enabled, at).Scan(&j.ID)
	if err != nil {
		return CronJob{}, err
	}
	j.CreatedAt = parseTime(at)
	s.cronChanged(j.SiteID, j.ID)
	return j, nil
}

// GetCronJob reads one job; ErrNotFound when there is none.
func (s *Store) GetCronJob(id int64) (CronJob, error) {
	return readCron(s.db.QueryRow(cronSelect+` WHERE id=?`, id))
}

// ListCronJobs returns a site's jobs in the order they were made.
func (s *Store) ListCronJobs(siteID int64) ([]CronJob, error) {
	rows, err := s.db.Query(cronSelect+` WHERE site_id=? ORDER BY id`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []CronJob
	for rows.Next() {
		j, err := readCron(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, j)
	}
	return list, rows.Err()
}

// cronWrite runs a statement that ends in RETURNING site_id on one job and
// announces it. missingOK makes an absent job a no-op instead of
// ErrNotFound.
func (s *Store) cronWrite(id int64, missingOK bool, query string, args ...any) error {
	var site int64
	err := s.db.QueryRow(query, args...).Scan(&site)
	if errors.Is(err, sql.ErrNoRows) {
		if missingOK {
			return nil
		}
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	s.cronChanged(site, id)
	return nil
}

// UpdateCronJob saves what a person can change about a job: schedule,
// command, description and whether it is enabled. Its site, its last run
// and its creation time stay as they are.
func (s *Store) UpdateCronJob(j CronJob) error {
	return s.cronWrite(j.ID, false, `UPDATE cron_jobs SET schedule=?, command=?, description=?, enabled=?
		WHERE id=? RETURNING site_id`, j.Schedule, j.Command, j.Description, j.Enabled, j.ID)
}

// DeleteCronJob removes a job; ErrNotFound when there is none.
func (s *Store) DeleteCronJob(id int64) error {
	return s.cronWrite(id, false, `DELETE FROM cron_jobs WHERE id=? RETURNING site_id`, id)
}

// UpdateCronRun records a run that ended now with status. A job deleted
// while it ran is not an error.
func (s *Store) UpdateCronRun(id int64, status string) error {
	return s.cronWrite(id, true, `UPDATE cron_jobs SET last_run=?, last_status=? WHERE id=? RETURNING site_id`, now(), status, id)
}
