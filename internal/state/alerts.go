package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Alerts (SPEC.md invariant 6): one row per condition a person should act
// on, raised and cleared by the API's evaluator, read by the dashboard.

// Alert is one condition.
type Alert struct {
	// ID is the public id: AlertID(Key, Fingerprint).
	ID string
	// Key is the condition's kind and subject (task:42, service:nginx).
	Key string
	// Kind is what the interface groups it by: certificate, task, service,
	// drift or deploy.
	Kind string
	// SiteID is the site it is about, 0 for the server.
	SiteID int64
	// Subject names what an alert of no site is about, shown where a
	// site's domain would be: a deleted site's domain, for a task that
	// failed on it (its creation, rolled back).
	Subject string
	// Site is the site's domain, or Subject for an alert of no site,
	// filled when the alert is read.
	Site  string
	Level string
	// Fingerprint names this occurrence of the condition: a different one
	// is a new alert under the same key.
	Fingerprint string
	// Message is the alert in English.
	Message string
	Text    AlertText
	// At is a time the alert is about (an expiry, a task's start), as the
	// interface shows it; "" for none.
	At          string
	RaisedAt    time.Time
	SeenAt      time.Time
	DismissedAt *time.Time
	NotifiedAt  *time.Time
}

// AlertText is how Message is written in another language: a catalogue
// key, a count when the key has plural forms, and the arguments. A task's
// error may have a translation of its own (DetailKey, DetailArgs), which
// then stands for the last argument.
type AlertText struct {
	Key        string
	Counted    bool
	Count      int
	Args       []string
	DetailKey  string
	DetailArgs []string
}

// alertStamp is now in a fixed-width form, so the times of this table sort as
// text (RFC3339Nano drops trailing zeros and does not).
func alertStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }

// AlertID is the public id of the occurrence fingerprint of the condition
// key.
func AlertID(key, fingerprint string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + fingerprint))
	return hex.EncodeToString(sum[:6])
}

const alertSelect = `SELECT a.public_id, a.key, a.kind, COALESCE(a.site_id, 0), COALESCE(s.domain, a.subject), a.subject, a.level,
	a.fingerprint, a.message, a.text_key, a.text_count, a.text_args, a.detail_key, a.detail_args, a.at,
	a.raised_at, a.seen_at, a.dismissed_at, a.notified_at
	FROM alerts a LEFT JOIN sites s ON s.id = a.site_id`

func readAlert(sc interface{ Scan(...any) error }) (Alert, error) {
	var (
		a                   Alert
		count               int
		args, detailArgs    string
		raised, seen        string
		dismissed, notified sql.NullString
	)
	err := sc.Scan(&a.ID, &a.Key, &a.Kind, &a.SiteID, &a.Site, &a.Subject, &a.Level, &a.Fingerprint, &a.Message,
		&a.Text.Key, &count, &args, &a.Text.DetailKey, &detailArgs, &a.At, &raised, &seen, &dismissed, &notified)
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, ErrNotFound
	}
	if err != nil {
		return Alert{}, err
	}
	a.Text.Counted, a.Text.Count = count >= 0, max(count, 0)
	_ = json.Unmarshal([]byte(args), &a.Text.Args)
	_ = json.Unmarshal([]byte(detailArgs), &a.Text.DetailArgs)
	a.RaisedAt, a.SeenAt = parseTime(raised), parseTime(seen)
	a.DismissedAt, a.NotifiedAt = parseTimePtr(dismissed), parseTimePtr(notified)
	return a, nil
}

// alertEvent is an event to publish once the transaction that caused it
// has committed.
type alertEvent struct {
	typ  events.Type
	site int64
	data map[string]any
}

func (s *Store) publishAlerts(evs []alertEvent) {
	for _, e := range evs {
		s.publish(e.typ, e.site, e.data)
	}
}

func cleared(id string, site int64) alertEvent {
	return alertEvent{events.AlertCleared, site, map[string]any{"id": id}}
}

// nullSite is a site id as the column holds it.
func nullSite(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func jsonStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// raiseAlert is RaiseAlert inside a transaction; the events it caused are
// appended to evs. An alert about a site that no longer exists is skipped.
func raiseAlert(ctx context.Context, conn *sql.Conn, a Alert, evs *[]alertEvent) error {
	domain := a.Subject
	if a.SiteID != 0 {
		err := conn.QueryRowContext(ctx, `SELECT domain FROM sites WHERE id=?`, a.SiteID).Scan(&domain)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	at := alertStamp()
	id := AlertID(a.Key, a.Fingerprint)
	count := -1
	if a.Text.Counted {
		count = a.Text.Count
	}
	var oldID, oldFP string
	var wasDismissed bool
	err := conn.QueryRowContext(ctx, `SELECT public_id, fingerprint, dismissed_at IS NOT NULL FROM alerts WHERE key=?`, a.Key).
		Scan(&oldID, &oldFP, &wasDismissed)
	fields := []any{a.Kind, nullSite(a.SiteID), a.Subject, a.Level, a.Message, a.Text.Key, count,
		jsonStrings(a.Text.Args), a.Text.DetailKey, jsonStrings(a.Text.DetailArgs), a.At, at}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Right after the upgrade to the table (migration 19) the
		// conditions the old panel already announced are raised again:
		// they count as notified, so the webhook does not repeat them all.
		if _, err := conn.ExecContext(ctx, `INSERT INTO alerts (kind, site_id, subject, level, message, text_key, text_count,
			text_args, detail_key, detail_args, at, seen_at, key, public_id, fingerprint, raised_at, notified_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				CASE WHEN EXISTS (SELECT 1 FROM meta WHERE key = '`+metaAlertsUpgraded+`') THEN ? END)`,
			append(fields, a.Key, id, a.Fingerprint, at, at)...); err != nil {
			return err
		}
	case err != nil:
		return err
	case oldFP == a.Fingerprint:
		// The same occurrence, seen again: its wording may have moved on (a
		// day count, a detail line), nothing else.
		_, err := conn.ExecContext(ctx, `UPDATE alerts SET kind=?, site_id=?, subject=?, level=?, message=?, text_key=?, text_count=?,
			text_args=?, detail_key=?, detail_args=?, at=?, seen_at=? WHERE key=?`, append(fields, a.Key)...)
		return err
	default:
		// A new occurrence under the same key replaces the old one, and
		// with it its dismissal and notification.
		if _, err := conn.ExecContext(ctx, `UPDATE alerts SET kind=?, site_id=?, subject=?, level=?, message=?, text_key=?, text_count=?,
			text_args=?, detail_key=?, detail_args=?, at=?, seen_at=?, public_id=?, fingerprint=?, raised_at=?,
			dismissed_at=NULL, notified_at=NULL WHERE key=?`, append(fields, id, a.Fingerprint, at, a.Key)...); err != nil {
			return err
		}
		if !wasDismissed {
			*evs = append(*evs, cleared(oldID, a.SiteID))
		}
	}
	*evs = append(*evs, alertEvent{events.AlertRaised, a.SiteID, map[string]any{
		"id": id, "level": a.Level, "kind": a.Kind, "site": domain, "message": a.Message,
	}})
	return nil
}

// clearAlerts deletes the rows matching where whose key is not in keep
// (every one of them when keep is nil), appending their events.
func clearAlerts(ctx context.Context, conn *sql.Conn, where string, args []any, keep map[string]bool, evs *[]alertEvent) error {
	rows, err := conn.QueryContext(ctx, `SELECT key, public_id, COALESCE(site_id, 0), dismissed_at IS NOT NULL FROM alerts WHERE `+where, args...)
	if err != nil {
		return err
	}
	type gone struct {
		key, id   string
		site      int64
		dismissed bool
	}
	var drop []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.key, &g.id, &g.site, &g.dismissed); err != nil {
			rows.Close()
			return err
		}
		if !keep[g.key] {
			drop = append(drop, g)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range drop {
		if _, err := conn.ExecContext(ctx, `DELETE FROM alerts WHERE key=?`, g.key); err != nil {
			return err
		}
		if !g.dismissed {
			*evs = append(*evs, cleared(g.id, g.site))
		}
	}
	return nil
}

// RaiseAlert records a condition: a new key is a new alert, the same
// fingerprint refreshes it, another fingerprint replaces it (SPEC.md
// invariant 6).
func (s *Store) RaiseAlert(a Alert) error {
	var evs []alertEvent
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error { return raiseAlert(ctx, conn, a, &evs) })
	if err == nil {
		s.publishAlerts(evs)
	}
	return err
}

// ClearAlert removes the condition key; an absent one is not an error.
func (s *Store) ClearAlert(key string) error {
	var evs []alertEvent
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		return clearAlerts(ctx, conn, `key=?`, []any{key}, nil, &evs)
	})
	if err == nil {
		s.publishAlerts(evs)
	}
	return err
}

// SyncAlerts makes set the alerts of kind: each is raised, and every other
// row of kind is cleared, in one transaction.
func (s *Store) SyncAlerts(kind string, set []Alert) error {
	var evs []alertEvent
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		keep := make(map[string]bool, len(set))
		for _, a := range set {
			a.Kind = kind
			if err := raiseAlert(ctx, conn, a, &evs); err != nil {
				return err
			}
			keep[a.Key] = true
		}
		return clearAlerts(ctx, conn, `kind=?`, []any{kind}, keep, &evs)
	})
	if err == nil {
		s.publishAlerts(evs)
	}
	return err
}

// metaAlertsUpgraded marks a panel upgraded to the alerts table whose
// first evaluation has not run yet (migration 19).
const metaAlertsUpgraded = "alerts_upgraded"

// EndAlertsUpgrade records that the first evaluation after the upgrade has
// run: alerts raised from now on are new, and are notified.
func (s *Store) EndAlertsUpgrade() error {
	_, err := s.db.Exec(`DELETE FROM meta WHERE key=?`, metaAlertsUpgraded)
	return err
}

// GetAlert reads the alert whose public id is id, dismissed or not.
func (s *Store) GetAlert(id string) (Alert, error) {
	return readAlert(s.db.QueryRow(alertSelect+` WHERE a.public_id=?`, id))
}

// DismissAlert hides an alert for everyone until its condition changes or
// clears. An unknown or already dismissed id is ErrNotFound.
func (s *Store) DismissAlert(id string) error {
	var site int64
	err := s.db.QueryRow(`UPDATE alerts SET dismissed_at=? WHERE public_id=? AND dismissed_at IS NULL
		RETURNING COALESCE(site_id, 0)`, alertStamp(), id).Scan(&site)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	s.publishAlerts([]alertEvent{cleared(id, site)})
	return nil
}

func (s *Store) queryAlerts(where string, args ...any) ([]Alert, error) {
	rows, err := s.db.Query(alertSelect+` WHERE `+where+
		` ORDER BY a.level = 'error' DESC, a.raised_at DESC, a.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := readAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAlerts returns every alert nobody dismissed, errors first, newest
// first.
func (s *Store) ListAlerts() ([]Alert, error) {
	return s.queryAlerts(`a.dismissed_at IS NULL`)
}

// AlertsToNotify returns the undismissed alerts never notified, or last
// notified before since.
func (s *Store) AlertsToNotify(since time.Time) ([]Alert, error) {
	list, err := s.queryAlerts(`a.dismissed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	due := list[:0]
	for _, a := range list {
		if a.NotifiedAt == nil || a.NotifiedAt.Before(since) {
			due = append(due, a)
		}
	}
	return due, nil
}

// MarkAlertsNotified records that the alerts ids were sent now.
func (s *Store) MarkAlertsNotified(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{alertStamp()}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(`UPDATE alerts SET notified_at=? WHERE public_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	return err
}
