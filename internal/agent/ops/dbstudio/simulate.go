package dbstudio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Simulate (SPEC §12) runs one UPDATE, DELETE, INSERT or REPLACE in a
// transaction that is always rolled back, and says how many rows it
// matched; for an UPDATE or DELETE also up to previewRows of those rows as
// they are now. What cannot be rolled back is refused before anything runs:
// any other statement (DDL and the rest commit implicitly or are not
// transactional), a table that is not InnoDB, a view, a trigger or a stored
// function that could reach further. A refusal is a result (Refused), and a
// statement the server refuses is one too (Message).
func (o *Ops) Simulate(ctx context.Context, p rpc.DBStudioSimulateParams) (rpc.DBStudioSimulateResult, error) {
	stmt := strings.TrimRight(strings.TrimSpace(p.SQL), "; \t\r\n")
	if stmt == "" {
		return rpc.DBStudioSimulateResult{}, agentrpc.Invalid("empty query")
	}
	if len(stmt) > maxStatement {
		return rpc.DBStudioSimulateResult{}, agentrpc.Invalid("the statement is longer than %d KiB", maxStatement>>10)
	}
	plan, why := planSimulation(stmt)
	if why != "" {
		return rpc.DBStudioSimulateResult{Refused: why}, nil
	}
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, p.ReadOnly, browseTimeout)
	if err != nil {
		return rpc.DBStudioSimulateResult{}, err
	}
	defer s.Close()
	// One budget for all of it, the preview and the statement included:
	// each statement's server-side limit is what is left of it (budget).
	outer := ctx
	deadline := time.Now().Add(simBudget)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	res := rpc.DBStudioSimulateResult{Kind: plan.kind}
	// The checks read the statement as the default sql_mode does; a
	// session that reads strings otherwise is refused (one reading is
	// fewer ambiguities; the other two stay as a second line).
	mode, err := collect(ctx, s, 1, simModeQuery)
	if err != nil {
		if ctx.Err() != nil {
			return rpc.DBStudioSimulateResult{}, ctx.Err()
		}
		return rpc.DBStudioSimulateResult{}, sqlErr(s, err)
	}
	if len(mode) != 1 {
		return rpc.DBStudioSimulateResult{}, errors.New("the server did not say its sql_mode")
	}
	if why := unsupportedMode(str(mode[0][0])); why != "" {
		res.Refused = why
		return res, nil
	}
	if why, err := o.rollbackable(ctx, s, plan.names); err != nil {
		if ctx.Err() != nil {
			return rpc.DBStudioSimulateResult{}, ctx.Err()
		}
		return rpc.DBStudioSimulateResult{}, sqlErr(s, err)
	} else if why != "" {
		res.Refused = why
		return res, nil
	}
	// A simulation waits little for a lock the live site holds: while it
	// waits it keeps the rows it has locked already, and the site waits
	// on those in turn.
	if _, err := s.Exec(ctx, simLockWait); err != nil {
		if ctx.Err() != nil {
			return rpc.DBStudioSimulateResult{}, ctx.Err()
		}
		return rpc.DBStudioSimulateResult{}, sqlErr(s, err)
	}
	var rolledBack bool
	err = inRolledBackTransaction(ctx, s, &rolledBack, func() error {
		if plan.preview != "" {
			// The rows as they are, before the statement: a SELECT made of
			// the statement's own tables and WHERE. One the server refuses
			// leaves the preview out; the statement still says its count.
			limit, err := budget(deadline)
			if err != nil {
				return err
			}
			if pr, err := readPreview(ctx, s, plan.preview, limit); err == nil {
				res.Preview, res.PreviewSQL = &pr, plan.preview
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		limit, err := budget(deadline)
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, fmt.Sprintf("SET SESSION max_statement_time = %.3f", limit.Seconds())); err != nil {
			return err
		}
		if plan.returning {
			// A statement with RETURNING answers rows, not a count:
			// one per row it changed.
			var n int64
			err := s.Each(ctx, stmt, nil, func([]string, [][]byte) error { n++; return nil })
			res.Affected = n
			return err
		}
		n, err := s.Exec(ctx, stmt)
		if err != nil {
			return err
		}
		res.Affected = n
		return nil
	})
	var rb *rollbackError
	switch {
	case err == nil:
		res.RolledBack = true
	case errors.As(err, &rb):
		// The connection is closed with the session, and the server rolls
		// back the transaction of a closed connection.
		o.log().Warn("a simulation's rollback failed; its connection is closed", "err", rb.err)
		return rpc.DBStudioSimulateResult{}, errors.New("the simulation could not be rolled back cleanly; its connection was closed, which rolls it back")
	case errors.Is(err, errNoTransaction):
		res.Refused = "the server did not open a transaction, so nothing was run"
	case errors.Is(err, errOutOfTime):
		res.Message = fmt.Sprintf("the simulation used up its %s before the statement could run; nothing was changed", simBudget)
		res.RolledBack = rolledBack
	case outer.Err() != nil:
		return rpc.DBStudioSimulateResult{}, outer.Err()
	case errors.Is(err, context.DeadlineExceeded):
		res.Message = fmt.Sprintf("the simulation ran longer than %s and was stopped", simBudget)
		res.RolledBack = rolledBack
	default:
		res.Message = s.ServerMessage(err)
		res.RolledBack = rolledBack
	}
	return res, nil
}

// simBudget is the time a whole simulation has once connected: checks,
// preview and statement (the rollback has its own). simMargin is left to
// the server to stop a statement itself, before the agent kills it.
var simBudget = browseTimeout

const simMargin = 5 * time.Second

// errOutOfTime: too little of the budget is left to run the next
// statement.
var errOutOfTime = errors.New("the simulation's time is used up")

// budget is the server-side limit for the next statement: what is left
// before deadline, less simMargin; errOutOfTime under a second.
func budget(deadline time.Time) (time.Duration, error) {
	left := time.Until(deadline) - simMargin
	if left < time.Second {
		return 0, errOutOfTime
	}
	return left, nil
}

// simModeQuery is the session's sql_mode, which decides how the server
// reads the statement's strings.
const simModeQuery = "SELECT @@SESSION.sql_mode"

// unsupportedMode says why a session in sql_mode mode cannot simulate, or
// "": ANSI_QUOTES (and the modes that include it, ANSI, ORACLE, MSSQL …,
// which the server lists expanded) and NO_BACKSLASH_ESCAPES read strings
// otherwise than the default.
func unsupportedMode(mode string) string {
	for _, m := range strings.Split(mode, ",") {
		if m = strings.ToUpper(strings.TrimSpace(m)); m == "ANSI_QUOTES" || m == "NO_BACKSLASH_ESCAPES" {
			return "the server's sql_mode has " + m + ", which reads strings differently: simulate needs the default quoting"
		}
	}
	return ""
}

// simLockWait bounds how long a simulation waits for a row lock or a
// table's metadata lock, in seconds (the server's defaults are 50 and a
// day).
const simLockWait = "SET SESSION innodb_lock_wait_timeout = 5, lock_wait_timeout = 5"

// previewRows is how many of the matched rows a simulation shows, in at
// most previewBytes of cells.
const (
	previewRows  = 25
	previewBytes = 1 << 20
)

// readPreview runs the preview SELECT and keeps what a simulation shows.
// The rows past it are read and dropped, never cut off: ending a result
// early drops the connection, and with it the transaction the statement
// is about to run in. sql_select_limit makes the server stop one row past
// what is shown (a LIMIT of the statement's own still wins).
func readPreview(ctx context.Context, s *db.Session, q string, limit time.Duration) (rpc.DBQueryResult, error) {
	var res rpc.DBQueryResult
	size := 0
	err := s.Each(ctx, fmt.Sprintf("SET STATEMENT sql_select_limit = %d, max_statement_time = %.3f FOR %s", previewRows+1, limit.Seconds(), q), nil, func(cols []string, row [][]byte) error {
		if res.Truncated {
			return nil
		}
		if res.Columns == nil {
			res.Columns = append([]string(nil), cols...)
		}
		out := make([]string, len(row))
		n := 0
		for i, v := range row {
			if v == nil {
				out[i] = "NULL"
			} else {
				out[i] = string(v)
			}
			n += len(out[i])
		}
		if len(res.Rows) >= previewRows || size+n > previewBytes {
			res.Truncated = true
			res.Message = fmt.Sprintf("showing the first %d rows", len(res.Rows))
			return nil
		}
		size += n
		res.Rows = append(res.Rows, out)
		return nil
	})
	if err != nil {
		return rpc.DBQueryResult{}, err
	}
	if len(res.Rows) == 0 && res.Message == "" {
		res.Message = "OK (no rows)"
	}
	return res, nil
}

var errNoTransaction = errors.New("no transaction is open")

type rollbackError struct{ err error }

func (e *rollbackError) Error() string { return "rollback: " + e.err.Error() }

// inRolledBackTransaction runs fn between START TRANSACTION and ROLLBACK,
// on every path: fn's error, a panic, a cancelled request (the rollback has
// its own context). Nothing runs unless the server confirms a transaction
// is open (@@in_transaction), so a statement is never sent in autocommit.
// rolledBack is set once the server has taken the ROLLBACK.
// A failed rollback is a *rollbackError; the caller closes the session,
// which ends the transaction on the server.
func inRolledBackTransaction(ctx context.Context, s *db.Session, rolledBack *bool, fn func() error) (err error) {
	if _, err := s.Exec(ctx, "START TRANSACTION"); err != nil {
		return err
	}
	defer func() {
		_, rerr := s.Exec(context.WithoutCancel(ctx), "ROLLBACK")
		if rerr != nil {
			err = &rollbackError{errors.Join(rerr, err)}
			return
		}
		*rolledBack = true
	}()
	open, err := collect(ctx, s, 1, "SELECT @@in_transaction")
	if err != nil {
		return err
	}
	if len(open) != 1 || str(open[0][0]) != "1" {
		return errNoTransaction
	}
	return fn()
}

// Queries of the rollback check. The tables are read in the session, so
// only what the user may see is looked at; triggers, routines and UDFs as
// root, because a scoped account (it lacks TRIGGER) or a user may not see
// the triggers that would run as their definer. Every routine type counts:
// a package's functions are called through its name (pk.f()), and a
// procedure so named errs on the safe side.
const (
	simTablesQuery = "SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, ENGINE FROM information_schema.TABLES " +
		"WHERE (TABLE_SCHEMA = DATABASE() OR LOWER(TABLE_SCHEMA) IN (%s)) AND LOWER(TABLE_NAME) IN (%s)"
	simSchemasQuery  = "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = DATABASE() OR LOWER(SCHEMA_NAME) IN (%s)"
	simTriggersQuery = "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE %s"
	simRoutinesQuery = "SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA IN (%s) AND LOWER(ROUTINE_NAME) IN (%s)"
	simUDFsQuery     = "SELECT COUNT(*) FROM mysql.func WHERE LOWER(name) IN (%s)"
	// maxSimNames bounds the names a statement may give to be checked.
	maxSimNames = 1000
	// maxSimTables bounds the tables those names may match; past it the
	// statement is refused, never checked in part.
	maxSimTables = 10000
)

// rollbackable says why the statement, whose every word and quoted name
// is in names, cannot be simulated, or "" when everything it can reach is
// an InnoDB table without triggers. Any word that names a table counts:
// a column called like a MyISAM table refuses too, which errs safe.
func (o *Ops) rollbackable(ctx context.Context, s *db.Session, names []string) (string, error) {
	if len(names) > maxSimNames {
		return fmt.Sprintf("the statement names more than %d things to check", maxSimNames), nil
	}
	args := make([]any, 0, 2*len(names))
	for _, n := range names {
		args = append(args, n)
	}
	in := lowered(len(names))
	rows, err := collect(ctx, s, maxSimTables+1, fmt.Sprintf(simTablesQuery, in, in), append(args, args...)...)
	if err != nil {
		return "", err
	}
	if len(rows) > maxSimTables {
		// collect drops the rows past its limit: those tables would go
		// unchecked.
		return fmt.Sprintf("the statement names more than %d tables to check", maxSimTables), nil
	}
	if len(rows) == 0 {
		return "it names no table this user can see, so what it changes cannot be checked", nil
	}
	type pair struct{ schema, table string }
	var tables []pair
	schemas := map[string]bool{}
	for _, r := range rows {
		schema, table, typ, engine := str(r[0]), str(r[1]), str(r[2]), str(r[3])
		name := schema + "." + table
		switch {
		case typ != "BASE TABLE":
			return fmt.Sprintf("%s is a %s, which cannot be checked: simulate on its tables", name, strings.ToLower(typ)), nil
		case !strings.EqualFold(engine, "InnoDB"):
			if engine == "" {
				engine = "unknown"
			}
			return fmt.Sprintf("%s uses the %s engine, which does not roll back", name, engine), nil
		}
		tables = append(tables, pair{schema, table})
		schemas[schema] = true
	}
	more, err := collect(ctx, s, maxSimTables+1, fmt.Sprintf(simSchemasQuery, in), args...)
	if err != nil {
		return "", err
	}
	if len(more) > maxSimTables {
		return fmt.Sprintf("the statement names more than %d databases to check", maxSimTables), nil
	}
	for _, r := range more {
		schemas[str(r[0])] = true
	}

	var where strings.Builder
	var targs []any
	for i, t := range tables {
		if i > 0 {
			where.WriteString(" OR ")
		}
		where.WriteString("(EVENT_OBJECT_SCHEMA = ? AND EVENT_OBJECT_TABLE = ?)")
		targs = append(targs, t.schema, t.table)
	}
	n, err := s.Maria().QueryStrings(ctx, fmt.Sprintf(simTriggersQuery, where.String()), targs...)
	if err != nil {
		return "", fmt.Errorf("check for triggers: %w", err)
	}
	if len(n) != 1 || n[0] != "0" {
		return "a table it names has triggers, which may change what a rollback cannot undo", nil
	}
	sargs := make([]any, 0, len(schemas)+len(names))
	for schema := range schemas {
		sargs = append(sargs, schema)
	}
	n, err = s.Maria().QueryStrings(ctx, fmt.Sprintf(simRoutinesQuery, placeholders(len(schemas)), in), append(sargs, args...)...)
	if err != nil {
		return "", fmt.Errorf("check for stored functions: %w", err)
	}
	if len(n) != 1 || n[0] != "0" {
		return "it names a stored function or package, which may change what a rollback cannot undo", nil
	}
	n, err = s.Maria().QueryStrings(ctx, fmt.Sprintf(simUDFsQuery, in), args...)
	if err != nil {
		return "", fmt.Errorf("check for loadable functions: %w", err)
	}
	if len(n) != 1 || n[0] != "0" {
		return "it names a loadable function (UDF), which may change what a rollback cannot undo", nil
	}
	return "", nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// lowered is n placeholders, each lowered by the server: its lower case,
// not Go's, is the one a name matches (they differ for some capitals,
// U+0130 among them), and a name compared without LOWER may be looked up
// on disk, case-sensitively.
func lowered(n int) string {
	return strings.TrimSuffix(strings.Repeat("LOWER(?), ", n), ", ")
}

// simulation is what planSimulation found in a statement.
type simulation struct {
	kind    string   // update, delete, insert, replace
	names   []string // every word and quoted name, as written, once
	preview string   // the SELECT of the rows it would change, or ""
	// returning: the statement ends in RETURNING (in some reading), so it
	// answers the rows it changed rather than a count.
	returning bool
}

// implicitCommit are statements that commit, or cannot be undone, by
// themselves: they are named in the refusal.
var implicitCommit = map[string]bool{
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "RENAME": true, "LOCK": true, "UNLOCK": true,
	"SET": true, "GRANT": true, "REVOKE": true, "START": true, "BEGIN": true, "COMMIT": true, "ROLLBACK": true,
	"SAVEPOINT": true, "RELEASE": true, "XA": true, "LOAD": true, "FLUSH": true, "OPTIMIZE": true, "ANALYZE": true,
	"REPAIR": true, "CHECK": true, "CALL": true, "DO": true, "HANDLER": true, "INSTALL": true, "UNINSTALL": true,
	"RESET": true, "PURGE": true, "CHANGE": true, "KILL": true, "SHUTDOWN": true, "BACKUP": true, "PREPARE": true,
	"EXECUTE": true, "DEALLOCATE": true, "CACHE": true,
}

// planSimulation decides whether stmt may be simulated and how, or says
// why not. It lexes the statement in each way a server's sql_mode reads
// it (readings), and takes the names of all: whatever the mode, no name
// is missed.
func planSimulation(stmt string) (simulation, string) {
	var all [][]token
	for _, r := range readings {
		list, err := lex(stmt, r)
		if err != nil {
			return simulation{}, err.Error()
		}
		all = append(all, list)
	}
	toks := all[0]
	for _, list := range all {
		for _, t := range list {
			if t.kind == tPunct && t.text == ";" {
				return simulation{}, "simulate one statement at a time"
			}
		}
	}
	if len(toks) == 0 || toks[0].kind != tWord {
		return simulation{}, "only UPDATE, DELETE, INSERT and REPLACE can be simulated"
	}
	first := strings.ToUpper(toks[0].text)
	var kind string
	switch first {
	case "UPDATE", "DELETE", "INSERT", "REPLACE":
		kind = strings.ToLower(first)
	case "SELECT", "WITH", "VALUES", "TABLE", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "HELP":
		return simulation{}, first + " changes nothing: run it, or use Explain"
	default:
		if first == "SET" && len(toks) > 1 && isWord(toks[1], "STATEMENT") {
			return simulation{}, "SET STATEMENT … FOR changes the session for the statement: simulate the statement alone"
		}
		if implicitCommit[first] {
			return simulation{}, first + " commits by itself or cannot be undone, so it cannot be simulated in a transaction"
		}
		return simulation{}, "only UPDATE, DELETE, INSERT and REPLACE can be simulated"
	}
	for _, list := range all {
		if why := checkShape(list, kind); why != "" {
			return simulation{}, why
		}
		if why := checkCalls(stmt, list); why != "" {
			return simulation{}, why
		}
	}
	seen := map[string]bool{}
	var names []string
	for _, list := range all {
		for i, t := range list {
			// What could name a table: a `name` or a "string" (a name
			// under ANSI_QUOTES), a word that is not a number, and digits
			// after a name's dot (db.123 names table 123); 64 characters
			// at most, the longest name there is. As written: the server
			// lowers them, as it lowers the names it compares them with.
			switch {
			case t.kind == tPunct, t.kind == tString && stmt[t.start] != '"', utf8.RuneCountInString(t.text) > 64:
				continue
			case t.kind == tWord && allDigits(t.text) && !afterNameDot(list, i):
				continue
			}
			if t.text != "" && !seen[t.text] {
				seen[t.text] = true
				names = append(names, t.text)
			}
		}
	}
	sim := simulation{kind: kind, names: names}
	for _, list := range all {
		for _, t := range list {
			if t.kind == tWord && t.depth == 0 && strings.EqualFold(t.text, "RETURNING") {
				sim.returning = true
			}
		}
	}
	sim.preview = previewSelect(stmt, all)
	return sim, ""
}

// reading is how a sql_mode reads strings: whether a backslash escapes in
// '…' and in "…".
type reading struct{ single, double bool }

// readings are the three a server may use: the default (a backslash
// escapes in both), NO_BACKSLASH_ESCAPES (in neither), and ANSI_QUOTES
// with escapes (in '…' only, "…" being a name).
var readings = []reading{{true, true}, {false, false}, {true, false}}

// previewSelect builds, for a one-target UPDATE or DELETE, the SELECT of
// the rows it would change: SELECT * FROM <its tables> <its WHERE, ORDER
// BY and LIMIT>. Every lexing must agree on where the parts are, and the
// tail may not hold an INTO; otherwise there is no preview.
func previewSelect(stmt string, all [][]token) string {
	a := previewParts(stmt, all[0])
	for _, list := range all[1:] {
		if previewParts(stmt, list) != a {
			return ""
		}
	}
	return a
}

func previewParts(stmt string, toks []token) string {
	top := func(i int, words ...string) bool {
		if toks[i].kind != tWord || toks[i].depth != 0 {
			return false
		}
		for _, w := range words {
			if strings.EqualFold(toks[i].text, w) {
				return true
			}
		}
		return false
	}
	find := func(from int, words ...string) int {
		for i := from; i < len(toks); i++ {
			if top(i, words...) {
				return i
			}
		}
		return -1
	}
	i := 1
	var refs, tail string
	switch strings.ToUpper(toks[0].text) {
	case "UPDATE":
		for i < len(toks) && top(i, "LOW_PRIORITY", "IGNORE") {
			i++
		}
		set := find(i, "SET")
		if set <= i {
			return ""
		}
		refs = stmt[toks[i].start:toks[set-1].end]
		if w := find(set+1, "WHERE", "ORDER", "LIMIT"); w > 0 {
			tail = stmt[toks[w].start:]
		}
	case "DELETE":
		for i < len(toks) && top(i, "LOW_PRIORITY", "QUICK", "IGNORE") {
			i++
		}
		if i >= len(toks) || !top(i, "FROM") || find(i+1, "USING") > 0 {
			return ""
		}
		i++
		end := find(i, "WHERE", "ORDER", "LIMIT", "RETURNING")
		if end < 0 {
			end = len(toks)
		}
		if end <= i {
			return ""
		}
		refs = stmt[toks[i].start:toks[end-1].end]
		if end < len(toks) && !top(end, "RETURNING") {
			stop := len(stmt)
			if r := find(end, "RETURNING"); r > 0 {
				stop = toks[r].start
			}
			tail = strings.TrimSpace(stmt[toks[end].start:stop])
		}
	default:
		return ""
	}
	for j := range toks {
		if top(j, "INTO", "PROCEDURE") {
			return ""
		}
	}
	q := "SELECT * FROM " + strings.TrimSpace(refs)
	if tail != "" {
		q += " " + tail
	}
	return q
}

type tokKind int

const (
	tWord   tokKind = iota // a word or number
	tName                  // a `quoted` name
	tString                // a '…' or "…" string (a "…" may be a name with ANSI_QUOTES)
	tPunct
)

type token struct {
	kind       tokKind
	text       string
	start, end int
	depth      int // parentheses around it
}

// lex splits a statement into tokens the way MariaDB reads it, enough to
// find its first word, its names and its top-level clauses. Comments are
// skipped; an executable comment (/*! … */, /*M! … */) is refused, since
// its content runs; so are an unterminated string, name or comment and
// unbalanced parentheses.
func lex(s string, r reading) ([]token, error) {
	var out []token
	depth := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '#' || (c == '-' && i+1 < len(s) && s[i+1] == '-' && (i+2 == len(s) || s[i+2] <= ' ')):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			if strings.HasPrefix(s[i+2:], "!") || strings.HasPrefix(s[i+2:], "M!") {
				return nil, errors.New("an executable comment (/*! … */) cannot be simulated")
			}
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("a comment is not closed")
			}
			i += 2 + end + 2
		case c == '\'' || c == '"' || c == '`':
			start := i
			var b strings.Builder
			i++
			closed := false
			for i < len(s) {
				d := s[i]
				if d == '\\' && (c == '\'' && r.single || c == '"' && r.double) && i+1 < len(s) {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if d == c {
					if i+1 < len(s) && s[i+1] == c {
						b.WriteByte(c)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errors.New("a string or quoted name is not closed")
			}
			kind := tString
			if c == '`' {
				kind = tName
			}
			out = append(out, token{kind: kind, text: b.String(), start: start, end: i, depth: depth})
		case isWordByte(c):
			start := i
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
			out = append(out, token{kind: tWord, text: s[start:i], start: start, end: i, depth: depth})
		default:
			if c == ')' {
				depth--
				if depth < 0 {
					return nil, errors.New("the parentheses do not balance")
				}
			}
			out = append(out, token{kind: tPunct, text: string(c), start: i, end: i + 1, depth: depth})
			if c == '(' {
				depth++
			}
			i++
		}
	}
	if depth != 0 {
		return nil, errors.New("the parentheses do not balance")
	}
	return out, nil
}

// afterNameDot reports whether list[i] follows a dot that follows a name
// (db.123), not a number (1.5).
func afterNameDot(list []token, i int) bool {
	if i < 2 || list[i-1].kind != tPunct || list[i-1].text != "." {
		return false
	}
	p := list[i-2]
	return p.kind == tName || p.kind == tString || (p.kind == tWord && !allDigits(p.text))
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
