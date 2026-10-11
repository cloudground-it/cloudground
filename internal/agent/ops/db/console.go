package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/go-sql-driver/mysql"
)

// maxStatement is the longest console statement accepted.
const maxStatement = 1 << 20

func (o *Ops) consoleTimeout() (t time.Duration) {
	if o.ConsoleTimeout > 0 {
		return o.ConsoleTimeout
	}
	return DefaultConsoleTimeout
}

// Query runs one console statement on the site's own database as a scoped
// account (SPEC §3). A statement the server refuses is a result with the
// server's message, not an error.
func (o *Ops) Query(ctx context.Context, p rpc.DBQueryParams) (rpc.DBQueryResult, error) {
	database, err := o.SiteDatabase(ctx, p.Site, p.Database)
	if err != nil {
		return rpc.DBQueryResult{}, err
	}
	// The usual trailing semicolon is accepted; the protocol takes the
	// statement without it.
	stmt := strings.TrimRight(strings.TrimSpace(p.SQL), "; \t\r\n")
	if stmt == "" {
		return rpc.DBQueryResult{}, agentrpc.Invalid("empty query")
	}
	if len(stmt) > maxStatement {
		return rpc.DBQueryResult{}, agentrpc.Invalid("the statement is longer than %d KiB", maxStatement>>10)
	}
	timeout := o.consoleTimeout()
	s, err := o.session(ctx, database, p.ReadOnly, timeout)
	if err != nil {
		return rpc.DBQueryResult{}, err
	}
	defer s.Close()
	res, err := s.Query(ctx, ConsoleLimits, stmt)
	if err != nil {
		if ctx.Err() != nil {
			// The caller went away: nobody reads a message.
			return rpc.DBQueryResult{}, ctx.Err()
		}
		return rpc.DBQueryResult{Message: o.serverMessage(err, timeout)}, nil
	}
	return res, nil
}

// serverMessage is what the console shows for a failed statement: the
// server's own error, or that the time ran out. Anything else (a broken
// connection) is logged and shown as a plain failure.
func (o *Ops) serverMessage(err error, timeout time.Duration) string {
	var me *mysql.MySQLError
	switch {
	case errors.As(err, &me):
		if me.SQLState != [5]byte{} {
			return fmt.Sprintf("ERROR %d (%s): %s", me.Number, string(me.SQLState[:]), me.Message)
		}
		return fmt.Sprintf("ERROR %d: %s", me.Number, me.Message)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("the statement ran longer than %s and was stopped", timeout)
	}
	o.log().Warn("console statement failed", "err", err)
	return "the query failed"
}

// tablesQuery lists the session's database: a scoped account sees nothing
// else in information_schema.
const tablesQuery = "SELECT TABLE_NAME, COALESCE(TABLE_ROWS, 0), COALESCE(DATA_LENGTH, 0), COALESCE(INDEX_LENGTH, 0), " +
	"COALESCE(ENGINE, ''), COALESCE(TABLE_COLLATION, '') FROM information_schema.TABLES " +
	"WHERE TABLE_SCHEMA = DATABASE() ORDER BY DATA_LENGTH DESC, TABLE_NAME"

// Tables lists the site's tables, largest first, through a read-only
// session.
func (o *Ops) Tables(ctx context.Context, p rpc.DBTablesParams) (rpc.DBTablesResult, error) {
	database, err := o.SiteDatabase(ctx, p.Site, p.Database)
	if err != nil {
		return rpc.DBTablesResult{}, err
	}
	s, err := o.session(ctx, database, true, o.consoleTimeout())
	if err != nil {
		return rpc.DBTablesResult{}, err
	}
	defer s.Close()
	res, err := s.Query(ctx, Limits{MaxRows: 10000}, tablesQuery)
	if err != nil {
		return rpc.DBTablesResult{}, fmt.Errorf("list the tables of %s: %w", database, err)
	}
	out := rpc.DBTablesResult{Database: database, Tables: []rpc.DBTable{}, Truncated: res.Truncated}
	for _, r := range res.Rows {
		if len(r) != 6 {
			return rpc.DBTablesResult{}, fmt.Errorf("list the tables of %s: unexpected row %q", database, r)
		}
		out.Tables = append(out.Tables, rpc.DBTable{
			Name: r[0], Rows: atoi(r[1]), DataBytes: atoi(r[2]), IndexBytes: atoi(r[3]),
			Engine: r[4], Collation: r[5],
		})
	}
	return out, nil
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
