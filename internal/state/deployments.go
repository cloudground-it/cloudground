package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Deployments are a state machine (SPEC.md "Deployments", internal/api
// SPEC.md §4). The statuses below join the ones model_release.go names (created,
// guarding, blocked, promoted, rolled_back).
const (
	// DeployPending: accepted; its job has not started.
	DeployPending DeploymentStatus = "pending"
	// DeployBuilding: the agent is building the release.
	DeployBuilding DeploymentStatus = "building"
	// DeployPromoting: the agent is making the release live.
	DeployPromoting DeploymentStatus = "promoting"
	// DeployVerifying: live, and Performance Guard is checking it.
	DeployVerifying DeploymentStatus = "verifying"
	// DeploySuperseded: was live until another release was promoted.
	DeploySuperseded DeploymentStatus = "superseded"
	// DeployFailed: ended without a release to promote, or with one that
	// never went live.
	DeployFailed DeploymentStatus = "failed"
	// DeployCanceled: canceled before anything changed on the server.
	DeployCanceled DeploymentStatus = "canceled"
)

// deployMoves is every allowed transition: from → the statuses it may go
// to. Anything else is refused (ErrDeploymentState).
var deployMoves = map[DeploymentStatus][]DeploymentStatus{
	DeployPending:    {DeployBuilding, DeployGuarding, DeployCanceled, DeployFailed},
	DeployGuarding:   {DeployGuarding, DeployBlocked, DeployBuilding, DeployCanceled, DeployFailed},
	DeployBuilding:   {DeployCreated, DeployPromoting, DeployFailed},
	DeployCreated:    {DeployPromoting},
	DeploySuperseded: {DeployPromoting},
	DeployRolledBack: {DeployPromoting},
	DeployPromoted:   {DeployPromoting, DeploySuperseded, DeployRolledBack},
	DeployPromoting:  {DeployPromoted, DeployVerifying, DeployCreated, DeploySuperseded, DeployRolledBack},
	DeployVerifying:  {DeployVerifying, DeployPromoted, DeployRolledBack, DeployCreated, DeploySuperseded},
	DeployBlocked:    nil,
	DeployFailed:     nil,
	DeployCanceled:   nil,
}

// DeploymentCanMove reports whether a deployment in from may go to to.
func DeploymentCanMove(from, to DeploymentStatus) bool {
	return slices.Contains(deployMoves[from], to)
}

// DeploymentPromotable reports whether a release in status can be made
// live by a promote (or a rollback): it was built, and no job is working
// on it. (A safe push takes its own release from building to promoting.)
func DeploymentPromotable(status DeploymentStatus) bool {
	switch status {
	case DeployCreated, DeploySuperseded, DeployRolledBack, DeployPromoted:
		return true
	}
	return false
}

// DeploymentTerminal reports whether nothing can happen to a deployment in
// status any more.
func DeploymentTerminal(status DeploymentStatus) bool {
	moves, known := deployMoves[status]
	return known && len(moves) == 0
}

// ErrDeploymentState is a transition the state machine does not allow.
var ErrDeploymentState = errors.New("deployment state")

// StateError says which transition was refused.
type StateError struct {
	From, To DeploymentStatus
}

func (e *StateError) Error() string {
	return fmt.Sprintf("the deployment is %s and cannot become %s", e.From, e.To)
}

func (e *StateError) Unwrap() error { return ErrDeploymentState }

const deploymentCols = `id, site_id, release_id, path, checksum, status, guard_json, created_at, promoted_at`

type scanner interface{ Scan(...any) error }

func scanDeploymentRow(row scanner) (Deployment, error) {
	var d Deployment
	var status, created string
	var promoted sql.NullString
	if err := row.Scan(&d.ID, &d.SiteID, &d.ReleaseID, &d.Path, &d.Checksum, &status, &d.GuardJSON, &created, &promoted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, err
	}
	d.Status, d.CreatedAt, d.PromotedAt = DeploymentStatus(status), parseTime(created), parseTimePtr(promoted)
	return d, nil
}

// InitialRelease is the release provisioning creates (site.InitialRelease).
// A site has at most one record of it, as of any release: the table is
// UNIQUE (site_id, release_id).
const InitialRelease = "initial"

// RecordInitialRelease gives a site's initial release a record in status
// when it has none, and reports whether it made one. The check and the
// insert are one transaction: a reconcile and a rollback that both found
// no record used to both insert one, and the loser failed on the table's
// unique constraint (an error in the journal for a record that was there).
func (s *Store) RecordInitialRelease(siteID int64, status DeploymentStatus) (bool, error) {
	if _, known := deployMoves[status]; !known {
		return false, fmt.Errorf("unknown deployment status %q", status)
	}
	var made Deployment
	var superseded []int64
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE site_id = ? AND release_id = ?`,
			siteID, InitialRelease).Scan(&n); err != nil || n > 0 {
			return err
		}
		var err error
		made, superseded, err = insertDeployment(ctx, conn, Deployment{SiteID: siteID, ReleaseID: InitialRelease, Status: status})
		return err
	})
	if err != nil || made.ID == 0 {
		return false, err
	}
	s.publishDeployments(siteID, superseded, DeploySuperseded)
	s.publish(events.DeploymentChanged, siteID, map[string]any{"id": made.ID, "status": made.Status})
	return true, nil
}

// CreateDeployment records a deployment in its first status. A record
// created live (a migration's release) takes over from the site's live
// one, which becomes superseded: a site has at most one promoted release.
func (s *Store) CreateDeployment(d Deployment) (Deployment, error) {
	if _, known := deployMoves[d.Status]; !known {
		return Deployment{}, fmt.Errorf("unknown deployment status %q", d.Status)
	}
	var superseded []int64
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		var err error
		d, superseded, err = insertDeployment(ctx, conn, d)
		return err
	})
	if err != nil {
		return Deployment{}, err
	}
	s.publishDeployments(d.SiteID, superseded, DeploySuperseded)
	s.publish(events.DeploymentChanged, d.SiteID, map[string]any{"id": d.ID, "status": d.Status})
	return d, nil
}

// insertDeployment inserts d in its first status, inside a write
// transaction; a live record supersedes the site's live one, whose ID is
// returned.
func insertDeployment(ctx context.Context, conn *sql.Conn, d Deployment) (Deployment, []int64, error) {
	ts := now()
	var promoted any
	var superseded []int64
	if d.Status == DeployPromoted {
		var err error
		if superseded, err = supersedeLive(ctx, conn, d.SiteID, 0); err != nil {
			return Deployment{}, nil, err
		}
		promoted = ts
	}
	res, err := conn.ExecContext(ctx, `INSERT INTO deployments (site_id, release_id, path, checksum, status, guard_json, created_at, promoted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.SiteID, d.ReleaseID, d.Path, d.Checksum, string(d.Status), d.GuardJSON, ts, promoted)
	if err != nil {
		return Deployment{}, nil, err
	}
	d.ID, _ = res.LastInsertId()
	d.CreatedAt = parseTime(ts)
	if promoted != nil {
		d.PromotedAt = parseTimePtr(sql.NullString{String: ts, Valid: true})
	}
	return d, superseded, nil
}

// DeploymentUpdate is what a transition may also record.
type DeploymentUpdate struct {
	// GuardJSON replaces the Guard report when not empty.
	GuardJSON string
	// Path and Checksum, set together when the release has been built.
	Path, Checksum string
	// Regressed marks a release a safe push rolled back for a regression:
	// it is never a rollback's target.
	Regressed bool
	// Note is what a person should know about the deployment now (it
	// raises an alert); a move without one clears the previous note.
	Note string
}

// MoveDeployment takes a deployment from one of the statuses in from (any
// status when from is empty) to to, if the state machine allows it, and
// records what u carries. Becoming promoted stamps promoted_at and
// supersedes the site's other promoted release. A refused move changes
// nothing and returns a *StateError; a missing deployment ErrNotFound.
func (s *Store) MoveDeployment(id int64, from []DeploymentStatus, to DeploymentStatus, u DeploymentUpdate) (Deployment, error) {
	var d Deployment
	var superseded []int64
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		var err error
		d, err = scanDeploymentRow(conn.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id=?`, id))
		if err != nil {
			return err
		}
		if len(from) > 0 && !slices.Contains(from, d.Status) || !DeploymentCanMove(d.Status, to) {
			return &StateError{From: d.Status, To: to}
		}
		ts := now()
		var promoted any
		if to == DeployPromoted {
			if superseded, err = supersedeLive(ctx, conn, d.SiteID, d.ID); err != nil {
				return err
			}
			promoted = ts
		}
		if u.GuardJSON != "" {
			d.GuardJSON = u.GuardJSON
		}
		if u.Path != "" {
			d.Path, d.Checksum = u.Path, u.Checksum
		}
		if _, err := conn.ExecContext(ctx, `UPDATE deployments SET status=?, guard_json=?, path=?, checksum=?,
			promoted_at=COALESCE(?, promoted_at), regressed=MAX(regressed, ?), note=? WHERE id=?`,
			string(to), d.GuardJSON, d.Path, d.Checksum, promoted, u.Regressed, u.Note, d.ID); err != nil {
			return err
		}
		d.Status = to
		if promoted != nil {
			d.PromotedAt = parseTimePtr(sql.NullString{String: ts, Valid: true})
		}
		return nil
	})
	if err != nil {
		return Deployment{}, err
	}
	s.publishDeployments(d.SiteID, superseded, DeploySuperseded)
	s.publish(events.DeploymentChanged, d.SiteID, map[string]any{"id": d.ID, "status": d.Status})
	return d, nil
}

// supersedeLive moves the site's promoted releases but keep to superseded
// and returns which.
func supersedeLive(ctx context.Context, conn *sql.Conn, siteID, keep int64) ([]int64, error) {
	rows, err := conn.QueryContext(ctx, `SELECT id FROM deployments WHERE site_id=? AND status=? AND id<>?`,
		siteID, string(DeployPromoted), keep)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Close(), rows.Err()); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx, `UPDATE deployments SET status=? WHERE id=?`, string(DeploySuperseded), id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (s *Store) publishDeployments(siteID int64, ids []int64, status DeploymentStatus) {
	for _, id := range ids {
		s.publish(events.DeploymentChanged, siteID, map[string]any{"id": id, "status": status})
	}
}

// RecordRollback records that a rollback made release live on the site:
// the site's promoted release becomes rolled_back and the record of
// release, when there is one, promoted (a release the panel has no record
// of, such as provisioning's, leaves none promoted). A release that was
// never live, or one rolled back for a regression, is refused
// (ErrNeverLive) and nothing changes. It returns the deployment rolled
// back from (zero when none was live).
func (s *Store) RecordRollback(siteID int64, release string) (Deployment, error) {
	var from, to Deployment
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		if err := checkTarget(ctx, conn, siteID, release); err != nil {
			return err
		}
		var err error
		from, err = scanDeploymentRow(conn.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments
			WHERE site_id=? AND status=? ORDER BY id DESC LIMIT 1`, siteID, string(DeployPromoted)))
		switch {
		case errors.Is(err, ErrNotFound):
			from = Deployment{}
		case err != nil:
			return err
		case from.ReleaseID == release:
			from = Deployment{} // already the live one: nothing moves
			return nil
		default:
			if _, err := conn.ExecContext(ctx, `UPDATE deployments SET status=?, note='' WHERE id=?`, string(DeployRolledBack), from.ID); err != nil {
				return err
			}
			from.Status = DeployRolledBack
		}
		to, err = scanDeploymentRow(conn.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments
			WHERE site_id=? AND release_id=?`, siteID, release))
		switch {
		case errors.Is(err, ErrNotFound):
			to = Deployment{}
			return nil
		case err != nil:
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE deployments SET status=?, promoted_at=?, note='' WHERE id=?`, string(DeployPromoted), now(), to.ID); err != nil {
			return err
		}
		to.Status = DeployPromoted
		return nil
	})
	if err != nil {
		return Deployment{}, err
	}
	for _, d := range []Deployment{from, to} {
		if d.ID != 0 {
			s.publish(events.DeploymentChanged, siteID, map[string]any{"id": d.ID, "status": d.Status})
		}
	}
	return from, nil
}

// ListDeployments returns a site's deployments, newest first (at most
// limit; 20 when limit is not positive).
func (s *Store) ListDeployments(siteID int64, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+deploymentCols+` FROM deployments WHERE site_id=? ORDER BY id DESC LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		d, err := scanDeploymentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDeployment reads one deployment.
func (s *Store) GetDeployment(id int64) (Deployment, error) {
	return scanDeploymentRow(s.db.QueryRow(`SELECT `+deploymentCols+` FROM deployments WHERE id=?`, id))
}
