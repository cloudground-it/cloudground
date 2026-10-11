package dbstudio

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

const maxChanges = 100

// built is one statement of a batch and the rows it must touch.
type built struct {
	*stmt
	want int64
}

// buildChange turns one change into its statement, every name checked
// against the table (SPEC §5).
func buildChange(t *table, c rpc.DBChange) (built, error) {
	if t.Type != "table" {
		return built{}, agentrpc.Invalid("%s is a view; edit its tables instead", t.Name)
	}
	// Values in the table's column order: the SQL is the same for the same
	// change, whatever order the request listed them in.
	var cols []rpc.DBColumn
	for name := range c.Values {
		col, err := t.column(name)
		if err != nil {
			return built{}, err
		}
		if col.Binary {
			return built{}, agentrpc.Invalid("%s is a binary column; change it with SQL", col.Name)
		}
	}
	for _, col := range t.Columns {
		if _, ok := c.Values[col.Name]; ok {
			cols = append(cols, col)
		}
	}
	b := &stmt{}
	switch c.Op {
	case "insert":
		if len(c.Key) != 0 {
			return built{}, agentrpc.Invalid("an insert takes values, not a key")
		}
		b.sql("INSERT INTO ").ident(t.Name).sql(" (")
		for i, col := range cols {
			if i > 0 {
				b.sql(", ")
			}
			b.ident(col.Name)
		}
		b.sql(") VALUES (")
		for i, col := range cols {
			if i > 0 {
				b.sql(", ")
			}
			b.arg(c.Values[col.Name])
		}
		b.sql(")")
		return built{b, 1}, nil
	case "update":
		if len(cols) == 0 {
			return built{}, agentrpc.Invalid("an update needs at least one value")
		}
		b.sql("UPDATE ").ident(t.Name).sql(" SET ")
		for i, col := range cols {
			if i > 0 {
				b.sql(", ")
			}
			b.ident(col.Name).sql(" = ").arg(c.Values[col.Name])
		}
	case "delete":
		if len(c.Values) != 0 {
			return built{}, agentrpc.Invalid("a delete takes a key, not values")
		}
		b.sql("DELETE FROM ").ident(t.Name)
	default:
		return built{}, agentrpc.Invalid("unknown change %q", c.Op)
	}
	if err := byKey(b, t, c.Key); err != nil {
		return built{}, err
	}
	b.sql(" LIMIT 1")
	return built{b, 1}, nil
}

// byKey appends WHERE pk1 = ? AND …: the key must name every primary-key
// column and nothing else.
func byKey(b *stmt, t *table, key map[string]string) error {
	if len(t.PrimaryKey) == 0 {
		return agentrpc.Invalid("%s has no primary key; edit it with SQL", t.Name)
	}
	if len(key) != len(t.PrimaryKey) {
		return agentrpc.Invalid("the key must give the primary key (%s)", strings.Join(t.PrimaryKey, ", "))
	}
	for i, k := range t.PrimaryKey {
		v, ok := key[k]
		if !ok {
			return agentrpc.Invalid("the key must give the primary key (%s)", strings.Join(t.PrimaryKey, ", "))
		}
		if col, _ := t.column(k); col.Binary {
			return agentrpc.Invalid("the primary key of %s is binary; edit it with SQL", t.Name)
		}
		if i == 0 {
			b.sql(" WHERE ")
		} else {
			b.sql(" AND ")
		}
		b.ident(k).sql(" = ").arg(v)
	}
	return nil
}

// Edit changes rows by primary key in one transaction, or with DryRun
// returns the SQL it would run (SPEC §5).
func (o *Ops) Edit(ctx context.Context, p rpc.DBStudioEditParams) (rpc.DBStudioEditResult, error) {
	if len(p.Changes) == 0 || len(p.Changes) > maxChanges {
		return rpc.DBStudioEditResult{}, agentrpc.Invalid("give 1 to %d changes", maxChanges)
	}
	// A dry run only reads the table's columns. A read-only caller gets a
	// read-only account: the server refuses its write (SPEC §2).
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, p.ReadOnly || p.DryRun, editTimeout)
	if err != nil {
		return rpc.DBStudioEditResult{}, err
	}
	defer s.Close()
	t, err := lookup(ctx, s, p.Table)
	if err != nil {
		return rpc.DBStudioEditResult{}, err
	}
	stmts := make([]built, 0, len(p.Changes))
	res := rpc.DBStudioEditResult{SQL: []string{"START TRANSACTION"}}
	for _, c := range p.Changes {
		b, err := buildChange(t, c)
		if err != nil {
			return rpc.DBStudioEditResult{}, err
		}
		stmts = append(stmts, b)
		res.SQL = append(res.SQL, b.display())
	}
	res.SQL = append(res.SQL, "COMMIT")
	if p.DryRun {
		return res, nil
	}
	if _, err := s.Exec(ctx, "START TRANSACTION"); err != nil {
		return rpc.DBStudioEditResult{}, sqlErr(s, err)
	}
	for i, b := range stmts {
		n, err := s.Exec(ctx, b.text(), b.args...)
		if err == nil && n != b.want {
			err = agentrpc.Conflict("change %d matched %d rows, not %d: the row changed or is gone; nothing was saved", i+1, n, b.want)
		} else if err != nil {
			err = sqlErr(s, err)
		}
		if err != nil {
			return rpc.DBStudioEditResult{}, rollback(ctx, s, err)
		}
		res.Affected += n
	}
	if _, err := s.Exec(ctx, "COMMIT"); err != nil {
		return rpc.DBStudioEditResult{}, rollback(ctx, s, sqlErr(s, err))
	}
	res.Executed = true
	return res, nil
}

// rollback ends a failed transaction and returns cause. Should the
// rollback fail too, the connection is closed with the session, and the
// server rolls back an open transaction of a closed connection itself.
func rollback(ctx context.Context, s *db.Session, cause error) error {
	if _, err := s.Exec(context.WithoutCancel(ctx), "ROLLBACK"); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback: %w", err))
	}
	return cause
}

// newNameRe is the rule for a column or index Quarry adds.
var newNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// columnType is how a type of the guided ALTER takes its arguments.
type columnType struct {
	length   int // 0 none, 1 optional, 2 required
	decimal  bool
	unsigned bool
	numeric  bool
	clock    bool // CURRENT_TIMESTAMP is a default
}

var columnTypes = map[string]columnType{
	"TINYINT": {unsigned: true, numeric: true}, "SMALLINT": {unsigned: true, numeric: true},
	"MEDIUMINT": {unsigned: true, numeric: true}, "INT": {unsigned: true, numeric: true},
	"BIGINT": {unsigned: true, numeric: true}, "DECIMAL": {decimal: true, unsigned: true, numeric: true},
	"FLOAT": {unsigned: true, numeric: true}, "DOUBLE": {unsigned: true, numeric: true},
	"BOOLEAN": {numeric: true},
	"CHAR":    {length: 1}, "VARCHAR": {length: 2}, "BINARY": {length: 1}, "VARBINARY": {length: 2},
	"TINYTEXT": {}, "TEXT": {}, "MEDIUMTEXT": {}, "LONGTEXT": {},
	"BLOB": {}, "MEDIUMBLOB": {}, "LONGBLOB": {},
	"DATE": {}, "DATETIME": {clock: true}, "TIMESTAMP": {clock: true}, "TIME": {}, "YEAR": {numeric: true},
	"JSON": {},
}

var numberRe = regexp.MustCompile(`^-?[0-9]{1,30}(\.[0-9]{1,30})?$`)

// columnSQL appends a new column's definition.
func columnSQL(b *stmt, t *table, c *rpc.DBNewColumn) error {
	if !newNameRe.MatchString(c.Name) {
		return agentrpc.Invalid("a column name is letters, digits and _, at most 64, not starting with a digit")
	}
	if _, err := t.column(c.Name); err == nil {
		return agentrpc.Conflict("%s already has a column %s", t.Name, c.Name)
	}
	typ := strings.ToUpper(strings.TrimSpace(c.Type))
	ct, ok := columnTypes[typ]
	if !ok {
		return agentrpc.Invalid("unsupported column type %q", c.Type)
	}
	b.sql("ALTER TABLE ").ident(t.Name).sql(" ADD COLUMN ").ident(c.Name).sql(" " + typ)
	switch {
	case ct.decimal:
		if c.Length < 1 || c.Length > 65 || c.Scale < 0 || c.Scale > 30 || c.Scale > c.Length {
			return agentrpc.Invalid("DECIMAL takes a precision of 1 to 65 and a scale of 0 to 30, not above it")
		}
		b.sql(fmt.Sprintf("(%d,%d)", c.Length, c.Scale))
	case ct.length == 2 && c.Length == 0:
		return agentrpc.Invalid("%s needs a length", typ)
	case ct.length > 0 && c.Length != 0:
		if c.Length < 1 || c.Length > 65535 {
			return agentrpc.Invalid("the length must be 1 to 65535")
		}
		b.sql(fmt.Sprintf("(%d)", c.Length))
	case c.Length != 0 || c.Scale != 0:
		return agentrpc.Invalid("%s takes no length", typ)
	}
	if c.Unsigned {
		if !ct.unsigned {
			return agentrpc.Invalid("%s cannot be unsigned", typ)
		}
		b.sql(" UNSIGNED")
	}
	if c.Nullable {
		b.sql(" NULL")
	} else {
		b.sql(" NOT NULL")
	}
	if c.Default != nil {
		d := *c.Default
		switch {
		case ct.clock && strings.EqualFold(d, "CURRENT_TIMESTAMP"):
			b.sql(" DEFAULT CURRENT_TIMESTAMP")
		case ct.numeric:
			if !numberRe.MatchString(d) {
				return agentrpc.Invalid("the default of a %s column must be a number", typ)
			}
			b.sql(" DEFAULT " + d)
		default:
			if len(d) > 1024 || strings.ContainsRune(d, '\\') || strings.ContainsFunc(d, unicode.IsControl) {
				return agentrpc.Invalid("the default must be at most 1024 characters, without backslashes or control characters")
			}
			// DDL takes no bound values: the literal is quoted with its
			// quotes doubled, and a default can hold no backslash, so it
			// reads the same with or without NO_BACKSLASH_ESCAPES.
			b.sql(" DEFAULT '" + strings.ReplaceAll(d, "'", "''") + "'")
		}
	}
	if c.After != "" {
		after, err := t.column(c.After)
		if err != nil {
			return err
		}
		b.sql(" AFTER ").ident(after.Name)
	}
	return nil
}

// indexSQL appends a new index.
func indexSQL(b *stmt, t *table, ix *rpc.DBNewIndex, existing []string) error {
	if !newNameRe.MatchString(ix.Name) || strings.EqualFold(ix.Name, "PRIMARY") {
		return agentrpc.Invalid("an index name is letters, digits and _, at most 64, not starting with a digit")
	}
	for _, e := range existing {
		if strings.EqualFold(e, ix.Name) {
			return agentrpc.Conflict("%s already has an index %s", t.Name, ix.Name)
		}
	}
	if len(ix.Columns) == 0 || len(ix.Columns) > 16 {
		return agentrpc.Invalid("an index has 1 to 16 columns")
	}
	b.sql("ALTER TABLE ").ident(t.Name).sql(" ADD ")
	if ix.Unique {
		b.sql("UNIQUE ")
	}
	b.sql("INDEX ").ident(ix.Name).sql(" (")
	for i, ic := range ix.Columns {
		c, err := t.column(ic.Name)
		if err != nil {
			return err
		}
		if i > 0 {
			b.sql(", ")
		}
		b.ident(c.Name)
		if ic.Length != 0 {
			if ic.Length < 1 || ic.Length > 3072 {
				return agentrpc.Invalid("a prefix length must be 1 to 3072")
			}
			b.sql(fmt.Sprintf("(%d)", ic.Length))
		}
	}
	b.sql(")")
	return nil
}

const indexNamesQuery = "SELECT DISTINCT INDEX_NAME FROM information_schema.STATISTICS " +
	"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?"

// Alter adds a column or an index, or with DryRun returns the ALTER it
// would run (SPEC §6).
func (o *Ops) Alter(ctx context.Context, p rpc.DBStudioAlterParams) (rpc.DBStudioAlterResult, error) {
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, p.ReadOnly || p.DryRun, alterTimeout)
	if err != nil {
		return rpc.DBStudioAlterResult{}, err
	}
	defer s.Close()
	t, err := lookup(ctx, s, p.Table)
	if err != nil {
		return rpc.DBStudioAlterResult{}, err
	}
	if t.Type != "table" && p.Kind != "drop" {
		return rpc.DBStudioAlterResult{}, agentrpc.Invalid("%s is a view", t.Name)
	}
	b := &stmt{}
	switch {
	case p.Kind == "truncate" || p.Kind == "drop":
		// Operations (SPEC §6): the name typed must be the table's own,
		// byte for byte.
		if p.Confirm != t.Name {
			return rpc.DBStudioAlterResult{}, agentrpc.Invalid("type the table's name to confirm")
		}
		switch {
		case p.Kind == "truncate":
			b.sql("TRUNCATE TABLE ")
		case t.Type == "view":
			b.sql("DROP VIEW ")
		default:
			b.sql("DROP TABLE ")
		}
		b.ident(t.Name)
	case p.Kind == "add_column" && p.Column != nil:
		err = columnSQL(b, t, p.Column)
	case p.Kind == "add_index" && p.Index != nil:
		var names [][]*string
		names, err = collect(ctx, s, 10000, indexNamesQuery, t.Name)
		if err != nil {
			return rpc.DBStudioAlterResult{}, sqlErr(s, err)
		}
		existing := make([]string, 0, len(names))
		for _, n := range names {
			existing = append(existing, str(n[0]))
		}
		err = indexSQL(b, t, p.Index, existing)
	default:
		err = agentrpc.Invalid("give add_column with a column, or add_index with an index")
	}
	if err != nil {
		return rpc.DBStudioAlterResult{}, err
	}
	res := rpc.DBStudioAlterResult{SQL: b.display()}
	if p.DryRun {
		return res, nil
	}
	if _, err := s.Exec(ctx, b.text(), b.args...); err != nil {
		return rpc.DBStudioAlterResult{}, sqlErr(s, err)
	}
	res.Executed = true
	return res, nil
}
