package state

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// A site's databases and database accounts (SPEC.md "Site databases",
// migration 22). The store records what the panel asked for; the agent
// keeps its own record of which names belong to which site and refuses
// anything else (internal/agent/ops/db SPEC §2).

// ErrDBNameTaken is a database or account name another row already has:
// MariaDB has one namespace for each, across every site.
var ErrDBNameTaken = errors.New("the name is already in use on this server")

// SiteDatabase is one of a site's databases.
type SiteDatabase struct {
	ID     int64  `json:"id"`
	SiteID int64  `json:"site_id"`
	Name   string `json:"name"`
	// ServerID is the external server it is on (0: the box's own).
	ServerID  int64     `json:"server_id"`
	CreatedAt time.Time `json:"created_at"`
}

// DBGrant is an account's privileges on one database: "all" or "read".
type DBGrant struct {
	Database string `json:"database"`
	Level    string `json:"level"`
}

// DBUser is one of a site's database accounts. Secret is the name of the
// secret holding its password.
type DBUser struct {
	ID     int64  `json:"id"`
	SiteID int64  `json:"site_id"`
	Name   string `json:"name"`
	// ServerID is the external server it is on (0: the box's own).
	ServerID  int64     `json:"server_id"`
	Secret    string    `json:"-"`
	Grants    []DBGrant `json:"grants"`
	CreatedAt time.Time `json:"created_at"`
}

// DBUserSecret is the secret name of a new account's password.
func DBUserSecret(siteID int64, user string) string {
	return SiteSecret(siteID, "db_user."+user)
}

func (s *Store) databasesChanged(site int64, name string) {
	s.publish(events.DatabasesChanged, site, map[string]any{"name": name})
}

// AnnounceDatabases tells open pages that one of the site's databases
// changed on the server (a creation the agent finished).
func (s *Store) AnnounceDatabases(siteID int64, name string) { s.databasesChanged(siteID, name) }

// taken maps a UNIQUE failure on a name to ErrDBNameTaken.
func taken(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrDBNameTaken
	}
	return err
}

// ListSiteDatabases returns a site's databases by name.
func (s *Store) ListSiteDatabases(siteID int64) ([]SiteDatabase, error) {
	rows, err := s.db.Query(`SELECT id, site_id, name, server_id, created_at FROM site_databases WHERE site_id = ? ORDER BY name`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []SiteDatabase{}
	for rows.Next() {
		var d SiteDatabase
		var at string
		if err := rows.Scan(&d.ID, &d.SiteID, &d.Name, &d.ServerID, &at); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(at)
		list = append(list, d)
	}
	return list, rows.Err()
}

// SiteDatabaseByName reads one of a site's databases; ErrNotFound when the
// site has none of that name.
func (s *Store) SiteDatabaseByName(siteID int64, name string) (SiteDatabase, error) {
	var d SiteDatabase
	var at string
	err := s.db.QueryRow(`SELECT id, site_id, name, server_id, created_at FROM site_databases WHERE site_id = ? AND name = ?`, siteID, name).
		Scan(&d.ID, &d.SiteID, &d.Name, &d.ServerID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteDatabase{}, ErrNotFound
	}
	d.CreatedAt = parseTime(at)
	return d, err
}

// DBNameInUse reports whether any site has a database (or, users, an
// account) of this name.
func (s *Store) DBNameInUse(name string, users bool) (bool, error) {
	table := "site_databases"
	if users {
		table = "site_db_users"
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// AddSiteDatabase records a new database of the site. A name in use
// anywhere is ErrDBNameTaken.
func (s *Store) AddSiteDatabase(siteID int64, name string) (SiteDatabase, error) {
	return s.AddSiteDatabaseOn(siteID, name, 0)
}

// AddSiteDatabaseOn is AddSiteDatabase on an external server (0: the box's
// own).
func (s *Store) AddSiteDatabaseOn(siteID int64, name string, serverID int64) (SiteDatabase, error) {
	at := now()
	d := SiteDatabase{SiteID: siteID, Name: name, ServerID: serverID, CreatedAt: parseTime(at)}
	err := s.db.QueryRow(`INSERT INTO site_databases (site_id, name, server_id, created_at) VALUES (?, ?, ?, ?) RETURNING id`, siteID, name, serverID, at).Scan(&d.ID)
	if err != nil {
		return SiteDatabase{}, taken(err)
	}
	s.databasesChanged(siteID, name)
	return d, nil
}

// RemoveSiteDatabase forgets one of the site's databases and every grant
// on it; ErrNotFound when it has none of that name.
func (s *Store) RemoveSiteDatabase(siteID int64, name string) error {
	res, err := s.db.Exec(`DELETE FROM site_databases WHERE site_id = ? AND name = ?`, siteID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.databasesChanged(siteID, name)
	return nil
}

// ListDBUsers returns a site's database accounts by name, with their
// grants.
func (s *Store) ListDBUsers(siteID int64) ([]DBUser, error) {
	rows, err := s.db.Query(`SELECT id, site_id, name, secret, server_id, created_at FROM site_db_users WHERE site_id = ? ORDER BY name`, siteID)
	if err != nil {
		return nil, err
	}
	list := []DBUser{}
	byID := map[int64]int{}
	for rows.Next() {
		var u DBUser
		var at string
		if err := rows.Scan(&u.ID, &u.SiteID, &u.Name, &u.Secret, &u.ServerID, &at); err != nil {
			rows.Close()
			return nil, err
		}
		u.CreatedAt, u.Grants = parseTime(at), []DBGrant{}
		byID[u.ID] = len(list)
		list = append(list, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	g, err := s.db.Query(`SELECT g.user_id, d.name, g.level FROM site_db_grants g
		JOIN site_databases d ON d.id = g.database_id WHERE d.site_id = ? ORDER BY d.name`, siteID)
	if err != nil {
		return nil, err
	}
	defer g.Close()
	for g.Next() {
		var id int64
		var gr DBGrant
		if err := g.Scan(&id, &gr.Database, &gr.Level); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok {
			list[i].Grants = append(list[i].Grants, gr)
		}
	}
	return list, g.Err()
}

// DBUserSite returns the site a database account belongs to (names are
// unique across the server); ErrNotFound when no site has one of that
// name. Quarry's sign-in asks it (internal/api SPEC §6.4).
func (s *Store) DBUserSite(name string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT site_id FROM site_db_users WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// DBUserByName reads one of a site's accounts; ErrNotFound when it has none
// of that name.
func (s *Store) DBUserByName(siteID int64, name string) (DBUser, error) {
	users, err := s.ListDBUsers(siteID)
	if err != nil {
		return DBUser{}, err
	}
	for _, u := range users {
		if u.Name == name {
			return u, nil
		}
	}
	return DBUser{}, ErrNotFound
}

// AddDBUser records a new account of the site with its password (sealed
// under secret) and grants, all or nothing. A name in use anywhere is
// ErrDBNameTaken; a grant on a database the site does not have is an error.
func (s *Store) AddDBUser(siteID int64, name, secret, password string, grants []DBGrant) (DBUser, error) {
	return s.AddDBUserOn(siteID, name, secret, password, grants, 0)
}

// AddDBUserOn is AddDBUser on an external server (0: the box's own).
func (s *Store) AddDBUserOn(siteID int64, name, secret, password string, grants []DBGrant, serverID int64) (DBUser, error) {
	if !strings.HasPrefix(secret, SiteSecretPrefix(siteID)) {
		return DBUser{}, fmt.Errorf("the secret of a database account of site %d must be one of its secrets", siteID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return DBUser{}, err
	}
	defer tx.Rollback()
	at := now()
	var id int64
	if err := tx.QueryRow(`INSERT INTO site_db_users (site_id, name, secret, server_id, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		siteID, name, secret, serverID, at).Scan(&id); err != nil {
		return DBUser{}, taken(err)
	}
	if err := writeGrants(tx, siteID, id, grants); err != nil {
		return DBUser{}, err
	}
	if password != "" {
		if err := s.setSecret(tx, secret, password); err != nil {
			return DBUser{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DBUser{}, err
	}
	s.databasesChanged(siteID, name)
	return s.DBUserByName(siteID, name)
}

// writeGrants replaces an account's grants, refusing a database the site
// does not have.
func writeGrants(tx *sql.Tx, siteID, userID int64, grants []DBGrant) error {
	if _, err := tx.Exec(`DELETE FROM site_db_grants WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, g := range grants {
		if g.Level != "all" && g.Level != "read" {
			return fmt.Errorf("invalid grant level %q", g.Level)
		}
		var dbID int64
		err := tx.QueryRow(`SELECT id FROM site_databases WHERE site_id = ? AND name = ?`, siteID, g.Database).Scan(&dbID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: the site has no database %q", ErrNotFound, g.Database)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO site_db_grants (user_id, database_id, level) VALUES (?, ?, ?)`, userID, dbID, g.Level); err != nil {
			return err
		}
	}
	return nil
}

// SetDBUserGrants replaces an account's grants.
func (s *Store) SetDBUserGrants(siteID int64, name string, grants []DBGrant) (DBUser, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DBUser{}, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`SELECT id FROM site_db_users WHERE site_id = ? AND name = ?`, siteID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return DBUser{}, ErrNotFound
	}
	if err != nil {
		return DBUser{}, err
	}
	if err := writeGrants(tx, siteID, id, grants); err != nil {
		return DBUser{}, err
	}
	if err := tx.Commit(); err != nil {
		return DBUser{}, err
	}
	s.databasesChanged(siteID, name)
	return s.DBUserByName(siteID, name)
}

// SetDBUserPassword stores an account's new password.
func (s *Store) SetDBUserPassword(siteID int64, name, password string) error {
	u, err := s.DBUserByName(siteID, name)
	if err != nil {
		return err
	}
	if err := s.SetSecret(u.Secret, password); err != nil {
		return err
	}
	s.databasesChanged(siteID, name)
	return nil
}

// DBUserPassword reads an account's password ("" when none is stored).
func (s *Store) DBUserPassword(siteID int64, name string) (string, error) {
	u, err := s.DBUserByName(siteID, name)
	if err != nil {
		return "", err
	}
	return s.Secret(u.Secret)
}

// RemoveDBUser forgets one of the site's accounts, its grants and its
// password; ErrNotFound when it has none of that name.
func (s *Store) RemoveDBUser(siteID int64, name string) error {
	u, err := s.DBUserByName(siteID, name)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM site_db_users WHERE id = ?`, u.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM secrets WHERE name = ?`, u.Secret); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.databasesChanged(siteID, name)
	return nil
}

// SortGrants orders grants by database, for stable output.
func SortGrants(g []DBGrant) {
	sort.Slice(g, func(i, j int) bool { return g[i].Database < g[j].Database })
}
