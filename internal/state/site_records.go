package state

// The sites table (SPEC.md "Sites", invariant 5). Scalar fields are
// columns; the aliases and the config are JSON documents.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

var (
	// ErrNotFound: no record has the ID or name asked for.
	ErrNotFound = errors.New("not found")
	// ErrDomainTaken: another site already has the domain.
	ErrDomainTaken = errors.New("a site with this domain already exists")
)

// siteColumns are read in this order by readSite.
var siteColumns = []string{
	"id", "domain", "aliases", "type", "profile", "engine", "php_version",
	"system_user", "root_path", "status", "staging_of", "config",
	"created_at", "updated_at", "desired_generation", "applied_generation", "last_apply_error",
}

// siteCols is siteColumns as a SELECT list.
var siteCols = strings.Join(siteColumns, ", ")

// rowScanner is a *sql.Row or *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// readSite decodes one row selected with siteCols.
func readSite(row rowScanner) (Site, error) {
	var (
		out                     Site
		aliasJSON, cfgJSON      string
		kind, prof, eng, status string
		createdAt, updatedAt    string
	)
	dest := []any{
		&out.ID, &out.Domain, &aliasJSON, &kind, &prof, &eng, &out.PHPVersion,
		&out.SystemUser, &out.RootPath, &status, &out.StagingOf, &cfgJSON,
		&createdAt, &updatedAt, &out.DesiredGeneration, &out.AppliedGeneration, &out.LastApplyError,
	}
	switch err := row.Scan(dest...); {
	case errors.Is(err, sql.ErrNoRows):
		return Site{}, ErrNotFound
	case err != nil:
		return Site{}, err
	}
	out.Type = SiteType(kind)
	out.Profile = Profile(prof)
	out.Engine = Engine(eng)
	out.Status = SiteStatus(status)
	if err := json.Unmarshal([]byte(aliasJSON), &out.Aliases); err != nil {
		return Site{}, fmt.Errorf("site %d: aliases column: %w", out.ID, err)
	}
	if err := json.Unmarshal([]byte(cfgJSON), &out.Config); err != nil {
		return Site{}, fmt.Errorf("site %d: config column: %w", out.ID, err)
	}
	out.CreatedAt = parseTime(createdAt)
	out.UpdatedAt = parseTime(updatedAt)
	return out, nil
}

// siteDocuments are a site's two JSON columns.
func siteDocuments(site Site) (aliases, cfg string, err error) {
	a, err := json.Marshal(site.Aliases)
	if err != nil {
		return "", "", err
	}
	c, err := json.Marshal(site.Config)
	if err != nil {
		return "", "", err
	}
	return string(a), string(c), nil
}

// oneSite reads the single site a WHERE clause selects.
func (s *Store) oneSite(where string, args ...any) (Site, error) {
	return readSite(s.db.QueryRow("SELECT "+siteCols+" FROM sites WHERE "+where, args...))
}

// manySites reads every site a query selects, in its order.
func (s *Store) manySites(query string, args ...any) ([]Site, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Site
	for rows.Next() {
		site, err := readSite(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, site)
	}
	return list, rows.Err()
}

// announceSite publishes a site's state on the event bus.
func (s *Store) announceSite(site Site, reason string) {
	s.publish(events.SiteChanged, site.ID, map[string]any{
		"reason":             reason,
		"status":             site.Status,
		"staging_of":         site.StagingOf,
		"desired_generation": site.DesiredGeneration,
		"applied_generation": site.AppliedGeneration,
	})
}

// CreateSite inserts site and answers it as stored, with its ID and
// generations. A domain in use already is ErrDomainTaken.
func (s *Store) CreateSite(site Site) (Site, error) {
	aliases, cfg, err := siteDocuments(site)
	if err != nil {
		return Site{}, err
	}
	stamp := now()
	// An ID given is kept (a site restored from another server keeps the
	// one it had there, internal/api SPEC §5.10); 0 takes the next.
	var given any
	if site.ID > 0 {
		given = site.ID
	}
	res, err := s.db.Exec(`INSERT INTO sites
		(id, domain, aliases, type, profile, engine, php_version, system_user, root_path, status, staging_of, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		given, site.Domain, aliases, string(site.Type), string(site.Profile), string(site.Engine), site.PHPVersion,
		site.SystemUser, site.RootPath, string(site.Status), site.StagingOf, cfg, stamp, stamp)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: sites.domain") {
			return Site{}, ErrDomainTaken
		}
		return Site{}, fmt.Errorf("insert site %s: %w", site.Domain, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Site{}, err
	}
	stored, err := s.GetSite(id)
	if err != nil {
		return Site{}, err
	}
	s.announceSite(stored, "created")
	return stored, nil
}

// GetSite reads one site by its ID.
func (s *Store) GetSite(id int64) (Site, error) { return s.oneSite("id = ?", id) }

// GetSiteByDomain reads one site by its primary domain.
func (s *Store) GetSiteByDomain(domain string) (Site, error) {
	return s.oneSite("domain = ?", domain)
}

// StagingSiteFor reads the staging copy of a production site.
func (s *Store) StagingSiteFor(productionID int64) (Site, error) {
	return s.oneSite("staging_of = ?", productionID)
}

// ListSites reads every site, by domain.
func (s *Store) ListSites() ([]Site, error) {
	return s.manySites("SELECT " + siteCols + " FROM sites ORDER BY domain")
}

// ListSitesForUser reads the sites assigned to a user and the staging
// copies of those (an operator works on the copies it makes), by domain.
func (s *Store) ListSitesForUser(userID int64) ([]Site, error) {
	return s.manySites("SELECT "+siteCols+` FROM sites
		WHERE id IN (SELECT site_id FROM user_sites WHERE user_id = ?)
		   OR staging_of IN (SELECT site_id FROM user_sites WHERE user_id = ?)
		ORDER BY domain`, userID, userID)
}

// MutateSite runs fn on the site as stored now and writes the result back,
// in one write transaction, so a change saved by someone else since the
// caller read the site is never overwritten with an older copy. fn must not
// use the Store (the transaction holds its only connection); its error
// aborts the change and comes back unwrapped. A change to the desired
// configuration raises DesiredGeneration; the generation fields and the ID
// are the store's, whatever fn does to them.
func (s *Store) MutateSite(id int64, fn func(*Site) error) (Site, error) {
	return s.rewriteSite(id, fn, false)
}

// MutateSiteApplied is MutateSite for a change the agent has made on the
// server already: a site that was fully applied stays fully applied.
func (s *Store) MutateSiteApplied(id int64, fn func(*Site) error) (Site, error) {
	return s.rewriteSite(id, fn, true)
}

// callerError carries fn's error out of the transaction untouched.
type callerError struct{ err error }

func (c callerError) Error() string { return c.err.Error() }

func (s *Store) rewriteSite(id int64, fn func(*Site) error, alreadyApplied bool) (Site, error) {
	var next Site
	txErr := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		cur, err := readSite(conn.QueryRowContext(ctx, "SELECT "+siteCols+" FROM sites WHERE id = ?", id))
		if err != nil {
			return err
		}
		next = cur
		next.Aliases = slices.Clone(cur.Aliases)
		if err := fn(&next); err != nil {
			return callerError{err}
		}
		next.ID = id
		next.DesiredGeneration = cur.DesiredGeneration
		next.AppliedGeneration = cur.AppliedGeneration
		next.LastApplyError = cur.LastApplyError
		if desiredDiffers(cur, next) {
			caughtUp := cur.AppliedGeneration == cur.DesiredGeneration
			next.DesiredGeneration++
			if alreadyApplied && caughtUp {
				next.AppliedGeneration = next.DesiredGeneration
			}
		}
		aliases, cfg, err := siteDocuments(next)
		if err != nil {
			return err
		}
		stamp := now()
		_, err = conn.ExecContext(ctx, `UPDATE sites SET
			domain = ?, aliases = ?, type = ?, profile = ?, engine = ?, php_version = ?, system_user = ?,
			root_path = ?, status = ?, staging_of = ?, config = ?, updated_at = ?,
			desired_generation = ?, applied_generation = ?
			WHERE id = ?`,
			next.Domain, aliases, string(next.Type), string(next.Profile), string(next.Engine), next.PHPVersion,
			next.SystemUser, next.RootPath, string(next.Status), next.StagingOf, cfg, stamp,
			next.DesiredGeneration, next.AppliedGeneration, id)
		if err != nil {
			return err
		}
		next.UpdatedAt = parseTime(stamp)
		return nil
	})
	var fromFn callerError
	switch {
	case errors.As(txErr, &fromFn):
		return Site{}, fromFn.err
	case errors.Is(txErr, ErrNotFound):
		return Site{}, ErrNotFound
	case txErr != nil:
		return Site{}, fmt.Errorf("write site %d: %w", id, txErr)
	}
	s.announceSite(next, "updated")
	return next, nil
}

// desiredDiffers reports whether two copies of a site describe different
// desired configurations. The status, the timestamps and the generation
// bookkeeping are not configuration; no aliases and an empty list are the
// same.
func desiredDiffers(a, b Site) bool {
	strip := func(x Site) Site {
		x.Status = ""
		x.CreatedAt, x.UpdatedAt = time.Time{}, time.Time{}
		x.DesiredGeneration, x.AppliedGeneration, x.LastApplyError = 0, 0, ""
		if len(x.Aliases) == 0 {
			x.Aliases = nil
		}
		return x
	}
	return !reflect.DeepEqual(strip(a), strip(b))
}

// MarkApplied records how rendering the site at generation gen went. On
// success the site is applied up to gen and its error cleared; on failure
// the error is recorded and the applied generation kept. A report about a
// generation older than the applied one changes nothing (a slow job cannot
// move the record back), and neither does one about a deleted site.
func (s *Store) MarkApplied(id, gen int64, applyErr error) error {
	query, args := `UPDATE sites SET applied_generation = ?, last_apply_error = '' WHERE id = ? AND applied_generation <= ?`,
		[]any{gen, id, gen}
	if applyErr != nil {
		query, args = `UPDATE sites SET last_apply_error = ? WHERE id = ? AND applied_generation <= ?`,
			[]any{applyErr.Error(), id, gen}
	}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return nil
	}
	if site, err := s.GetSite(id); err == nil {
		s.announceSite(site, "applied")
	}
	return nil
}

// DeleteSite removes a site and its secrets in one transaction; what
// references it (deployments, backups, cron jobs, assignments, certificate
// state, alert dismissals) cascades.
func (s *Store) DeleteSite(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteSecretsWithPrefix(tx, SiteSecretPrefix(id)); err != nil {
		return err
	}
	var parent int64
	err = tx.QueryRow(`DELETE FROM sites WHERE id = ? RETURNING staging_of`, id).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publish(events.SiteDeleted, id, map[string]any{"staging_of": parent})
	return nil
}
