package dbstudio

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// table is what the server says about one table: the names requests are
// checked against (SPEC §3).
type table struct {
	Name       string
	Type       string // table | view
	Estimate   int64
	Columns    []rpc.DBColumn
	PrimaryKey []string
	byName     map[string]int
}

// column returns the column called name, exactly.
func (t *table) column(name string) (rpc.DBColumn, error) {
	i, ok := t.byName[name]
	if !ok {
		return rpc.DBColumn{}, agentrpc.NotFound("no column %q in %s", name, t.Name)
	}
	return t.Columns[i], nil
}

const tableQuery = "SELECT TABLE_NAME, TABLE_TYPE, COALESCE(TABLE_ROWS, 0) FROM information_schema.TABLES " +
	"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?"

const columnsQuery = "SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT, COLUMN_KEY, EXTRA, " +
	"COALESCE(COLLATION_NAME, ''), COLUMN_COMMENT FROM information_schema.COLUMNS " +
	"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION"

const primaryQuery = "SELECT COLUMN_NAME FROM information_schema.STATISTICS " +
	"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = 'PRIMARY' ORDER BY SEQ_IN_INDEX"

// lookup reads a table's columns and primary key from information_schema.
// The name must be one the server has, byte for byte.
func lookup(ctx context.Context, s *db.Session, name string) (*table, error) {
	if name == "" || len(name) > 64 {
		return nil, agentrpc.Invalid("invalid table name")
	}
	rows, err := collect(ctx, s, 10, tableQuery, name)
	if err != nil {
		return nil, sqlErr(s, err)
	}
	t := &table{byName: map[string]int{}}
	for _, r := range rows {
		// information_schema compares names without regard to case on some
		// servers: only the exact name counts.
		if str(r[0]) == name {
			t.Name, t.Type, t.Estimate = name, tableType(str(r[1])), num(r[2])
		}
	}
	if t.Name == "" {
		return nil, agentrpc.NotFound("no table %q", name)
	}
	cols, err := collect(ctx, s, 5000, columnsQuery, name)
	if err != nil {
		return nil, sqlErr(s, err)
	}
	for _, r := range cols {
		c := rpc.DBColumn{
			Name: str(r[0]), Type: str(r[1]), DataType: strings.ToLower(str(r[2])),
			Nullable: str(r[3]) == "YES", Default: r[4], Key: str(r[5]), Extra: str(r[6]),
			Collation: str(r[7]), Comment: str(r[8]),
		}
		c.Binary = binaryTypes[c.DataType]
		t.byName[c.Name] = len(t.Columns)
		t.Columns = append(t.Columns, c)
	}
	pk, err := collect(ctx, s, 64, primaryQuery, name)
	if err != nil {
		return nil, sqlErr(s, err)
	}
	for _, r := range pk {
		t.PrimaryKey = append(t.PrimaryKey, str(r[0]))
	}
	return t, nil
}

func tableType(t string) string {
	if strings.Contains(strings.ToUpper(t), "VIEW") {
		return "view"
	}
	return "table"
}

// binaryTypes are shown as hex and never edited as text.
var binaryTypes = map[string]bool{
	"binary": true, "varbinary": true, "tinyblob": true, "blob": true, "mediumblob": true, "longblob": true,
	"bit": true, "geometry": true, "point": true, "linestring": true, "polygon": true,
	"multipoint": true, "multilinestring": true, "multipolygon": true, "geometrycollection": true,
}

// longTypes are read with LEFT() so a big cell is never fetched whole.
var longTypes = map[string]bool{
	"char": true, "varchar": true, "tinytext": true, "text": true, "mediumtext": true, "longtext": true,
	"json": true, "binary": true, "varbinary": true, "tinyblob": true, "blob": true, "mediumblob": true, "longblob": true,
}

const tablesQuery = "SELECT TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE, ''), COALESCE(TABLE_COLLATION, ''), " +
	"COALESCE(TABLE_ROWS, 0), COALESCE(DATA_LENGTH, 0), COALESCE(INDEX_LENGTH, 0), COALESCE(AUTO_INCREMENT, 0), " +
	"COALESCE(TABLE_COMMENT, '') FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME"

const allColumnsQuery = "SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS " +
	"WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION"

// routinesQuery lists the database's stored procedures and functions: the
// tree shows them, Quarry never runs or changes one.
const routinesQuery = "SELECT ROUTINE_NAME, ROUTINE_TYPE FROM information_schema.ROUTINES " +
	"WHERE ROUTINE_SCHEMA = DATABASE() ORDER BY ROUTINE_NAME"

// Tables lists the site's tables and views (SPEC §4).
func (o *Ops) Tables(ctx context.Context, p rpc.DBStudioTablesParams) (rpc.DBStudioTablesResult, error) {
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, true, browseTimeout)
	if err != nil {
		return rpc.DBStudioTablesResult{}, err
	}
	defer s.Close()
	rows, err := collect(ctx, s, 20000, tablesQuery)
	if err != nil {
		return rpc.DBStudioTablesResult{}, sqlErr(s, err)
	}
	out := rpc.DBStudioTablesResult{Database: s.Database(), Tables: []rpc.DBStudioTable{}}
	for _, r := range rows {
		out.Tables = append(out.Tables, rpc.DBStudioTable{
			Name: str(r[0]), Type: tableType(str(r[1])), Engine: str(r[2]), Collation: str(r[3]),
			Rows: num(r[4]), DataBytes: num(r[5]), IndexBytes: num(r[6]), AutoIncrement: num(r[7]), Comment: str(r[8]),
		})
	}
	routines, err := collect(ctx, s, 10000, routinesQuery)
	if err != nil {
		return rpc.DBStudioTablesResult{}, sqlErr(s, err)
	}
	for _, r := range routines {
		if len(r) == 2 {
			out.Routines = append(out.Routines, rpc.DBRoutine{Name: str(r[0]), Type: strings.ToLower(str(r[1]))})
		}
	}
	if p.Columns {
		cols, err := collect(ctx, s, 200000, allColumnsQuery)
		if err != nil {
			return rpc.DBStudioTablesResult{}, sqlErr(s, err)
		}
		out.Columns = map[string][]string{}
		for _, r := range cols {
			out.Columns[str(r[0])] = append(out.Columns[str(r[0])], str(r[1]))
		}
	}
	return out, nil
}

const indexesQuery = "SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME, COALESCE(SUB_PART, 0), INDEX_TYPE " +
	"FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? " +
	"ORDER BY INDEX_NAME <> 'PRIMARY', INDEX_NAME, SEQ_IN_INDEX"

const foreignKeysQuery = "SELECT k.CONSTRAINT_NAME, k.COLUMN_NAME, k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME, " +
	"r.UPDATE_RULE, r.DELETE_RULE FROM information_schema.KEY_COLUMN_USAGE k " +
	"JOIN information_schema.REFERENTIAL_CONSTRAINTS r ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA " +
	"AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND r.TABLE_NAME = k.TABLE_NAME " +
	"WHERE k.TABLE_SCHEMA = DATABASE() AND k.TABLE_NAME = ? AND k.REFERENCED_TABLE_NAME IS NOT NULL " +
	"ORDER BY k.CONSTRAINT_NAME, k.ORDINAL_POSITION"

// Structure describes one table (SPEC §4).
func (o *Ops) Structure(ctx context.Context, p rpc.DBStudioTableParams) (rpc.DBStudioStructure, error) {
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, true, browseTimeout)
	if err != nil {
		return rpc.DBStudioStructure{}, err
	}
	defer s.Close()
	t, err := lookup(ctx, s, p.Table)
	if err != nil {
		return rpc.DBStudioStructure{}, err
	}
	out := rpc.DBStudioStructure{
		Table: t.Name, Type: t.Type, Columns: t.Columns, PrimaryKey: t.PrimaryKey,
		Indexes: []rpc.DBIndex{}, ForeignKeys: []rpc.DBForeignKey{},
	}
	if out.PrimaryKey == nil {
		out.PrimaryKey = []string{}
	}
	idx, err := collect(ctx, s, 10000, indexesQuery, t.Name)
	if err != nil {
		return rpc.DBStudioStructure{}, sqlErr(s, err)
	}
	for _, r := range idx {
		name := str(r[0])
		if n := len(out.Indexes); n == 0 || out.Indexes[n-1].Name != name {
			out.Indexes = append(out.Indexes, rpc.DBIndex{Name: name, Unique: str(r[1]) == "0", Type: str(r[4])})
		}
		last := &out.Indexes[len(out.Indexes)-1]
		last.Columns = append(last.Columns, rpc.DBIndexColumn{Name: str(r[2]), Length: int(num(r[3]))})
	}
	fks, err := collect(ctx, s, 10000, foreignKeysQuery, t.Name)
	if err != nil {
		return rpc.DBStudioStructure{}, sqlErr(s, err)
	}
	for _, r := range fks {
		name := str(r[0])
		if n := len(out.ForeignKeys); n == 0 || out.ForeignKeys[n-1].Name != name {
			out.ForeignKeys = append(out.ForeignKeys, rpc.DBForeignKey{Name: name, RefTable: str(r[2]), OnUpdate: str(r[4]), OnDelete: str(r[5])})
		}
		last := &out.ForeignKeys[len(out.ForeignKeys)-1]
		last.Columns = append(last.Columns, str(r[1]))
		last.RefColumns = append(last.RefColumns, str(r[3]))
	}
	kind := "TABLE"
	if t.Type == "view" {
		kind = "VIEW"
	}
	create, err := collect(ctx, s, 1, "SHOW CREATE "+kind+" "+quoteIdent(t.Name))
	if err != nil {
		return rpc.DBStudioStructure{}, sqlErr(s, err)
	}
	if len(create) == 1 && len(create[0]) >= 2 {
		out.CreateSQL = str(create[0][1])
	}
	return out, nil
}

// Limits of a page of rows (SPEC §4).
const (
	defaultPageSize = 50
	maxPageSize     = 500
	maxOffset       = 10_000_000
	maxSorts        = 4
	maxFilters      = 16
	maxFilterValue  = 4 << 10
	cellBytes       = 4096
	binaryBytes     = 2048
	pageBytes       = 8 << 20
	estimateAbove   = 1_000_000
)

// where appends the filters as a WHERE clause.
func where(b *stmt, t *table, filters []rpc.DBFilter) error {
	if len(filters) > maxFilters {
		return agentrpc.Invalid("at most %d filters", maxFilters)
	}
	for i, f := range filters {
		c, err := t.column(f.Column)
		if err != nil {
			return err
		}
		if len(f.Value) > maxFilterValue {
			return agentrpc.Invalid("a filter value is longer than %d KiB", maxFilterValue>>10)
		}
		if i == 0 {
			b.sql(" WHERE ")
		} else {
			b.sql(" AND ")
		}
		b.ident(c.Name)
		switch f.Op {
		case "eq", "ne", "lt", "le", "gt", "ge":
			b.sql(" " + comparisons[f.Op] + " ").arg(f.Value)
		case "contains":
			b.sql(" LIKE ").arg("%" + likeEscape(f.Value) + "%").sql(" ESCAPE '!'")
		case "starts":
			b.sql(" LIKE ").arg(likeEscape(f.Value) + "%").sql(" ESCAPE '!'")
		case "null":
			b.sql(" IS NULL")
		case "notnull":
			b.sql(" IS NOT NULL")
		default:
			return agentrpc.Invalid("unknown filter %q", f.Op)
		}
	}
	return nil
}

// searchTypes are the columns a search looks in: text a person reads.
var searchTypes = map[string]bool{
	"char": true, "varchar": true, "tinytext": true, "text": true, "mediumtext": true, "longtext": true,
	"json": true, "enum": true, "set": true,
}

// searchClause appends, when term is set, that some text column of t
// contains it: (c1 LIKE ? ESCAPE '!' OR c2 …), ANDed to the filters. A
// table without text columns matches nothing.
func searchClause(b *stmt, t *table, term string, and bool) {
	if term == "" {
		return
	}
	if and {
		b.sql(" AND (")
	} else {
		b.sql(" WHERE (")
	}
	n := 0
	for _, c := range t.Columns {
		if !searchTypes[c.DataType] {
			continue
		}
		if n > 0 {
			b.sql(" OR ")
		}
		b.ident(c.Name).sql(" LIKE ").arg("%" + likeEscape(term) + "%").sql(" ESCAPE '!'")
		n++
	}
	if n == 0 {
		b.sql("1 = 0")
	}
	b.sql(")")
}

var comparisons = map[string]string{"eq": "=", "ne": "<>", "lt": "<", "le": "<=", "gt": ">", "ge": ">="}

// likeEscape makes a value match itself in a LIKE with ESCAPE '!'.
func likeEscape(v string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(v)
}

// Rows reads one page of a table (SPEC §4).
func (o *Ops) Rows(ctx context.Context, p rpc.DBStudioRowsParams) (rpc.DBStudioRowsResult, error) {
	size := p.PageSize
	if size == 0 {
		size = defaultPageSize
	}
	if size < 1 || size > maxPageSize {
		return rpc.DBStudioRowsResult{}, agentrpc.Invalid("the page size must be 1 to %d", maxPageSize)
	}
	if p.Page < 0 || int64(p.Page)*int64(size) > maxOffset {
		return rpc.DBStudioRowsResult{}, agentrpc.Invalid("invalid page")
	}
	if len(p.Sort) > maxSorts {
		return rpc.DBStudioRowsResult{}, agentrpc.Invalid("sort by at most %d columns", maxSorts)
	}
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, true, browseTimeout)
	if err != nil {
		return rpc.DBStudioRowsResult{}, err
	}
	defer s.Close()
	t, err := lookup(ctx, s, p.Table)
	if err != nil {
		return rpc.DBStudioRowsResult{}, err
	}
	pk := map[string]bool{}
	for _, k := range t.PrimaryKey {
		pk[k] = true
	}

	b := &stmt{}
	b.sql("SELECT ")
	for i, c := range t.Columns {
		if i > 0 {
			b.sql(", ")
		}
		switch {
		case pk[c.Name] || !longTypes[c.DataType]:
			b.ident(c.Name)
		case c.Binary:
			b.sql(fmt.Sprintf("LEFT(%s, %d) AS ", quoteIdent(c.Name), binaryBytes+1)).ident(c.Name)
		default:
			b.sql(fmt.Sprintf("LEFT(%s, %d) AS ", quoteIdent(c.Name), cellBytes+1)).ident(c.Name)
		}
	}
	b.sql(" FROM ").ident(t.Name)
	count := &stmt{}
	count.sql("SELECT COUNT(*) FROM ").ident(t.Name)
	if len(p.Search) > maxFilterValue {
		return rpc.DBStudioRowsResult{}, agentrpc.Invalid("the search is longer than %d KiB", maxFilterValue>>10)
	}
	for _, q := range []*stmt{b, count} {
		if err := where(q, t, p.Filters); err != nil {
			return rpc.DBStudioRowsResult{}, err
		}
		searchClause(q, t, p.Search, len(p.Filters) > 0)
	}
	order := []rpc.DBSort{}
	seen := map[string]bool{}
	for _, srt := range p.Sort {
		c, err := t.column(srt.Column)
		if err != nil {
			return rpc.DBStudioRowsResult{}, err
		}
		if !seen[c.Name] {
			seen[c.Name] = true
			order = append(order, rpc.DBSort{Column: c.Name, Desc: srt.Desc})
		}
	}
	// The primary key last: pages do not shift between reads.
	for _, k := range t.PrimaryKey {
		if !seen[k] {
			order = append(order, rpc.DBSort{Column: k})
		}
	}
	for i, srt := range order {
		if i == 0 {
			b.sql(" ORDER BY ")
		} else {
			b.sql(", ")
		}
		b.ident(srt.Column)
		if srt.Desc {
			b.sql(" DESC")
		}
	}
	b.sql(" LIMIT ").arg(size).sql(" OFFSET ").arg(p.Page * size)

	out := rpc.DBStudioRowsResult{
		Columns: t.Columns, PrimaryKey: t.PrimaryKey, Rows: []rpc.DBRow{},
		Page: p.Page, PageSize: size, SQL: b.display(),
	}
	if out.PrimaryKey == nil {
		out.PrimaryKey = []string{}
	}
	// The count first: a page cut at its byte limit ends its result early,
	// which drops the connection.
	if len(p.Filters) == 0 && p.Search == "" && t.Estimate > estimateAbove {
		out.Total, out.TotalEstimated = t.Estimate, true
	} else {
		n, err := collect(ctx, s, 1, count.text(), count.args...)
		if err != nil {
			return rpc.DBStudioRowsResult{}, sqlErr(s, err)
		}
		if len(n) == 1 {
			out.Total = num(n[0][0])
		}
	}
	used := 0
	err = s.Each(ctx, b.text(), b.args, func(_ []string, row [][]byte) error {
		r := rpc.DBRow{Values: make([]*string, len(row))}
		for i, v := range row {
			if v == nil {
				continue
			}
			c := t.Columns[i]
			var text string
			switch {
			case c.Binary:
				shown := v
				if len(shown) > binaryBytes {
					shown = shown[:binaryBytes]
				}
				text = "0x" + strings.ToUpper(hex.EncodeToString(shown))
				r.Cut = append(r.Cut, i)
			case len(v) > cellBytes && !pk[c.Name]:
				text = string(v[:cellBytes])
				r.Cut = append(r.Cut, i)
			default:
				text = string(v)
			}
			used += len(text)
			r.Values[i] = &text
		}
		if used > pageBytes {
			out.Truncated = true
			return db.ErrStop
		}
		out.Rows = append(out.Rows, r)
		return nil
	})
	if err != nil {
		return rpc.DBStudioRowsResult{}, sqlErr(s, err)
	}
	return out, nil
}
