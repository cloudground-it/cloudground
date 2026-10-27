package state

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// External database servers (#30, api SPEC §6.5.1): other MariaDB or MySQL
// servers a site's databases may be on. Server 0 is the box's own and has no
// row; a server's administrator password is the sealed secret
// DBServerSecret.

// DBServer is a registered external server.
type DBServer struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Kind is "mariadb" (MySQL servers too).
	Kind string `json:"kind"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// TLS is "required" or "off".
	TLS       string `json:"tls"`
	CACert    string `json:"ca_cert"`
	AdminUser string `json:"admin_user"`
	// Databases and Users count what is on the server (list only).
	Databases int `json:"databases"`
	Users     int `json:"-"`
}

// Server TLS settings.
const (
	DBServerTLSRequired = "required"
	DBServerTLSOff      = "off"
)

// ErrDBServerInUse is a server a database or an account is still on.
var ErrDBServerInUse = errors.New("databases are still on this server")

// ErrDBServerNameTaken is a name another server has.
var ErrDBServerNameTaken = errors.New("a database server of that name exists")

// DBServerSecret names the secret holding a server's administrator
// password.
func DBServerSecret(id int64) string {
	return "dbserver." + strconv.FormatInt(id, 10) + ".admin_password"
}

const dbServerCols = `id, name, kind, host, port, tls, ca_cert, admin_user`

func scanDBServer(row interface{ Scan(...any) error }) (DBServer, error) {
	var d DBServer
	err := row.Scan(&d.ID, &d.Name, &d.Kind, &d.Host, &d.Port, &d.TLS, &d.CACert, &d.AdminUser)
	return d, err
}

// CreateDBServer registers a server with its password (sealed).
func (s *Store) CreateDBServer(d DBServer, password string) (DBServer, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DBServer{}, err
	}
	defer tx.Rollback()
	if d.Kind == "" {
		d.Kind = "mariadb"
	}
	err = tx.QueryRow(`INSERT INTO db_servers (name, kind, host, port, tls, ca_cert, admin_user, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		d.Name, d.Kind, d.Host, d.Port, d.TLS, d.CACert, d.AdminUser, now()).Scan(&d.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return DBServer{}, ErrDBServerNameTaken
		}
		return DBServer{}, err
	}
	if err := s.setSecret(tx, DBServerSecret(d.ID), password); err != nil {
		return DBServer{}, err
	}
	return d, tx.Commit()
}

// GetDBServer reads one; ErrNotFound when there is none.
func (s *Store) GetDBServer(id int64) (DBServer, error) {
	d, err := scanDBServer(s.db.QueryRow(`SELECT `+dbServerCols+` FROM db_servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return DBServer{}, ErrNotFound
	}
	return d, err
}

// ListDBServers returns every server by name, with how many databases and
// accounts are on each.
func (s *Store) ListDBServers() ([]DBServer, error) {
	rows, err := s.db.Query(`SELECT ` + dbServerCols + `,
		(SELECT COUNT(*) FROM site_databases WHERE server_id = db_servers.id),
		(SELECT COUNT(*) FROM site_db_users WHERE server_id = db_servers.id) FROM db_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []DBServer{}
	for rows.Next() {
		var d DBServer
		if err := rows.Scan(&d.ID, &d.Name, &d.Kind, &d.Host, &d.Port, &d.TLS, &d.CACert, &d.AdminUser, &d.Databases, &d.Users); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// DBServerPassword reads a server's administrator password.
func (s *Store) DBServerPassword(id int64) (string, error) {
	return s.Secret(DBServerSecret(id))
}

// UpdateDBServer changes a server; password "" keeps the stored one.
func (s *Store) UpdateDBServer(d DBServer, password string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE db_servers SET name = ?, host = ?, port = ?, tls = ?, ca_cert = ?, admin_user = ? WHERE id = ?`,
		d.Name, d.Host, d.Port, d.TLS, d.CACert, d.AdminUser, d.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDBServerNameTaken
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if password != "" {
		if err := s.setSecret(tx, DBServerSecret(d.ID), password); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteDBServer forgets a server and its password; ErrDBServerInUse while
// a database or an account is on it, ErrNotFound when there is none.
func (s *Store) DeleteDBServer(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM site_databases WHERE server_id = ?) + (SELECT COUNT(*) FROM site_db_users WHERE server_id = ?)`, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrDBServerInUse
	}
	res, err := tx.Exec(`DELETE FROM db_servers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM secrets WHERE name = ?`, DBServerSecret(id)); err != nil {
		return err
	}
	return tx.Commit()
}
