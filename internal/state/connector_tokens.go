package state

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

// Connector tokens are found by hash (SPEC.md invariant 7): every
// WordPress post save sends one, and reading the site behind it must not
// mean decrypting every site's token.

// secretLookup is the lookup column of a secret: the hex SHA-256 of the
// value for a site's connector token, NULL for every other secret.
func secretLookup(name, value string) any {
	if !strings.HasPrefix(name, secretSiteName) || !strings.HasSuffix(name, "."+SiteSecretConnectorToken) {
		return nil
	}
	return connectorTokenHash(value)
}

func connectorTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SiteForConnectorToken returns the site whose connector token is token:
// one indexed read, then the stored token compared in constant time.
// ErrNotFound when no site has it.
func (s *Store) SiteForConnectorToken(token string) (Site, error) {
	if s.key == nil {
		return Site{}, ErrNoSecretKey
	}
	if token == "" {
		return Site{}, ErrNotFound
	}
	var (
		name   string
		sealed []byte
		siteID sql.NullInt64
	)
	err := s.db.QueryRow(`SELECT name, sealed, site_id FROM secrets WHERE lookup=?`, connectorTokenHash(token)).
		Scan(&name, &sealed, &siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, ErrNotFound
	}
	if err != nil {
		return Site{}, err
	}
	stored, err := s.open(name, sealed)
	if err != nil {
		return Site{}, err
	}
	if !siteID.Valid || subtle.ConstantTimeCompare([]byte(stored), []byte(token)) != 1 {
		return Site{}, ErrNotFound
	}
	return s.GetSite(siteID.Int64)
}

// indexConnectorTokens fills the lookup of every connector token sealed
// before the column existed. It needs the key, so it runs at Open after the
// secrets moved; a second run finds nothing to do.
func (s *Store) indexConnectorTokens() error {
	rows, err := s.db.Query(`SELECT name, sealed FROM secrets WHERE lookup IS NULL AND name LIKE 'site.%.' || ?`, SiteSecretConnectorToken)
	if err != nil {
		return err
	}
	type entry struct {
		name   string
		sealed []byte
	}
	var todo []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.name, &e.sealed); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range todo {
		value, err := s.open(e.name, e.sealed)
		if err != nil {
			return err
		}
		// OR IGNORE: two sites with one token (never minted so) would
		// otherwise stop the panel from opening; the second is left
		// unindexed, and its purges are refused.
		if _, err := s.db.Exec(`UPDATE OR IGNORE secrets SET lookup=? WHERE name=?`, secretLookup(e.name, value), e.name); err != nil {
			return err
		}
	}
	return nil
}
