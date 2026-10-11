package rpc

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Quarry's parameters and results (internal/agent/ops/dbstudio/SPEC.md).

// DBStudioTablesParams lists the site's tables; Columns adds every
// table's column names.
type DBStudioTablesParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login   string `json:"login,omitempty"`
	Columns bool   `json:"columns,omitempty"`
}

// DBStudioTable is one table or view.
type DBStudioTable struct {
	Name          string `json:"name"`
	Type          string `json:"type"` // table | view
	Engine        string `json:"engine,omitempty"`
	Collation     string `json:"collation,omitempty"`
	Rows          int64  `json:"rows"`
	DataBytes     int64  `json:"data_bytes"`
	IndexBytes    int64  `json:"index_bytes"`
	AutoIncrement int64  `json:"auto_increment,omitempty"`
	Comment       string `json:"comment,omitempty"`
}

// DBStudioTablesResult lists the tables by name, and the stored
// procedures and functions (shown, never run or changed).
type DBStudioTablesResult struct {
	Database string              `json:"database"`
	Tables   []DBStudioTable     `json:"tables"`
	Routines []DBRoutine         `json:"routines,omitempty"`
	Columns  map[string][]string `json:"columns,omitempty"`
}

// DBRoutine is a stored procedure or function.
type DBRoutine struct {
	Name string `json:"name"`
	Type string `json:"type"` // procedure | function
}

// DBStudioTableParams names one table.
type DBStudioTableParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login string `json:"login,omitempty"`
	Table string `json:"table"`
}

// DBColumn is one column of a table.
type DBColumn struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`      // the full column type, e.g. varchar(191)
	DataType  string  `json:"data_type"` // e.g. varchar
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"`
	Key       string  `json:"key,omitempty"` // PRI | UNI | MUL
	Extra     string  `json:"extra,omitempty"`
	Collation string  `json:"collation,omitempty"`
	Comment   string  `json:"comment,omitempty"`
	Binary    bool    `json:"binary,omitempty"`
}

// DBIndexColumn is a column of an index, with its prefix length (0: whole).
type DBIndexColumn struct {
	Name   string `json:"name"`
	Length int    `json:"length,omitempty"`
}

// DBIndex is one index of a table.
type DBIndex struct {
	Name    string          `json:"name"`
	Unique  bool            `json:"unique"`
	Type    string          `json:"type,omitempty"`
	Columns []DBIndexColumn `json:"columns"`
}

// DBForeignKey is one foreign key of a table.
type DBForeignKey struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
	OnUpdate   string   `json:"on_update,omitempty"`
	OnDelete   string   `json:"on_delete,omitempty"`
}

// DBStudioStructure is a table's structure.
type DBStudioStructure struct {
	Table       string         `json:"table"`
	Type        string         `json:"type"`
	Columns     []DBColumn     `json:"columns"`
	Indexes     []DBIndex      `json:"indexes"`
	ForeignKeys []DBForeignKey `json:"foreign_keys"`
	PrimaryKey  []string       `json:"primary_key"`
	CreateSQL   string         `json:"create_sql"`
}

// DBSort orders rows by a column.
type DBSort struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc,omitempty"`
}

// DBFilter keeps the rows whose column satisfies Op (eq ne lt le gt ge
// contains starts null notnull) with Value.
type DBFilter struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  string `json:"value,omitempty"`
}

// DBStudioRowsParams reads a page of a table.
type DBStudioRowsParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login    string     `json:"login,omitempty"`
	Table    string     `json:"table"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Sort     []DBSort   `json:"sort,omitempty"`
	Filters  []DBFilter `json:"filters,omitempty"`
	// Search keeps the rows where any text column contains it (SPEC §4).
	Search string `json:"search,omitempty"`
}

// DBRow is one row: its values (nil is NULL) and the indexes of the cells
// that were cut.
type DBRow struct {
	Values []*string `json:"v"`
	Cut    []int     `json:"cut,omitempty"`
}

// DBStudioRowsResult is a page of rows.
type DBStudioRowsResult struct {
	Columns        []DBColumn `json:"columns"`
	PrimaryKey     []string   `json:"primary_key"`
	Rows           []DBRow    `json:"rows"`
	Total          int64      `json:"total"`
	TotalEstimated bool       `json:"total_estimated,omitempty"`
	Page           int        `json:"page"`
	PageSize       int        `json:"page_size"`
	Truncated      bool       `json:"truncated,omitempty"`
	SQL            string     `json:"sql"`
}

// DBChange is one row change: insert (Values), update (Key, Values) or
// delete (Key). A nil value is NULL.
type DBChange struct {
	Op     string             `json:"op"`
	Key    map[string]string  `json:"key,omitempty"`
	Values map[string]*string `json:"values,omitempty"`
}

// DBStudioEditParams changes rows of a table in one transaction. DryRun
// returns the SQL without running it; ReadOnly is a caller who may not
// write.
type DBStudioEditParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login    string     `json:"login,omitempty"`
	Table    string     `json:"table"`
	Changes  []DBChange `json:"changes"`
	DryRun   bool       `json:"dry_run,omitempty"`
	ReadOnly bool       `json:"read_only,omitempty"`
}

// DBStudioEditResult is the SQL built (and run, when Executed).
type DBStudioEditResult struct {
	SQL      []string `json:"sql"`
	Affected int64    `json:"affected"`
	Executed bool     `json:"executed"`
}

// DBNewColumn is a column to add.
type DBNewColumn struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Length   int     `json:"length,omitempty"`
	Scale    int     `json:"scale,omitempty"`
	Unsigned bool    `json:"unsigned,omitempty"`
	Nullable bool    `json:"nullable,omitempty"`
	Default  *string `json:"default,omitempty"`
	After    string  `json:"after,omitempty"`
}

// DBNewIndex is an index to add.
type DBNewIndex struct {
	Name    string          `json:"name"`
	Unique  bool            `json:"unique,omitempty"`
	Columns []DBIndexColumn `json:"columns"`
}

// DBStudioAlterParams adds a column (Kind add_column) or an index
// (add_index) to a table.
type DBStudioAlterParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login  string       `json:"login,omitempty"`
	Table  string       `json:"table"`
	Kind   string       `json:"kind"`
	Column *DBNewColumn `json:"column,omitempty"`
	Index  *DBNewIndex  `json:"index,omitempty"`
	// Confirm is the table's name, typed: truncate and drop need it.
	Confirm  string `json:"confirm,omitempty"`
	DryRun   bool   `json:"dry_run,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// DBStudioAlterResult is the ALTER built (and run, when Executed).
type DBStudioAlterResult struct {
	SQL      string `json:"sql"`
	Executed bool   `json:"executed"`
}

// DBStudioExportParams exports the database (sql, optionally some tables)
// or one table (csv) into shared/exports.
type DBStudioExportParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login  string   `json:"login,omitempty"`
	Format string   `json:"format"`
	Tables []string `json:"tables,omitempty"`
}

// DBStudioExportResult is the file written.
type DBStudioExportResult struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Rows      int64  `json:"rows,omitempty"`
}

// DBStudioFile is an export or an upload.
type DBStudioFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// DBStudioSiteParams names only the site.
type DBStudioSiteParams struct {
	Site state.Site `json:"site"`
}

// DBStudioFilesResult lists shared/exports and shared/imports.
type DBStudioFilesResult struct {
	Exports []DBStudioFile `json:"exports"`
	Imports []DBStudioFile `json:"imports"`
}

// DBStudioChunkParams reads Length bytes of an export at Offset.
type DBStudioChunkParams struct {
	Site   state.Site `json:"site"`
	Name   string     `json:"name"`
	Offset int64      `json:"offset"`
	Length int        `json:"length"`
}

// DBStudioChunkResult is the bytes read and the file's size.
type DBStudioChunkResult struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"`
}

// DBStudioUploadParams writes Data at Offset of shared/imports/Name: 0
// starts the file anew, anything else must be its current size.
type DBStudioUploadParams struct {
	Site   state.Site `json:"site"`
	Name   string     `json:"name"`
	Offset int64      `json:"offset"`
	Data   []byte     `json:"data"`
}

// DBStudioUploadResult is the file's size after the write.
type DBStudioUploadResult struct {
	Size int64 `json:"size"`
}

// DBStudioDeleteParams removes a file of shared/exports or shared/imports
// (Dir exports | imports).
type DBStudioDeleteParams struct {
	Site state.Site `json:"site"`
	Dir  string     `json:"dir"`
	Name string     `json:"name"`
}

// DBStudioImportCSVParams loads shared/imports/Name into Table.
type DBStudioImportCSVParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	// Login, when set, is a signed-in database user's Quarry session
	// (SPEC §11): the statements run as that user, not as a scoped account.
	Login    string `json:"login,omitempty"`
	Table    string `json:"table"`
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// DBStudioImportCSVResult counts the rows inserted.
type DBStudioImportCSVResult struct {
	Rows int64 `json:"rows"`
}

// DBAutoloadOption is one autoloaded option.
type DBAutoloadOption struct {
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"`
	Autoload string `json:"autoload"`
}

// DBStudioAutoloadResult is the size of what WordPress loads on every
// request.
type DBStudioAutoloadResult struct {
	Table string             `json:"table"`
	Count int64              `json:"count"`
	Bytes int64              `json:"bytes"`
	Top   []DBAutoloadOption `json:"top"`
}

// DBStudioSearchReplaceParams is a serialized-safe search and replace.
type DBStudioSearchReplaceParams struct {
	Site     state.Site `json:"site"`
	From     string     `json:"from"`
	To       string     `json:"to"`
	DryRun   bool       `json:"dry_run,omitempty"`
	ReadOnly bool       `json:"read_only,omitempty"`
}

// DBStudioSearchReplaceResult counts the replacements (that would be)
// made.
type DBStudioSearchReplaceResult struct {
	Replacements int64 `json:"replacements"`
	Executed     bool  `json:"executed"`
}

// DBStudioTransientsParams deletes (or, DryRun, counts) expired transients.
type DBStudioTransientsParams struct {
	Site     state.Site `json:"site"`
	DryRun   bool       `json:"dry_run,omitempty"`
	ReadOnly bool       `json:"read_only,omitempty"`
}

// DBStudioTransientsResult counts the expired transients and those deleted.
type DBStudioTransientsResult struct {
	Expired  int64 `json:"expired"`
	Deleted  int64 `json:"deleted"`
	Executed bool  `json:"executed"`
}

func (p *DBStudioTablesParams) Sites() []*state.Site        { return []*state.Site{&p.Site} }
func (p *DBStudioOpenParams) Sites() []*state.Site          { return []*state.Site{&p.Site} }
func (p *DBStudioLoginParams) Sites() []*state.Site         { return []*state.Site{&p.Site} }
func (p *DBStudioQueryParams) Sites() []*state.Site         { return []*state.Site{&p.Site} }
func (p *DBStudioSimulateParams) Sites() []*state.Site      { return []*state.Site{&p.Site} }
func (p *DBStudioSearchParams) Sites() []*state.Site        { return []*state.Site{&p.Site} }
func (p *DBStudioImportSQLParams) Sites() []*state.Site     { return []*state.Site{&p.Site} }
func (p *DBStudioTableParams) Sites() []*state.Site         { return []*state.Site{&p.Site} }
func (p *DBStudioRowsParams) Sites() []*state.Site          { return []*state.Site{&p.Site} }
func (p *DBStudioEditParams) Sites() []*state.Site          { return []*state.Site{&p.Site} }
func (p *DBStudioAlterParams) Sites() []*state.Site         { return []*state.Site{&p.Site} }
func (p *DBStudioExportParams) Sites() []*state.Site        { return []*state.Site{&p.Site} }
func (p *DBStudioSiteParams) Sites() []*state.Site          { return []*state.Site{&p.Site} }
func (p *DBStudioChunkParams) Sites() []*state.Site         { return []*state.Site{&p.Site} }
func (p *DBStudioUploadParams) Sites() []*state.Site        { return []*state.Site{&p.Site} }
func (p *DBStudioDeleteParams) Sites() []*state.Site        { return []*state.Site{&p.Site} }
func (p *DBStudioImportCSVParams) Sites() []*state.Site     { return []*state.Site{&p.Site} }
func (p *DBStudioSearchReplaceParams) Sites() []*state.Site { return []*state.Site{&p.Site} }
func (p *DBStudioTransientsParams) Sites() []*state.Site    { return []*state.Site{&p.Site} }

// Validate requires sql or csv, and exactly one table for csv.
func (p *DBStudioExportParams) Validate() error {
	if p.Format != "sql" && p.Format != "csv" {
		return errors.New("the format is sql or csv")
	}
	if p.Format == "csv" && len(p.Tables) != 1 {
		return errors.New("a CSV export is of one table")
	}
	return nil
}

// Validate requires one of the two folders.
func (p *DBStudioDeleteParams) Validate() error {
	if p.Dir != "exports" && p.Dir != "imports" {
		return errors.New("the folder is exports or imports")
	}
	return nil
}

// Validate requires a kind with its definition; truncate and drop need
// the table's name typed in Confirm.
func (p *DBStudioAlterParams) Validate() error {
	switch {
	case (p.Kind == "add_column" && p.Column != nil) || (p.Kind == "add_index" && p.Index != nil):
		return nil
	case p.Kind == "truncate" || p.Kind == "drop":
		if p.Confirm != p.Table {
			return errors.New("type the table's name to confirm")
		}
		return nil
	}
	return errors.New("give add_column with a column, add_index with an index, or truncate or drop")
}

// Signed-in database users (Fase 9b, ops/dbstudio SPEC §11).

// DBStudioOpenParams signs one of the site's database users in to DB
// Studio with its own password.
type DBStudioOpenParams struct {
	Site     state.Site `json:"site"`
	User     string     `json:"user"`
	Password string     `json:"password"`
}

// DBStudioOpenResult is the session's handle and the databases the user
// may see, or Refused: the server did not take the name and password.
type DBStudioOpenResult struct {
	Login     string   `json:"login,omitempty"`
	Refused   bool     `json:"refused,omitempty"`
	Databases []string `json:"databases,omitempty"`
}

// DBStudioLoginParams names a signed-in user's session: to list what it
// may see, or to end it.
type DBStudioLoginParams struct {
	Site  state.Site `json:"site"`
	Login string     `json:"login"`
}

// DBStudioDatabasesResult is the databases a signed-in user may see.
type DBStudioDatabasesResult struct {
	Databases []string `json:"databases"`
}

// DBStudioQueryParams is the SQL editor's statement: as the signed-in
// user (Login), or as a scoped account, read-only unless writes were
// allowed.
type DBStudioQueryParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	Login    string     `json:"login,omitempty"`
	SQL      string     `json:"sql"`
	ReadOnly bool       `json:"read_only,omitempty"`
}

// DBStudioSimulateParams is a write statement of the SQL editor to run in a
// transaction that is always rolled back (ops/dbstudio SPEC §12): as the
// signed-in user (Login), or as a scoped account, read-only when the
// caller may not write.
type DBStudioSimulateParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	Login    string     `json:"login,omitempty"`
	SQL      string     `json:"sql"`
	ReadOnly bool       `json:"read_only,omitempty"`
}

// DBStudioSimulateResult is what a simulated statement would have done:
// the rows it matched (Affected), up to 25 of the rows an UPDATE or DELETE
// would change as they are now (Preview, from PreviewSQL), or why it was
// not run (Refused) or what the server answered (Message). RolledBack is
// whether the transaction was seen rolled back; a run never commits.
type DBStudioSimulateResult struct {
	Kind       string         `json:"kind,omitempty"`
	Affected   int64          `json:"affected"`
	Preview    *DBQueryResult `json:"preview,omitempty"`
	PreviewSQL string         `json:"preview_sql,omitempty"`
	Refused    string         `json:"refused,omitempty"`
	Message    string         `json:"message,omitempty"`
	RolledBack bool           `json:"rolled_back"`
}

// DBStudioSearchParams counts, in each table (or the named ones), the rows
// where any text column contains Term.
type DBStudioSearchParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database,omitempty"`
	Login    string     `json:"login,omitempty"`
	Term     string     `json:"term"`
	Tables   []string   `json:"tables,omitempty"`
}

// DBStudioSearchHit is one table's count of matching rows.
type DBStudioSearchHit struct {
	Table   string `json:"table"`
	Matches int64  `json:"matches"`
}

// DBStudioSearchResult lists the tables with matches; Truncated when the
// search stopped at its table limit.
type DBStudioSearchResult struct {
	Tables    []DBStudioSearchHit `json:"tables"`
	Searched  int                 `json:"searched"`
	Truncated bool                `json:"truncated,omitempty"`
}

// DBStudioImportSQLParams imports shared/imports/Name as the signed-in
// user, through the sandboxed client.
type DBStudioImportSQLParams struct {
	Site     state.Site `json:"site"`
	Database string     `json:"database"`
	Login    string     `json:"login"`
	Name     string     `json:"name"`
}

// Validate refuses a name that is not a site's and a password out of
// bounds before the agent tries it.
func (p *DBStudioOpenParams) Validate() error {
	if !site.ValidDBName(p.User) || p.Password == "" || len(p.Password) > 256 || strings.ContainsAny(p.Password, "\x00\r\n") {
		return errors.New("invalid credentials")
	}
	return nil
}

// Validate requires a session handle.
func (p *DBStudioLoginParams) Validate() error {
	if !loginRe.MatchString(p.Login) {
		return errors.New("invalid Quarry session")
	}
	return nil
}

// Validate requires a session handle.
func (p *DBStudioImportSQLParams) Validate() error {
	if !loginRe.MatchString(p.Login) {
		return errors.New("invalid Quarry session")
	}
	return nil
}

// loginRe is a session handle: 32 random bytes in hex.
var loginRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
