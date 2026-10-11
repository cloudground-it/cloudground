package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudground-it/cloudground/internal/blocklist"
)

// The server's block list (#21, migration 25; internal/blocklist SPEC):
// one row per blocked or allowed prefix, and the automatic blocking's
// settings.

// Kinds of a block list row.
const (
	BlockKindBlock = "block"
	BlockKindAllow = "allow"
)

// BlockEntry is one row: a prefix (an address is its /32 or /128) blocked
// or allowed, who put it there (a failure source, or "manual"), the
// operator's note, the failures that blocked it, and when an automatic
// block ends (nil: never).
type BlockEntry struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	Prefix    string     `json:"address"`
	Source    string     `json:"source"`
	Note      string     `json:"note"`
	Failures  int        `json:"failures,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// blockStamp is t in a fixed-width form, so the expiry times sort as text.
func blockStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }

// BlockEntries returns every row, oldest first.
func (s *Store) BlockEntries() ([]BlockEntry, error) {
	rows, err := s.db.Query(`SELECT id, kind, prefix, source, note, failures, created_at, expires_at FROM blocklist ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockEntry{}
	for rows.Next() {
		var e BlockEntry
		var created string
		var expires sql.NullString
		if err := rows.Scan(&e.ID, &e.Kind, &e.Prefix, &e.Source, &e.Note, &e.Failures, &created, &expires); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		e.ExpiresAt = parseTimePtr(expires)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PutBlockEntry adds e, or replaces the row of the same kind and prefix
// (an address blocked again gets the new source, note and expiry). It
// returns the row as stored.
func (s *Store) PutBlockEntry(e BlockEntry) (BlockEntry, error) {
	if e.Kind != BlockKindBlock && e.Kind != BlockKindAllow {
		return BlockEntry{}, fmt.Errorf("block list kind %q", e.Kind)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	var expires any
	if e.ExpiresAt != nil {
		expires = blockStamp(*e.ExpiresAt)
	}
	err := s.db.QueryRow(`INSERT INTO blocklist (kind, prefix, source, note, failures, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, prefix) DO UPDATE SET source=excluded.source, note=excluded.note,
			failures=excluded.failures, created_at=excluded.created_at, expires_at=excluded.expires_at
		RETURNING id`,
		e.Kind, e.Prefix, e.Source, e.Note, e.Failures, blockStamp(e.CreatedAt), expires).Scan(&e.ID)
	if err != nil {
		return BlockEntry{}, err
	}
	e.CreatedAt = parseTime(blockStamp(e.CreatedAt))
	return e, nil
}

// DeleteBlockEntry removes the row of kind and prefix and reports whether
// there was one.
func (s *Store) DeleteBlockEntry(kind, prefix string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM blocklist WHERE kind = ? AND prefix = ?`, kind, prefix)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PruneBlockEntries removes the blocks that ended at or before now and
// returns how many.
func (s *Store) PruneBlockEntries(now time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM blocklist WHERE expires_at IS NOT NULL AND expires_at <= ?`, blockStamp(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// BlocklistSettings returns the automatic blocking's settings; the
// defaults until they were saved once.
func (s *Store) BlocklistSettings() (blocklist.Settings, error) {
	var raw string
	err := s.db.QueryRow(`SELECT settings FROM blocklist_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return blocklist.DefaultSettings(), nil
	}
	if err != nil {
		return blocklist.Settings{}, err
	}
	out := blocklist.DefaultSettings()
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return blocklist.Settings{}, fmt.Errorf("blocklist settings: %w", err)
	}
	return out, nil
}

// SetBlocklistSettings saves the settings.
func (s *Store) SetBlocklistSettings(v blocklist.Settings) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO blocklist_settings (id, settings) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET settings = excluded.settings`, string(raw))
	return err
}
