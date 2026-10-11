package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// The job queue's rows (migration 14). The queue in internal/jobs decides
// every transition; these are its reads and writes, each one statement, each
// guarded by the status it expects so a late write never moves a job that
// has already ended.

// ErrJobState is returned when a job is not in the status a transition
// expects (it was canceled, finished or started meanwhile).
var ErrJobState = errors.New("job is not in the expected state")

const jobCols = `id, kind, site_id, status, progress, message, log_head, log_tail, log_dropped, error,
	error_key, error_args, payload, locks, dedup_key, quiet, background, attempts, cancel_requested, not_before,
	created_at, started_at, finished_at`

// jobColsNoLog is jobCols with the log left empty: the dispatcher reads
// every queued job often and needs none of it.
const jobColsNoLog = `id, kind, site_id, status, progress, message, '', '', log_dropped, error,
	error_key, error_args, payload, locks, dedup_key, quiet, background, attempts, cancel_requested, not_before,
	created_at, started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var status, locks, created, errArgs string
	var dedup, notBefore, started, finished sql.NullString
	var quiet, background, cancel int
	err := row.Scan(&j.ID, &j.Kind, &j.SiteID, &status, &j.Progress, &j.Message, &j.LogHead, &j.LogTail,
		&j.LogDropped, &j.Error, &j.ErrorKey, &errArgs, &j.Payload, &locks, &dedup, &quiet, &background, &j.Attempts, &cancel,
		&notBefore, &created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j.Status = JobStatus(status)
	if j.ErrorKey != "" && errArgs != "" {
		_ = json.Unmarshal([]byte(errArgs), &j.ErrorArgs)
	}
	if locks != "" {
		if err := json.Unmarshal([]byte(locks), &j.Locks); err != nil {
			return Job{}, err
		}
	}
	j.DedupKey = dedup.String
	j.Quiet, j.Background, j.CancelRequested = quiet != 0, background != 0, cancel != 0
	j.NotBefore = parseTimePtr(notBefore)
	j.CreatedAt = parseTime(created)
	j.StartedAt = parseTimePtr(started)
	j.FinishedAt = parseTimePtr(finished)
	return j, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func timeOrNull(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func locksJSON(locks []string) (string, error) {
	if locks == nil {
		locks = []string{}
	}
	b, err := json.Marshal(locks)
	return string(b), err
}

// CreateJob stores a new queued job. With a dedup key that a queued or
// running job already has, nothing is stored and that job is returned with
// existing = true.
func (s *Store) CreateJob(j Job) (job Job, existing bool, err error) {
	if j.Payload == "" {
		j.Payload = "{}"
	}
	if j.Locks == nil {
		j.Locks = []string{}
	}
	locks, err := locksJSON(j.Locks)
	if err != nil {
		return Job{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var dedup any
	if j.DedupKey != "" {
		dedup = j.DedupKey
		cur, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs
			WHERE dedup_key=? AND status IN ('queued', 'running')`, j.DedupKey))
		if err == nil {
			return cur, true, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Job{}, false, err
		}
	}
	ts := now()
	res, err := tx.Exec(`INSERT INTO jobs (kind, site_id, status, payload, locks, dedup_key, quiet, background, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Kind, j.SiteID, string(JobQueued), j.Payload, locks, dedup, boolInt(j.Quiet), boolInt(j.Background), ts)
	if err != nil {
		return Job{}, false, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return Job{ID: id, Kind: j.Kind, SiteID: j.SiteID, Status: JobQueued, Payload: j.Payload,
		Locks: j.Locks, DedupKey: j.DedupKey, Quiet: j.Quiet, Background: j.Background,
		CreatedAt: parseTime(ts)}, false, nil
}

// GetJob fetches one job.
func (s *Store) GetJob(id int64) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) queryJobs(query string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListJobs returns the newest jobs first; quiet (maintenance) jobs only
// when includeQuiet.
func (s *Store) ListJobs(limit int, includeQuiet bool) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	if includeQuiet {
		return s.queryJobs(`SELECT `+jobCols+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
	}
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE quiet=0 ORDER BY id DESC LIMIT ?`, limit)
}

// ActiveJobs returns every queued and running job, oldest first.
func (s *Store) ActiveJobs() ([]Job, error) {
	return s.queryJobs(`SELECT ` + jobCols + ` FROM jobs WHERE status IN ('queued', 'running') ORDER BY id`)
}

// QueuedJobs returns the queued jobs, oldest first, without their logs.
func (s *Store) QueuedJobs() ([]Job, error) {
	return s.queryJobs(`SELECT ` + jobColsNoLog + ` FROM jobs WHERE status='queued' ORDER BY id`)
}

// SiteHasActiveJob reports whether a queued or running job other than
// except is about site.
func (s *Store) SiteHasActiveJob(siteID, except int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE site_id=? AND id<>? AND status IN ('queued', 'running')`,
		siteID, except).Scan(&n)
	return n > 0, err
}

// StartJob moves a queued job to running and counts the attempt.
func (s *Store) StartJob(id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRow(`UPDATE jobs SET status='running', started_at=?, attempts=attempts+1, not_before=NULL
		WHERE id=? AND status='queued' RETURNING `+jobCols, now(), id))
	if errors.Is(err, ErrNotFound) {
		return Job{}, ErrJobState
	}
	return j, err
}

// SetJobLocks replaces a queued job's lock keys.
func (s *Store) SetJobLocks(id int64, locks []string) error {
	l, err := locksJSON(locks)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE jobs SET locks=? WHERE id=? AND status='queued'`, l, id)
	return err
}

// SaveJobProgress records a running job's progress, message and log.
func (s *Store) SaveJobProgress(id int64, progress int, message, logHead, logTail string, logDropped int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET progress=?, message=?, log_head=?, log_tail=?, log_dropped=?
		WHERE id=? AND status='running'`, progress, message, logHead, logTail, logDropped, id)
	return err
}

// KeyedError is an error that names its message in the API's catalogue
// (internal/i18n) with the values to format it with; agentrpc.Error is one.
type KeyedError interface {
	error
	MessageKey() (key string, args []string)
}

// ErrorKeyOf is the translation key and values err (or an error it wraps)
// carries, "" when none.
func ErrorKeyOf(err error) (string, []string) {
	var ke KeyedError
	if errors.As(err, &ke) {
		return ke.MessageKey()
	}
	return "", nil
}

// FinishJob records a job's end, with its log as it stands. errKey and
// errArgs, when the failure carried them, name errMsg in the catalogue so
// each reader gets it in their language. Only a queued or running job can
// end; anything else is ErrJobState.
func (s *Store) FinishJob(id int64, status JobStatus, errMsg, errKey string, errArgs []string, progress int, logHead, logTail string, logDropped int64) (Job, error) {
	args := ""
	if errKey != "" {
		b, err := json.Marshal(errArgs)
		if err != nil {
			return Job{}, err
		}
		args = string(b)
	}
	j, err := scanJob(s.db.QueryRow(`UPDATE jobs SET status=?, error=?, error_key=?, error_args=?, progress=?, finished_at=?,
		log_head=?, log_tail=?, log_dropped=?
		WHERE id=? AND status IN ('queued', 'running') RETURNING `+jobCols,
		string(status), errMsg, errKey, args, progress, now(), logHead, logTail, logDropped, id))
	if errors.Is(err, ErrNotFound) {
		return Job{}, ErrJobState
	}
	return j, err
}

// FinishQueuedJob ends a job that has not started (a cancel, or recovery
// failing a job whose work is gone). ErrJobState if it is not queued.
func (s *Store) FinishQueuedJob(id int64, status JobStatus, errMsg string) (Job, error) {
	j, err := scanJob(s.db.QueryRow(`UPDATE jobs SET status=?, error=?, finished_at=?
		WHERE id=? AND status='queued' RETURNING `+jobCols, string(status), errMsg, now(), id))
	if errors.Is(err, ErrNotFound) {
		return Job{}, ErrJobState
	}
	return j, err
}

// RequeueJob puts a running job back in the queue to run again from the
// start, not before notBefore (nil: at once), with the log as it now
// stands (a line saying why included).
func (s *Store) RequeueJob(id int64, logHead, logTail string, logDropped int64, notBefore *time.Time) error {
	res, err := s.db.Exec(`UPDATE jobs SET status='queued', log_head=?, log_tail=?, log_dropped=?, not_before=?
		WHERE id=? AND status='running'`, logHead, logTail, logDropped, timeOrNull(notBefore), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobState
	}
	return nil
}

// SetJobCancelRequested marks a running job as asked to stop.
func (s *Store) SetJobCancelRequested(id int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET cancel_requested=1 WHERE id=? AND status='running'`, id)
	return err
}

// PruneJobs deletes quiet jobs finished before quietBefore and every job
// finished before allBefore.
func (s *Store) PruneJobs(quietBefore, allBefore time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE finished_at IS NOT NULL AND
		((quiet=1 AND finished_at < ?) OR finished_at < ?)`,
		quietBefore.UTC().Format(time.RFC3339Nano), allBefore.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
