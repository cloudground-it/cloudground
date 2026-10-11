package state

import (
	"database/sql"
	"errors"
	"fmt"
)

// DatabaseClaims is what the panel's state says each site's databases and
// database users are, read by `panel-agent adopt-databases` to rebuild the
// agent's lost record (internal/agent/ops/db SPEC §2). It is the panel's
// word, not the agent's: the agent adopts only what the server has and what
// passes its own checks.
type DatabaseClaims struct {
	Databases []DatabaseClaim
	Users     []DBUserClaim
}

// DatabaseClaim is one database a site claims.
type DatabaseClaim struct {
	SiteID int64
	Name   string
}

// DBUserClaim is one user a site claims, with its grants.
type DBUserClaim struct {
	SiteID int64
	Name   string
	Grants []DBGrant
}

// ReadDatabaseClaims opens the state database at path read-only (no
// migration, no write, no secret: the store's Open would migrate it) and
// reads every site's databases and users. A state from before migration
// 22 has no such tables: start panel-api once to migrate it.
func ReadDatabaseClaims(path string) (DatabaseClaims, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return DatabaseClaims{}, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('site_databases', 'site_db_users', 'site_db_grants')`).Scan(&n); err != nil {
		return DatabaseClaims{}, fmt.Errorf("read the panel's state %s: %w", path, err)
	}
	if n != 3 {
		return DatabaseClaims{}, errors.New("the panel's state has no record of the sites' databases yet (before migration 22): start panel-api once, then run this again")
	}
	var out DatabaseClaims
	rows, err := db.Query(`SELECT site_id, name FROM site_databases ORDER BY name`)
	if err != nil {
		return DatabaseClaims{}, err
	}
	for rows.Next() {
		var c DatabaseClaim
		if err := rows.Scan(&c.SiteID, &c.Name); err != nil {
			rows.Close()
			return DatabaseClaims{}, err
		}
		out.Databases = append(out.Databases, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DatabaseClaims{}, err
	}
	users, err := db.Query(`SELECT id, site_id, name FROM site_db_users ORDER BY name`)
	if err != nil {
		return DatabaseClaims{}, err
	}
	byID := map[int64]int{}
	for users.Next() {
		var id int64
		var c DBUserClaim
		if err := users.Scan(&id, &c.SiteID, &c.Name); err != nil {
			users.Close()
			return DatabaseClaims{}, err
		}
		byID[id] = len(out.Users)
		out.Users = append(out.Users, c)
	}
	users.Close()
	if err := users.Err(); err != nil {
		return DatabaseClaims{}, err
	}
	grants, err := db.Query(`SELECT g.user_id, d.name, g.level FROM site_db_grants g JOIN site_databases d ON d.id = g.database_id ORDER BY d.name`)
	if err != nil {
		return DatabaseClaims{}, err
	}
	defer grants.Close()
	for grants.Next() {
		var id int64
		var g DBGrant
		if err := grants.Scan(&id, &g.Database, &g.Level); err != nil {
			return DatabaseClaims{}, err
		}
		if i, ok := byID[id]; ok {
			out.Users[i].Grants = append(out.Users[i].Grants, g)
		}
	}
	return out, grants.Err()
}
