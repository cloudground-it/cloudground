package rpc

import (
	"fmt"

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
}

// Sites lists the site whose database it is.
func (p *SiteDatabaseParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate refuses a name the site may not take: reserved, a system name,
// another site's default.
func (p *SiteDatabaseParams) Validate() error { return site.ClaimableBy(p.Name, p.Site.ID) }

// DBUserParams creates one of a site's database accounts, changes its
// password or its grants, or drops it. Password is the new password
// (create, password; never logged). Grants are the account's privileges
// on the site's own databases, replacing what it had (create, grants).
type DBUserParams struct {
	Site     state.Site      `json:"site"`
	User     string          `json:"user"`
	Password string          `json:"password,omitempty"`
	Grants   []state.DBGrant `json:"grants,omitempty"`
}

// Sites lists the site whose account it is.
func (p *DBUserParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate refuses a name the site may not take and a grant level other
// than all or read.
func (p *DBUserParams) Validate() error {
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
	SizeBytes int64  `json:"size_bytes"`
	Tables    int64  `json:"tables"`
}

// DBUserInfo is one account and its grants.
type DBUserInfo struct {
	Name   string          `json:"name"`
	Grants []state.DBGrant `json:"grants"`
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
}

// Sites lists the site imported into.
func (p *DBImportParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// DBImportResult names the file imported and its size. Warnings are what
// the agent noticed in the file before importing it (statements that read
// or write a file, which the server refuses).
type DBImportResult struct {
	Imported  string   `json:"imported"`
	SizeBytes int64    `json:"size_bytes"`
	Warnings  []string `json:"warnings,omitempty"`
}
