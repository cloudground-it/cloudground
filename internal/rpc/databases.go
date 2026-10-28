package rpc

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// SiteDatabaseParams creates, or drops, one of a site's databases (Fase
// 9b). The name must be one the site may take (internal/site ClaimableBy);
// the agent also requires a database it drops to be recorded as the
// site's (ops/db SPEC §2).
type SiteDatabaseParams struct {
	Site state.Site `json:"site"`
	Name string     `json:"name"`
	// ServerID is the external server a new database is made on; 0 is the
	// box's own (#30, ops/db SPEC §9). A drop finds the server from the
	// agent's record.
	ServerID int64 `json:"server_id,omitempty"`
}

// Sites lists the site whose database it is.
func (p *SiteDatabaseParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate refuses a name the site may not take: reserved, a system name,
// another site's default.
func (p *SiteDatabaseParams) Validate() error {
	if p.ServerID < 0 {
		return errors.New("invalid database server")
	}
	return site.ClaimableBy(p.Name, p.Site.ID)
}

// DBUserParams creates one of a site's database accounts, changes its
// password or its grants, or drops it. Password is the new password
// (create, password; never logged). Grants are the account's privileges
// on the site's own databases, replacing what it had (create, grants).
type DBUserParams struct {
	Site     state.Site      `json:"site"`
	User     string          `json:"user"`
	Password string          `json:"password,omitempty"`
	Grants   []state.DBGrant `json:"grants,omitempty"`
	// ServerID is the external server a new account is made on (0: the
	// box's own); every later call finds it from the agent's record.
	ServerID int64 `json:"server_id,omitempty"`
}

// Sites lists the site whose account it is.
func (p *DBUserParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate refuses a name the site may not take and a grant level other
// than all or read.
func (p *DBUserParams) Validate() error {
	if p.ServerID < 0 {
		return errors.New("invalid database server")
	}
	if err := site.ClaimableBy(p.User, p.Site.ID); err != nil {
		return err
	}
	for _, g := range p.Grants {
		if !site.ValidGrant(g.Level) {
			return fmt.Errorf("invalid grant level %q", g.Level)
		}
		if err := site.ClaimableBy(g.Database, p.Site.ID); err != nil {
			return err
		}
	}
	return nil
}

// DBListParams lists a site's databases and accounts as the agent records
// them.
type DBListParams struct {
	Site state.Site `json:"site"`
}

// Sites lists the site listed.
func (p *DBListParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// DBListResult is a site's databases with their size, and its accounts
// with their grants. StatsError is why the sizes could not be read (they
// are then -1).
type DBListResult struct {
	Databases  []DBInfo     `json:"databases"`
	Users      []DBUserInfo `json:"users"`
	StatsError string       `json:"stats_error,omitempty"`
}

// DBInfo is one database: its size (data and indexes) and its tables.
type DBInfo struct {
	Name      string `json:"name"`
	ServerID  int64  `json:"server_id,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Tables    int64  `json:"tables"`
}

// DBUserInfo is one account and its grants.
type DBUserInfo struct {
	Name     string          `json:"name"`
	ServerID int64           `json:"server_id,omitempty"`
	Grants   []state.DBGrant `json:"grants"`
}

// DBQueryParams runs one SQL statement on a site's own database, through a
// throwaway account granted that database only. ReadOnly grants SELECT and
// SHOW VIEW alone, so MariaDB itself refuses a write.
type DBQueryParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database"`
	SQL      string     `json:"sql"`
	ReadOnly bool       `json:"read_only"`
}

// Sites lists the site queried.
func (p *DBQueryParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// DBQueryResult is the statement's rows, or its Message when it returns
// none. Truncated says rows were left out at the console's limit.
type DBQueryResult struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	Message   string     `json:"message,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// DBTablesParams lists the tables of one of a site's databases ("" is its
// primary).
type DBTablesParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
}

// Sites lists the site whose tables are listed.
func (p *DBTablesParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// DBTablesResult lists the tables, the largest first.
type DBTablesResult struct {
	Database  string    `json:"database"`
	Tables    []DBTable `json:"tables"`
	Truncated bool      `json:"truncated,omitempty"`
}

// DBTable is one table and its size. For InnoDB, Rows is MariaDB's
// estimate.
type DBTable struct {
	Name       string `json:"name"`
	Rows       int64  `json:"rows"`
	DataBytes  int64  `json:"data_bytes"`
	IndexBytes int64  `json:"index_bytes"`
	Engine     string `json:"engine,omitempty"`
	Collation  string `json:"collation,omitempty"`
}

// DBExportParams dumps a site's database to a file inside the site, for
// download.
type DBExportParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database"`
}

// Sites lists the site exported.
func (p *DBExportParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// DBExportResult is the dump written.
type DBExportResult struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// DBImportParams imports a SQL file already uploaded into the site's tree
// (RelPath, relative to its root), so a large dump never crosses the
// socket.
type DBImportParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database"`
	RelPath  string     `json:"rel_path"`
	// Accounts carries the site's database-account passwords (user →
	// password) for a database on an external server, where the agent
	// cannot borrow the account (ops/db SR-T7). The agent uses one only
	// for the account its own record picks; empty for a database on the
	// box's own server.
	Accounts map[string]string `json:"accounts,omitempty"`
}

// Sites lists the site imported into.
func (p *DBImportParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate bounds the import accounts (RelPath and Database are checked by
// the agent against its own record).
func (p *DBImportParams) Validate() error { return CheckImportAccounts(p.Accounts) }

// MaxImportAccounts bounds how many account passwords a request carries.
const MaxImportAccounts = 64

// CheckImportAccounts refuses an account map that is too large, names an
// account that is not a database user's, or holds a password with a NUL or
// a line break (it would go into a defaults file).
func CheckImportAccounts(accounts map[string]string) error {
	if len(accounts) > MaxImportAccounts {
		return errors.New("too many database accounts")
	}
	for user, pw := range accounts {
		if !site.ValidDBName(user) {
			return errors.New("invalid database account name " + strconvQuote(user))
		}
		if len(pw) > 256 || strings.ContainsAny(pw, "\x00\n\r") {
			return errors.New("a database account password is too long or has a line break")
		}
	}
	return nil
}

// DBImportResult names the file imported and its size. Warnings are what
// the agent noticed in the file before importing it (statements that read
// or write a file, which the server refuses).
type DBImportResult struct {
	Imported  string   `json:"imported"`
	SizeBytes int64    `json:"size_bytes"`
	Warnings  []string `json:"warnings,omitempty"`
}
