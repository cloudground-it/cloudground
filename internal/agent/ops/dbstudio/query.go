package dbstudio

import (
	"context"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// maxStatement is the longest statement the SQL editor sends.
const maxStatement = 1 << 20

// Query is the SQL editor (SPEC §1, §11): one statement, the console's
// limits (5000 rows, 8 MiB, 60 s), as the signed-in user or as a scoped
// account, read-only unless writes were allowed. A statement the server
// refuses is a result with its message, not an error.
func (o *Ops) Query(ctx context.Context, p rpc.DBStudioQueryParams) (rpc.DBQueryResult, error) {
	stmt := strings.TrimRight(strings.TrimSpace(p.SQL), "; \t\r\n")
	if stmt == "" {
		return rpc.DBQueryResult{}, agentrpc.Invalid("empty query")
	}
	if len(stmt) > maxStatement {
		return rpc.DBQueryResult{}, agentrpc.Invalid("the statement is longer than %d KiB", maxStatement>>10)
	}
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, p.ReadOnly, browseTimeout)
	if err != nil {
		return rpc.DBQueryResult{}, err
	}
	defer s.Close()
	res, err := s.Query(ctx, db.ConsoleLimits, stmt)
	if err != nil {
		if ctx.Err() != nil {
			return rpc.DBQueryResult{}, ctx.Err()
		}
		return rpc.DBQueryResult{Message: s.ServerMessage(err)}, nil
	}
	return res, nil
}

// Limits of a search (SPEC §4).
const (
	maxSearchTerm   = 256
	maxSearchTables = 200
	// searchBudget is how long a search goes on before it answers with
	// what it has (Truncated).
	searchBudget = 150 * time.Second
)

const searchColumnsQuery = "SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS " +
	"WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION"

// Search counts, table by table, the rows where some text column contains
// the term, across the whole database: one bound COUNT(*) per table,
// names from information_schema, the term as a LIKE value.
func (o *Ops) Search(ctx context.Context, p rpc.DBStudioSearchParams) (rpc.DBStudioSearchResult, error) {
	if p.Term == "" || len(p.Term) > maxSearchTerm {
		return rpc.DBStudioSearchResult{}, agentrpc.Invalid("search for 1 to %d characters", maxSearchTerm)
	}
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, true, browseTimeout)
	if err != nil {
		return rpc.DBStudioSearchResult{}, err
	}
	defer s.Close()
	rows, err := collect(ctx, s, 200000, searchColumnsQuery)
	if err != nil {
		return rpc.DBStudioSearchResult{}, sqlErr(s, err)
	}
	var order []string
	cols := map[string][]string{}
	for _, r := range rows {
		name := str(r[0])
		if !searchTypes[strings.ToLower(str(r[2]))] {
			if _, ok := cols[name]; !ok {
				order = append(order, name)
				cols[name] = nil
			}
			continue
		}
		if _, ok := cols[name]; !ok {
			order = append(order, name)
		}
		cols[name] = append(cols[name], str(r[1]))
	}
	if len(p.Tables) > 0 {
		var only []string
		for _, want := range p.Tables {
			if _, ok := cols[want]; !ok {
				return rpc.DBStudioSearchResult{}, agentrpc.NotFound("no table %q", want)
			}
			only = append(only, want)
		}
		order = only
	}
	out := rpc.DBStudioSearchResult{Tables: []rpc.DBStudioSearchHit{}}
	deadline := time.Now().Add(searchBudget)
	value := "%" + likeEscape(p.Term) + "%"
	for _, name := range order {
		if out.Searched >= maxSearchTables || time.Now().After(deadline) {
			out.Truncated = true
			break
		}
		out.Searched++
		if len(cols[name]) == 0 {
			continue
		}
		b := &stmt{}
		b.sql("SELECT COUNT(*) FROM ").ident(name).sql(" WHERE ")
		for i, c := range cols[name] {
			if i > 0 {
				b.sql(" OR ")
			}
			b.ident(c).sql(" LIKE ").arg(value).sql(" ESCAPE '!'")
		}
		n, err := collect(ctx, s, 1, b.text(), b.args...)
		if err != nil {
			return rpc.DBStudioSearchResult{}, sqlErr(s, err)
		}
		if len(n) == 1 && num(n[0][0]) > 0 {
			out.Tables = append(out.Tables, rpc.DBStudioSearchHit{Table: name, Matches: num(n[0][0])})
		}
	}
	return out, nil
}

// ImportSQL imports an uploaded .sql file as the signed-in user (SPEC §11):
// the sandboxed client with the user's own password, no rollback point.
func (o *Ops) ImportSQL(ctx context.Context, p rpc.DBStudioImportSQLParams) (rpc.DBImportResult, error) {
	if err := validFile(p.Name); err != nil || !strings.HasSuffix(strings.ToLower(p.Name), ".sql") {
		return rpc.DBImportResult{}, agentrpc.Invalid("choose an uploaded .sql file")
	}
	login, err := o.credentials(p.Site, p.Login)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	return o.DB.ImportAsLogin(ctx, p.Site, login, p.Database, importsDir+"/"+p.Name)
}
