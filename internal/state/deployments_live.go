package state

import (
	"context"
	"database/sql"
	"errors"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// What is live, and where a rollback goes (SPEC.md "Deployments"; internal/
// api/SPEC.md §4.2). The record follows the server: the agent's
// CurrentRelease is the truth, SyncLive aligns the records with it.

// ErrNeverLive refuses a rollback to a release that was never live, or was
// rolled back for a regression.
var ErrNeverLive = errors.New("that release was never live (or was rolled back for a regression): it is not a rollback target")

// DeploymentMeta is what the state machine keeps beside a deployment.
type DeploymentMeta struct {
	// RollbackTo is the release that was live when this one was promoted.
	RollbackTo string `json:"rollback_to,omitempty"`
	// Regressed: a safe push rolled it back for a regression.
	Regressed bool `json:"regressed,omitempty"`
	// Note is what a person should know (it raises an alert).
	Note string `json:"note,omitempty"`
}

// GetDeploymentMeta reads a deployment's meta.
func (s *Store) GetDeploymentMeta(id int64) (DeploymentMeta, error) {
	var m DeploymentMeta
	err := s.db.QueryRow(`SELECT rollback_to, regressed, note FROM deployments WHERE id=?`, id).Scan(&m.RollbackTo, &m.Regressed, &m.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// DeploymentMetas reads the meta of a site's deployments, by ID.
func (s *Store) DeploymentMetas(siteID int64) (map[int64]DeploymentMeta, error) {
	rows, err := s.db.Query(`SELECT id, rollback_to, regressed, note FROM deployments WHERE site_id=?`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]DeploymentMeta{}
	for rows.Next() {
		var id int64
		var m DeploymentMeta
		if err := rows.Scan(&id, &m.RollbackTo, &m.Regressed, &m.Note); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// SetRollbackTo records the release that was live when the deployment was
// promoted, at each promotion. (A promote run again after a restart finds
// its own release live, and the caller then records nothing.)
func (s *Store) SetRollbackTo(id int64, release string) error {
	_, err := s.db.Exec(`UPDATE deployments SET rollback_to=? WHERE id=?`, release, id)
	return err
}

// SetDeploymentNote records (or, with "", clears) what a person should
// know about a deployment.
func (s *Store) SetDeploymentNote(id int64, note string) error {
	var siteID int64
	err := s.db.QueryRow(`UPDATE deployments SET note=? WHERE id=? RETURNING site_id`, note, id).Scan(&siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err == nil {
		s.publish(events.DeploymentChanged, siteID, map[string]any{"id": id})
	}
	return err
}

// NotedDeployment is a deployment with a note, for the alerts.
type NotedDeployment struct {
	ID, SiteID int64
	ReleaseID  string
	Note       string
}

// NotedDeployments lists every deployment with a note.
func (s *Store) NotedDeployments() ([]NotedDeployment, error) {
	rows, err := s.db.Query(`SELECT id, site_id, release_id, note FROM deployments WHERE note<>'' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotedDeployment
	for rows.Next() {
		var n NotedDeployment
		if err := rows.Scan(&n.ID, &n.SiteID, &n.ReleaseID, &n.Note); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InFlightDeployments lists the deployments some job left promoting or
// verifying, oldest first.
func (s *Store) InFlightDeployments() ([]Deployment, error) {
	rows, err := s.db.Query(`SELECT `+deploymentCols+` FROM deployments WHERE status IN (?, ?) ORDER BY id`,
		string(DeployPromoting), string(DeployVerifying))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeploymentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// wasLive reports whether a deployment in status (regressed or not) is one
// a rollback may go back to: it was live, and not condemned by a safe push.
func wasLive(status DeploymentStatus, regressed bool) bool {
	switch status {
	case DeploySuperseded, DeployPromoted:
		return true
	case DeployRolledBack:
		return !regressed
	}
	return false
}

// checkTarget refuses a rollback target the site has a record of that was
// never live. A release without a record (provisioning's) is accepted.
func checkTarget(ctx context.Context, conn *sql.Conn, siteID int64, release string) error {
	var status string
	var regressed bool
	err := conn.QueryRowContext(ctx, `SELECT status, regressed FROM deployments WHERE site_id=? AND release_id=?`, siteID, release).Scan(&status, &regressed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case !wasLive(DeploymentStatus(status), regressed):
		return ErrNeverLive
	}
	return nil
}

// CheckRollbackTarget is checkTarget outside a transaction.
func (s *Store) CheckRollbackTarget(siteID int64, release string) error {
	return s.writeTx(func(conn *sql.Conn, ctx context.Context) error { return checkTarget(ctx, conn, siteID, release) })
}

// RollbackTarget is where a rollback of the site goes: the release that
// was live before the live one was promoted (its rollback_to), when that
// may be gone back to; else the newest superseded release, by when it was
// promoted. ErrNotFound when there is none.
func (s *Store) RollbackTarget(siteID int64) (string, error) {
	var live, back string
	err := s.db.QueryRow(`SELECT release_id, rollback_to FROM deployments WHERE site_id=? AND status=?`,
		siteID, string(DeployPromoted)).Scan(&live, &back)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if back != "" && back != live {
		if err := s.CheckRollbackTarget(siteID, back); err == nil {
			return back, nil
		}
	}
	var id string
	err = s.db.QueryRow(`SELECT release_id FROM deployments WHERE site_id=? AND status=? AND regressed=0 AND release_id<>?
		ORDER BY COALESCE(promoted_at, created_at) DESC, id DESC LIMIT 1`, siteID, string(DeploySuperseded), live).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// SyncLive makes the records say that release is what the site serves:
// its deployment, when there is one, promoted; every other promoted one
// superseded. It reports whether anything changed (the records had said
// otherwise) and, then, which release they had said was live.
func (s *Store) SyncLive(siteID int64, release string) (changed bool, recorded string, err error) {
	var touched []int64
	err = s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		err := conn.QueryRowContext(ctx, `SELECT release_id FROM deployments WHERE site_id=? AND status=?`,
			siteID, string(DeployPromoted)).Scan(&recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if recorded == release {
			return nil
		}
		changed = true
		rows, err := conn.QueryContext(ctx, `SELECT id FROM deployments WHERE site_id=? AND status=?`, siteID, string(DeployPromoted))
		if err != nil {
			return err
		}
		var old []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			old = append(old, id)
		}
		if err := errors.Join(rows.Close(), rows.Err()); err != nil {
			return err
		}
		for _, id := range old {
			if _, err := conn.ExecContext(ctx, `UPDATE deployments SET status=? WHERE id=?`, string(DeploySuperseded), id); err != nil {
				return err
			}
			touched = append(touched, id)
		}
		var id int64
		err = conn.QueryRowContext(ctx, `SELECT id FROM deployments WHERE site_id=? AND release_id=?`, siteID, release).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE deployments SET status=?, promoted_at=COALESCE(promoted_at, ?) WHERE id=?`,
			string(DeployPromoted), now(), id); err != nil {
			return err
		}
		touched = append(touched, id)
		return nil
	})
	if err != nil {
		return false, "", err
	}
	for _, id := range touched {
		s.publish(events.DeploymentChanged, siteID, map[string]any{"id": id})
	}
	return changed, recorded, nil
}
