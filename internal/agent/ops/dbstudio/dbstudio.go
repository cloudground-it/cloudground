// Package dbstudio is Quarry, the panel's database manager for a site:
// browse tables, structure and rows; edit rows by primary key in a
// transaction; guided ALTERs; streamed export and import; WordPress tools.
// Every statement runs in an ops/db Session as an account scoped to the
// site's own database. SPEC.md in this directory is the contract.
package dbstudio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

// Ops holds what the handlers use.
type Ops struct {
	DB *db.Ops
	// Open opens a site's tree; what it creates belongs to the site user.
	Open func(state.Site) (*sitefs.Root, error)
	// WP runs wp-cli as the site's user against its live release (ops/wp).
	WP func(ctx context.Context, s state.Site, args ...string) ([]byte, error)
	// Refresh makes a change to the site's database visible to the live
	// site (ops/wp Refresh).
	Refresh func(ctx context.Context, s state.Site) error
	// LogRoot is where each site's logs are, <LogRoot>/<domain>/: a cleanup
	// writes its result to cleanup.log there ("" writes none).
	LogRoot string
	// MaxFileBytes caps an export or an upload (0 means 16 GiB).
	MaxFileBytes int64
	Log          *slog.Logger
	Now          func() time.Time

	// logins are the signed-in database users' sessions (login.go).
	logins logins
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.DBStudioTables, o.Tables)
	agentrpc.Handle(s, agentrpc.DBStudioStructure, o.Structure)
	agentrpc.Handle(s, agentrpc.DBStudioRows, o.Rows)
	agentrpc.Handle(s, agentrpc.DBStudioEdit, o.Edit)
	agentrpc.Handle(s, agentrpc.DBStudioAlter, o.Alter)
	agentrpc.Handle(s, agentrpc.DBStudioExport, o.Export)
	agentrpc.Handle(s, agentrpc.DBStudioFiles, o.Files)
	agentrpc.Handle(s, agentrpc.DBStudioReadChunk, o.ReadChunk)
	agentrpc.Handle(s, agentrpc.DBStudioUploadChunk, o.UploadChunk)
	agentrpc.Handle(s, agentrpc.DBStudioDeleteFile, o.DeleteFile)
	agentrpc.Handle(s, agentrpc.DBStudioImportCSV, o.ImportCSV)
	agentrpc.Handle(s, agentrpc.DBStudioWPAutoload, o.Autoload)
	agentrpc.Handle(s, agentrpc.DBStudioWPSearchReplace, o.SearchReplace)
	agentrpc.Handle(s, agentrpc.DBStudioWPTransients, o.Transients)
	agentrpc.Handle(s, agentrpc.DBStudioWPCleanup, o.Cleanup)
	agentrpc.Handle(s, agentrpc.DBStudioQuery, o.Query)
	agentrpc.Handle(s, agentrpc.DBStudioSimulate, o.Simulate)
	agentrpc.Handle(s, agentrpc.DBStudioSearch, o.Search)
	agentrpc.Handle(s, agentrpc.DBStudioOpen, o.SignIn)
	agentrpc.Handle(s, agentrpc.DBStudioClose, o.SignOut)
	agentrpc.Handle(s, agentrpc.DBStudioDatabases, o.Databases)
	agentrpc.Handle(s, agentrpc.DBStudioImportSQL, o.ImportSQL)
}

// Statement times (SPEC §9).
const (
	browseTimeout = 60 * time.Second
	editTimeout   = 60 * time.Second
	alterTimeout  = 10 * time.Minute
	bulkTimeout   = 2*time.Hour + 50*time.Minute
)

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// session opens a session on one of the site's databases ("" is its
// primary); a name the agent's record does not give the site is refused.
func (o *Ops) session(ctx context.Context, s state.Site, database string, readOnly bool, timeout time.Duration) (*db.Session, error) {
	return o.DB.OpenSession(ctx, s, database, readOnly, timeout)
}

// sqlErr turns a statement's failure into what the caller is shown: the
// server's own message (a denied command, a duplicate key, a timeout) as
// invalid; anything else stays an internal error.
func sqlErr(s *db.Session, err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) || errors.Is(err, context.DeadlineExceeded) {
		return agentrpc.Invalid("%s", s.ServerMessage(err))
	}
	return err
}

// quoteIdent quotes a name the server gave, doubling any backtick.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// literal renders a bound value as SQL text, for display: NULL, a number,
// or a quoted string with the server's escapes.
func literal(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case *string:
		if x == nil {
			return "NULL"
		}
		return quoteString(*x)
	case string:
		return quoteString(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return fmt.Sprint(v)
}

func quoteString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			b.WriteString("''")
		case '\\':
			b.WriteString(`\\`)
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// stmt is a statement built twice at once: the text sent with its bound
// values, and the same statement with each value as a literal, for people.
// Whatever is shown is what runs (SPEC §5).
type stmt struct {
	q, show strings.Builder
	args    []any
}

// sql appends fixed text.
func (b *stmt) sql(s string) *stmt {
	b.q.WriteString(s)
	b.show.WriteString(s)
	return b
}

// ident appends a name the server gave, quoted.
func (b *stmt) ident(name string) *stmt { return b.sql(quoteIdent(name)) }

// arg appends a bound value.
func (b *stmt) arg(v any) *stmt {
	if p, ok := v.(*string); ok {
		if p == nil {
			v = nil
		} else {
			v = *p
		}
	}
	b.q.WriteByte('?')
	b.show.WriteString(literal(v))
	b.args = append(b.args, v)
	return b
}

func (b *stmt) text() string    { return b.q.String() }
func (b *stmt) display() string { return b.show.String() }

// collect runs a query and returns its rows as text (nil is NULL), at most
// limit of them. Rows past the limit are read and dropped rather than cut
// off: ending a result early drops the connection, and the session goes on
// to its next statement.
func collect(ctx context.Context, s *db.Session, limit int, q string, args ...any) ([][]*string, error) {
	var out [][]*string
	err := s.Each(ctx, q, args, func(_ []string, row [][]byte) error {
		if len(out) >= limit {
			return nil
		}
		r := make([]*string, len(row))
		for i, v := range row {
			if v != nil {
				t := string(v)
				r[i] = &t
			}
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func num(p *string) int64 {
	n, _ := strconv.ParseInt(str(p), 10, 64)
	return n
}
