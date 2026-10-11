package dbstudio

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

var ctx = context.Background()

// fakeTable is a table the fake server knows.
type fakeTable struct {
	typ    string
	engine string // "" is InnoDB
	// estimate is the server's row estimate ("" is 10).
	estimate string
	cols     [][]string // name, column type, data type, nullable, key
	pk       []string
	rows     [][]*string
}

type call struct {
	q    string
	args []any
}

// fakeServer answers the information_schema queries from its tables and
// records everything else.
type fakeServer struct {
	mu       sync.Mutex
	tables   map[string]*fakeTable
	queries  []call
	execs    []call
	readOnly []bool
	// exec, when set, answers Exec (statements are recorded first).
	exec func(q string, args []any) (int64, error)
	// selectRows answers any other SELECT.
	selectRows [][]*string
	// inTransaction answers SELECT @@in_transaction ("" is "1").
	inTransaction string
	// sqlMode answers SELECT @@SESSION.sql_mode.
	sqlMode string
	// query, when set, answers Query (recorded in queries first).
	query func(q string) (rpc.DBQueryResult, error)
	// closed counts the connections closed.
	closed int
}

func sp(s string) *string { return &s }

func (f *fakeServer) each(q string, args []any, fn func([]string, [][]byte) error) error {
	f.mu.Lock()
	var rows [][]*string
	switch q {
	case tableQuery:
		for name, t := range f.tables {
			// The real information_schema may compare without case.
			if strings.EqualFold(name, args[0].(string)) {
				est := t.estimate
				if est == "" {
					est = "10"
				}
				rows = append(rows, []*string{sp(name), sp(t.typ), sp(est)})
			}
		}
	case columnsQuery:
		if t := f.tables[args[0].(string)]; t != nil {
			for _, c := range t.cols {
				rows = append(rows, []*string{sp(c[0]), sp(c[1]), sp(c[2]), sp(c[3]), nil, sp(c[4]), sp(""), sp(""), sp("")})
			}
		}
	case primaryQuery:
		if t := f.tables[args[0].(string)]; t != nil {
			for _, k := range t.pk {
				rows = append(rows, []*string{sp(k)})
			}
		}
	case indexNamesQuery:
		rows = [][]*string{{sp("PRIMARY")}}
	case searchColumnsQuery:
		names := make([]string, 0, len(f.tables))
		for name := range f.tables {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, c := range f.tables[name].cols {
				rows = append(rows, []*string{sp(name), sp(c[0]), sp(c[2])})
			}
		}
	case simModeQuery:
		f.queries = append(f.queries, call{q, args})
		rows = [][]*string{{sp(f.sqlMode)}}
	case "SELECT @@in_transaction":
		f.queries = append(f.queries, call{q, args})
		v := f.inTransaction
		if v == "" {
			v = "1"
		}
		rows = [][]*string{{sp(v)}}
	default:
		f.queries = append(f.queries, call{q, args})
		switch {
		case strings.HasPrefix(q, "SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, ENGINE FROM information_schema.TABLES"):
			want := map[string]bool{}
			for _, a := range args[len(args)/2:] {
				want[strings.ToLower(a.(string))] = true
			}
			names := make([]string, 0, len(f.tables))
			for name := range f.tables {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				t := f.tables[name]
				if !want[strings.ToLower(name)] {
					continue
				}
				var engine *string
				if t.typ == "BASE TABLE" {
					engine = sp("InnoDB")
					if t.engine != "" {
						engine = sp(t.engine)
					}
				}
				rows = append(rows, []*string{sp("site_4"), sp(name), sp(t.typ), engine})
			}
		case strings.HasPrefix(q, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA"):
			rows = [][]*string{{sp("site_4")}}
		case strings.HasPrefix(q, "SELECT COUNT(*)"):
			rows = [][]*string{{sp(fmt.Sprint(len(f.selectRows)))}}
		default:
			rows = f.selectRows
		}
	}
	f.mu.Unlock()
	for _, r := range rows {
		raw := make([][]byte, len(r))
		for i, v := range r {
			if v != nil {
				raw[i] = []byte(*v)
			}
		}
		if err := fn(nil, raw); err != nil {
			if errors.Is(err, db.ErrStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (f *fakeServer) doExec(q string, args []any) (int64, error) {
	f.mu.Lock()
	f.execs = append(f.execs, call{q, args})
	fn := f.exec
	f.mu.Unlock()
	if fn != nil {
		return fn(q, args)
	}
	if strings.HasPrefix(q, "INSERT") {
		return int64(strings.Count(q, "), (") + 1), nil
	}
	return 1, nil
}

type fakeConn struct{ f *fakeServer }

func (c fakeConn) ID() int64 { return 7 }
func (c fakeConn) Query(_ context.Context, _ db.Limits, q string, args ...any) (rpc.DBQueryResult, error) {
	c.f.mu.Lock()
	c.f.queries = append(c.f.queries, call{q, args})
	fn := c.f.query
	c.f.mu.Unlock()
	if fn == nil {
		return rpc.DBQueryResult{}, errors.New("not used")
	}
	return fn(q)
}
func (c fakeConn) Each(_ context.Context, q string, args []any, fn func([]string, [][]byte) error) error {
	return c.f.each(q, args, fn)
}
func (c fakeConn) Exec(_ context.Context, q string, args ...any) (int64, error) {
	return c.f.doExec(q, args)
}
func (c fakeConn) Close() error {
	c.f.mu.Lock()
	c.f.closed++
	c.f.mu.Unlock()
	return nil
}

// recMariaDB records the mode of every scoped account.
type recMariaDB struct {
	*host.FakeMariaDB
	f *fakeServer
}

func (m recMariaDB) ScopedAccount(ctx context.Context, database string, readOnly bool) (*host.ScopedAccount, error) {
	m.f.mu.Lock()
	m.f.readOnly = append(m.f.readOnly, readOnly)
	m.f.mu.Unlock()
	return m.FakeMariaDB.ScopedAccount(ctx, database, readOnly)
}

type fixture struct {
	o    *Ops
	f    *fakeServer
	m    *host.FakeMariaDB
	site state.Site
	wp   [][]string
	wpFn func(args []string) ([]byte, error)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "shop.example.com")
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx := &fixture{f: &fakeServer{tables: map[string]*fakeTable{
		"wp_posts": {typ: "BASE TABLE", pk: []string{"ID"}, cols: [][]string{
			{"ID", "bigint(20) unsigned", "bigint", "NO", "PRI"},
			{"post_title", "text", "text", "NO", ""},
			{"post_status", "varchar(20)", "varchar", "NO", "MUL"},
			{"post_parent", "bigint(20)", "bigint", "YES", ""},
			{"thumb", "longblob", "longblob", "YES", ""},
		}},
		"wp_options": {typ: "BASE TABLE", pk: []string{"option_id"}, cols: [][]string{
			{"option_id", "bigint(20)", "bigint", "NO", "PRI"},
			{"option_name", "varchar(191)", "varchar", "NO", "UNI"},
			{"option_value", "longtext", "longtext", "NO", ""},
			{"autoload", "varchar(20)", "varchar", "NO", "MUL"},
		}},
		"wp_log":  {typ: "BASE TABLE", cols: [][]string{{"line", "text", "text", "YES", ""}}},
		"v_posts": {typ: "VIEW", cols: [][]string{{"ID", "bigint(20)", "bigint", "NO", ""}}},
	}}, m: host.NewFakeMariaDB()}
	fx.m.Rows[db.ScanDatabasesQuery] = []string{"site_4"}
	fx.m.Rows[db.ScanUsersQuery] = []string{"site_4"}
	fx.site = state.Site{ID: 4, Domain: "shop.example.com", Type: state.SiteWordPress, RootPath: root, SystemUser: "cg-site-4",
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "site_4", User: "site_4"}}}
	dbo := &db.Ops{
		MariaDB: recMariaDB{fx.m, fx.f},
		WorkDir: t.TempDir(),
		Dial:    func(context.Context, db.DialSpec) (db.Conn, error) { return fakeConn{fx.f}, nil },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	fx.o = &Ops{
		DB: dbo,
		Open: func(s state.Site) (*sitefs.Root, error) {
			return sitefs.Open(s.RootPath, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
		},
		WP: func(_ context.Context, _ state.Site, args ...string) ([]byte, error) {
			fx.wp = append(fx.wp, args)
			if fx.wpFn != nil {
				return fx.wpFn(args)
			}
			return []byte("wp_\n"), nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return fx
}

func code(err error) string {
	var e *agentrpc.Error
	if errors.As(err, &e) {
		return string(e.Code)
	}
	return "internal"
}

// sent is every statement text the server received, queries and execs.
func (fx *fixture) sent() string {
	fx.f.mu.Lock()
	defer fx.f.mu.Unlock()
	var b strings.Builder
	for _, c := range append(append([]call{}, fx.f.queries...), fx.f.execs...) {
		b.WriteString(c.q + "\n")
	}
	return b.String()
}

// A name that is not one of the table's own columns, however it is
// spelled, is refused before any statement that would carry it is built:
// nothing from the request reaches the SQL text.
func TestIdentifierInjectionIsRefused(t *testing.T) {
	fx := newFixture(t)
	evil := []string{
		"ID`; DROP TABLE wp_users; -- ",
		"ID` DESC, (SELECT SLEEP(10)) --",
		"post_title`=`post_title` OR 1=1 OR `",
		"id", // case differs: not the server's name
		"",
	}
	for _, name := range evil {
		_, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", Sort: []rpc.DBSort{{Column: name}}})
		if c := code(err); c != "not_found" {
			t.Errorf("sort by %q: %v (%s)", name, err, c)
		}
		_, err = fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", Filters: []rpc.DBFilter{{Column: name, Op: "eq", Value: "1"}}})
		if c := code(err); c != "not_found" {
			t.Errorf("filter on %q: %v (%s)", name, err, c)
		}
		_, err = fx.o.Edit(ctx, rpc.DBStudioEditParams{Site: fx.site, Table: "wp_posts",
			Changes: []rpc.DBChange{{Op: "update", Key: map[string]string{"ID": "1"}, Values: map[string]*string{name: sp("x")}}}})
		if c := code(err); c != "not_found" {
			t.Errorf("edit column %q: %v (%s)", name, err, c)
		}
		_, err = fx.o.Alter(ctx, rpc.DBStudioAlterParams{Site: fx.site, Table: "wp_posts", Kind: "add_column",
			Column: &rpc.DBNewColumn{Name: "extra", Type: "INT", After: name}})
		if c := code(err); c != "not_found" && name != "" {
			t.Errorf("alter after %q: %v (%s)", name, err, c)
		}
	}
	for _, table := range []string{"wp_posts`; DROP TABLE wp_users; --", "WP_POSTS", "mysql`.`user", "../other"} {
		_, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: table})
		if c := code(err); c != "not_found" && c != "invalid" {
			t.Errorf("table %q: %v (%s)", table, err, c)
		}
		_, err = fx.o.Structure(ctx, rpc.DBStudioTableParams{Site: fx.site, Table: table})
		if c := code(err); c != "not_found" && c != "invalid" {
			t.Errorf("structure of %q: %v (%s)", table, err, c)
		}
	}
	// An unknown filter operator is refused too.
	_, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", Filters: []rpc.DBFilter{{Column: "ID", Op: "= 1 OR 1=1 --"}}})
	if code(err) != "invalid" {
		t.Errorf("an unknown operator: %v", err)
	}
	if s := fx.sent(); strings.Contains(s, "DROP") || strings.Contains(s, "SLEEP") || strings.Contains(s, "OR 1=1") {
		t.Fatalf("an injected name reached the server:\n%s", s)
	}
}

// The page's SELECT quotes the server's names, binds every value, cuts long
// columns with LEFT() and orders by the primary key last.
func TestRowsBuildsBoundSQL(t *testing.T) {
	fx := newFixture(t)
	fx.f.selectRows = [][]*string{
		{sp("1"), sp(strings.Repeat("x", cellBytes+1)), sp("publish"), nil, sp("\x00\xff")},
	}
	res, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", Page: 2, PageSize: 10,
		Sort:    []rpc.DBSort{{Column: "post_status", Desc: true}},
		Filters: []rpc.DBFilter{{Column: "post_title", Op: "contains", Value: "50%_off!"}, {Column: "post_parent", Op: "null"}}})
	if err != nil {
		t.Fatal(err)
	}
	q := fx.f.queries[1] // after the count
	want := "SELECT `ID`, LEFT(`post_title`, 4097) AS `post_title`, LEFT(`post_status`, 4097) AS `post_status`, `post_parent`, " +
		"LEFT(`thumb`, 2049) AS `thumb` FROM `wp_posts` WHERE `post_title` LIKE ? ESCAPE '!' AND `post_parent` IS NULL " +
		"ORDER BY `post_status` DESC, `ID` LIMIT ? OFFSET ?"
	if q.q != want {
		t.Fatalf("query:\n%s\nwant:\n%s", q.q, want)
	}
	if fmt.Sprint(q.args) != "[%50!%!_off!!% 10 20]" {
		t.Fatalf("args = %v", q.args)
	}
	if !strings.Contains(res.SQL, "LIKE '%50!%!_off!!%' ESCAPE '!'") || !strings.HasSuffix(res.SQL, "LIMIT 10 OFFSET 20") {
		t.Fatalf("shown SQL: %s", res.SQL)
	}
	r := res.Rows[0]
	if r.Values[3] != nil || *r.Values[4] != "0x00FF" || len(*r.Values[1]) != cellBytes {
		t.Fatalf("cells: %v", r.Values)
	}
	if fmt.Sprint(r.Cut) != "[1 4]" {
		t.Fatalf("cut = %v", r.Cut)
	}
	if res.Total != 1 || fmt.Sprint(res.PrimaryKey) != "[ID]" {
		t.Fatalf("total %d, pk %v", res.Total, res.PrimaryKey)
	}
	// Reading is always read-only.
	for _, ro := range fx.f.readOnly {
		if !ro {
			t.Fatal("a read used a read-write account")
		}
	}
	for _, bad := range []rpc.DBStudioRowsParams{{PageSize: 501}, {Page: -1}, {Page: 1 << 30, PageSize: 500}} {
		bad.Site, bad.Table = fx.site, "wp_posts"
		if _, err := fx.o.Rows(ctx, bad); code(err) != "invalid" {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// Paging as Browse offers it (25, 50, 100, 250, 500 rows a page): the page
// is LIMIT size OFFSET page*size, the total exact by COUNT(*) for a small
// table and the server's estimate, marked, for a big one without filters.
func TestRowsPaging(t *testing.T) {
	for _, size := range []int{25, 50, 100, 250, 500} {
		fx := newFixture(t)
		res, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", Page: 3, PageSize: size})
		if err != nil {
			t.Fatal(err)
		}
		q := fx.f.queries[len(fx.f.queries)-1]
		if fmt.Sprint(q.args) != fmt.Sprintf("[%d %d]", size, 3*size) || res.PageSize != size || res.Page != 3 {
			t.Fatalf("size %d: args %v, result %d/%d", size, q.args, res.Page, res.PageSize)
		}
		if res.TotalEstimated || !strings.HasPrefix(fx.f.queries[0].q, "SELECT COUNT(*)") {
			t.Fatalf("size %d: a small table is not counted: %+v", size, res)
		}
	}
	fx := newFixture(t)
	fx.f.tables["wp_posts"].estimate = "2500000"
	res, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", PageSize: 25})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TotalEstimated || res.Total != 2500000 || strings.Contains(fx.sent(), "COUNT(*)") {
		t.Fatalf("a big table was counted: %+v\n%s", res, fx.sent())
	}
	res, err = fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", PageSize: 25,
		Filters: []rpc.DBFilter{{Column: "post_status", Op: "eq", Value: "draft"}}})
	if err != nil || res.TotalEstimated {
		t.Fatalf("a filtered page is counted exactly: %+v %v", res, err)
	}
	// The last page Browse can ask for stays within the offset limit.
	if _, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", PageSize: 500, Page: maxOffset / 500}); err != nil {
		t.Fatalf("the furthest page: %v", err)
	}
	if _, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_posts", PageSize: 500, Page: maxOffset/500 + 1}); code(err) != "invalid" {
		t.Fatalf("past the furthest page: %v", err)
	}
}

// A dry run returns exactly the statements a real run executes, opens a
// read-only account and writes nothing.
func TestEditPreviewIsWhatRuns(t *testing.T) {
	fx := newFixture(t)
	p := rpc.DBStudioEditParams{Site: fx.site, Table: "wp_posts", Changes: []rpc.DBChange{
		{Op: "update", Key: map[string]string{"ID": "5"}, Values: map[string]*string{"post_title": sp("It's a \\ test"), "post_parent": nil}},
		{Op: "insert", Values: map[string]*string{"post_status": sp("draft"), "post_title": sp("new")}},
		{Op: "delete", Key: map[string]string{"ID": "9"}},
	}}
	p.DryRun = true
	dry, err := fx.o.Edit(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Executed || len(fx.f.execs) != 0 || !fx.f.readOnly[len(fx.f.readOnly)-1] {
		t.Fatalf("a dry run wrote: %+v %v", dry, fx.f.execs)
	}
	want := []string{
		"START TRANSACTION",
		"UPDATE `wp_posts` SET `post_title` = 'It''s a \\\\ test', `post_parent` = NULL WHERE `ID` = '5' LIMIT 1",
		"INSERT INTO `wp_posts` (`post_title`, `post_status`) VALUES ('new', 'draft')",
		"DELETE FROM `wp_posts` WHERE `ID` = '9' LIMIT 1",
		"COMMIT",
	}
	if strings.Join(dry.SQL, "\n") != strings.Join(want, "\n") {
		t.Fatalf("preview:\n%s\nwant:\n%s", strings.Join(dry.SQL, "\n"), strings.Join(want, "\n"))
	}
	p.DryRun = false
	run, err := fx.o.Edit(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Executed || run.Affected != 3 || fx.f.readOnly[len(fx.f.readOnly)-1] {
		t.Fatalf("run: %+v (read-only %v)", run, fx.f.readOnly)
	}
	if strings.Join(run.SQL, "\n") != strings.Join(dry.SQL, "\n") {
		t.Fatalf("the run's SQL differs from the preview")
	}
	got := []string{}
	for _, e := range fx.f.execs {
		got = append(got, e.q+" "+fmt.Sprint(e.args))
	}
	wantExec := []string{
		"START TRANSACTION []",
		"UPDATE `wp_posts` SET `post_title` = ?, `post_parent` = ? WHERE `ID` = ? LIMIT 1 [It's a \\ test <nil> 5]",
		"INSERT INTO `wp_posts` (`post_title`, `post_status`) VALUES (?, ?) [new draft]",
		"DELETE FROM `wp_posts` WHERE `ID` = ? LIMIT 1 [9]",
		"COMMIT []",
	}
	if strings.Join(got, "\n") != strings.Join(wantExec, "\n") {
		t.Fatalf("executed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantExec, "\n"))
	}
}

// A change that fails, or that does not match exactly one row, rolls the
// whole batch back.
func TestEditRollsBackTheBatch(t *testing.T) {
	for name, fail := range map[string]func(q string) (int64, error){
		"server error": func(q string) (int64, error) {
			if strings.HasPrefix(q, "DELETE") {
				return 0, &mysql.MySQLError{Number: 1451, Message: "Cannot delete or update a parent row"}
			}
			return 1, nil
		},
		"row gone": func(q string) (int64, error) {
			if strings.HasPrefix(q, "DELETE") {
				return 0, nil
			}
			return 1, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t)
			fx.f.exec = func(q string, _ []any) (int64, error) { return fail(q) }
			_, err := fx.o.Edit(ctx, rpc.DBStudioEditParams{Site: fx.site, Table: "wp_posts", Changes: []rpc.DBChange{
				{Op: "update", Key: map[string]string{"ID": "5"}, Values: map[string]*string{"post_title": sp("a")}},
				{Op: "delete", Key: map[string]string{"ID": "6"}},
			}})
			if err == nil {
				t.Fatal("the batch succeeded")
			}
			var last []string
			for _, e := range fx.f.execs {
				last = append(last, strings.Fields(e.q)[0])
			}
			if strings.Join(last, " ") != "START UPDATE DELETE ROLLBACK" {
				t.Fatalf("statements: %v (%v)", last, err)
			}
		})
	}
}

// Edits need a primary key given whole, refuse views and binary values,
// and a read-only caller gets a read-only account.
func TestEditRules(t *testing.T) {
	fx := newFixture(t)
	cases := map[string]rpc.DBStudioEditParams{
		"no primary key":  {Table: "wp_log", Changes: []rpc.DBChange{{Op: "delete", Key: map[string]string{"line": "x"}}}},
		"partial key":     {Table: "wp_posts", Changes: []rpc.DBChange{{Op: "delete", Key: map[string]string{}}}},
		"extra key":       {Table: "wp_posts", Changes: []rpc.DBChange{{Op: "delete", Key: map[string]string{"ID": "1", "post_status": "x"}}}},
		"view":            {Table: "v_posts", Changes: []rpc.DBChange{{Op: "insert"}}},
		"binary":          {Table: "wp_posts", Changes: []rpc.DBChange{{Op: "insert", Values: map[string]*string{"thumb": sp("x")}}}},
		"unknown op":      {Table: "wp_posts", Changes: []rpc.DBChange{{Op: "truncate"}}},
		"no changes":      {Table: "wp_posts"},
		"empty update":    {Table: "wp_posts", Changes: []rpc.DBChange{{Op: "update", Key: map[string]string{"ID": "1"}}}},
		"delete w/ value": {Table: "wp_posts", Changes: []rpc.DBChange{{Op: "delete", Key: map[string]string{"ID": "1"}, Values: map[string]*string{"post_title": nil}}}},
	}
	for name, p := range cases {
		p.Site = fx.site
		if _, err := fx.o.Edit(ctx, p); code(err) != "invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(fx.f.execs) != 0 {
		t.Fatalf("a refused edit sent %v", fx.f.execs)
	}
	fx.f.readOnly = nil
	fx.o.Edit(ctx, rpc.DBStudioEditParams{Site: fx.site, Table: "wp_posts", ReadOnly: true,
		Changes: []rpc.DBChange{{Op: "delete", Key: map[string]string{"ID": "1"}}}})
	if len(fx.f.readOnly) != 1 || !fx.f.readOnly[0] {
		t.Fatalf("a read-only caller's edit used %v", fx.f.readOnly)
	}
	// Another site's database is never reachable: the site's own only.
	other := fx.site
	other.Config.Database.External = true
	if _, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: other, Table: "wp_posts"}); code(err) != "invalid" {
		t.Fatalf("an external database: %v", err)
	}
}

// The guided ALTER builds what it shows, from a fixed list of types, and
// quotes a default safely.
func TestAlter(t *testing.T) {
	fx := newFixture(t)
	res, err := fx.o.Alter(ctx, rpc.DBStudioAlterParams{Site: fx.site, Table: "wp_posts", Kind: "add_column", DryRun: true,
		Column: &rpc.DBNewColumn{Name: "note", Type: "varchar", Length: 100, Default: sp("it's"), After: "post_title"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "ALTER TABLE `wp_posts` ADD COLUMN `note` VARCHAR(100) NOT NULL DEFAULT 'it''s' AFTER `post_title`"; res.SQL != want || res.Executed {
		t.Fatalf("got %q", res.SQL)
	}
	if len(fx.f.execs) != 0 {
		t.Fatal("a dry run executed")
	}
	run, err := fx.o.Alter(ctx, rpc.DBStudioAlterParams{Site: fx.site, Table: "wp_posts", Kind: "add_index",
		Index: &rpc.DBNewIndex{Name: "status_title", Unique: true, Columns: []rpc.DBIndexColumn{{Name: "post_status"}, {Name: "post_title", Length: 50}}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "ALTER TABLE `wp_posts` ADD UNIQUE INDEX `status_title` (`post_status`, `post_title`(50))"; run.SQL != want || !run.Executed || fx.f.execs[0].q != want {
		t.Fatalf("got %q, executed %v", run.SQL, fx.f.execs)
	}
	bad := []rpc.DBNewColumn{
		{Name: "x", Type: "INT; DROP TABLE wp_users"},
		{Name: "x`y", Type: "INT"},
		{Name: "post_title", Type: "INT"},
		{Name: "x", Type: "VARCHAR"},
		{Name: "x", Type: "TEXT", Default: sp(`\'; DROP TABLE x; --`)},
		{Name: "x", Type: "INT", Default: sp("1; DROP TABLE x")},
		{Name: "x", Type: "DECIMAL", Length: 5, Scale: 6},
		{Name: "x", Type: "TEXT", Unsigned: true},
	}
	for _, c := range bad {
		c := c
		if _, err := fx.o.Alter(ctx, rpc.DBStudioAlterParams{Site: fx.site, Table: "wp_posts", Kind: "add_column", Column: &c, DryRun: true}); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
	if _, err := fx.o.Alter(ctx, rpc.DBStudioAlterParams{Site: fx.site, Table: "wp_posts", Kind: "add_index", DryRun: true,
		Index: &rpc.DBNewIndex{Name: "primary", Columns: []rpc.DBIndexColumn{{Name: "ID"}}}}); err == nil {
		t.Error("an index named PRIMARY was accepted")
	}
}

// Uploads are written chunk by chunk at the right offset, under
// shared/imports only, never through a symlink; exports are read back the
// same way.
func TestUploadAndReadChunks(t *testing.T) {
	fx := newFixture(t)
	up := func(name string, off int64, data string) (rpc.DBStudioUploadResult, error) {
		return fx.o.UploadChunk(ctx, rpc.DBStudioUploadParams{Site: fx.site, Name: name, Offset: off, Data: []byte(data)})
	}
	if _, err := up("dump.sql", 0, "CREATE TABLE a (x INT);\n"); err != nil {
		t.Fatal(err)
	}
	if r, err := up("dump.sql", 24, "INSERT INTO a VALUES (1);\n"); err != nil || r.Size != 50 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := up("dump.sql", 10, "x"); code(err) != "conflict" {
		t.Fatalf("a chunk at the wrong offset: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(fx.site.RootPath, "shared/imports/dump.sql"))
	if string(got) != "CREATE TABLE a (x INT);\nINSERT INTO a VALUES (1);\n" {
		t.Fatalf("file: %q", got)
	}
	for _, name := range []string{"../evil.sql", ".hidden.sql", "a/b.sql", "x.php", "x.sql\x00"} {
		if _, err := up(name, 0, "x"); code(err) != "invalid" {
			t.Errorf("%q: %v", name, err)
		}
	}
	fx.o.MaxFileBytes = 60
	if _, err := up("dump.sql", 50, strings.Repeat("y", 11)); code(err) != "invalid" {
		t.Errorf("past the limit: %v", err)
	}
	// A link planted where the upload goes is never followed.
	secret := filepath.Join(t.TempDir(), "secret.sql")
	os.WriteFile(secret, []byte("keep"), 0o600)
	os.Symlink(secret, filepath.Join(fx.site.RootPath, "shared/imports/link.sql"))
	if _, err := up("link.sql", 4, "more"); err == nil {
		t.Fatal("an upload appended through a symlink")
	}
	if b, _ := os.ReadFile(secret); string(b) != "keep" {
		t.Fatal("the link's target changed")
	}

	os.MkdirAll(filepath.Join(fx.site.RootPath, "shared/exports"), 0o750)
	os.WriteFile(filepath.Join(fx.site.RootPath, "shared/exports/site_4.sql"), []byte("0123456789"), 0o640)
	c, err := fx.o.ReadChunk(ctx, rpc.DBStudioChunkParams{Site: fx.site, Name: "site_4.sql", Offset: 4, Length: 4})
	if err != nil || string(c.Data) != "4567" || c.Size != 10 {
		t.Fatalf("%q %d %v", c.Data, c.Size, err)
	}
	os.Symlink(secret, filepath.Join(fx.site.RootPath, "shared/exports/link.sql"))
	if _, err := fx.o.ReadChunk(ctx, rpc.DBStudioChunkParams{Site: fx.site, Name: "link.sql", Length: 4}); err == nil {
		t.Fatal("a download followed a symlink")
	}
	files, err := fx.o.Files(ctx, rpc.DBStudioSiteParams{Site: fx.site})
	if err != nil || len(files.Exports) != 1 || len(files.Imports) != 1 {
		t.Fatalf("%+v %v", files, err)
	}
	if _, err := fx.o.DeleteFile(ctx, rpc.DBStudioDeleteParams{Site: fx.site, Dir: "exports", Name: "site_4.sql"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.o.DeleteFile(ctx, rpc.DBStudioDeleteParams{Site: fx.site, Dir: "../current", Name: "x.sql"}); code(err) != "invalid" {
		t.Fatalf("delete outside: %v", err)
	}
}

// A CSV export streams every row with a header and NULL as \N; the CSV
// import reads it back in one transaction, and a failure rolls back.
func TestCSVExportAndImport(t *testing.T) {
	fx := newFixture(t)
	fx.f.selectRows = [][]*string{{sp("1"), sp("siteurl"), sp("https://a, \"b\""), sp("yes")}, {sp("2"), sp("x"), nil, sp("no")}}
	res, err := fx.o.Export(ctx, rpc.DBStudioExportParams{Site: fx.site, Format: "csv", Tables: []string{"wp_options"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2 || !strings.HasPrefix(res.Name, "site_4-wp_options-") {
		t.Fatalf("%+v", res)
	}
	out, _ := os.ReadFile(filepath.Join(fx.site.RootPath, "shared/exports", res.Name))
	recs, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil || len(recs) != 3 || recs[0][2] != "option_value" || recs[1][2] != `https://a, "b"` || recs[2][2] != `\N` {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := fx.o.Export(ctx, rpc.DBStudioExportParams{Site: fx.site, Format: "csv", Tables: []string{"wp_options", "wp_posts"}}); code(err) != "invalid" {
		t.Fatalf("a CSV of two tables: %v", err)
	}

	os.MkdirAll(filepath.Join(fx.site.RootPath, "shared/imports"), 0o750)
	os.WriteFile(filepath.Join(fx.site.RootPath, "shared/imports/in.csv"), out, 0o640)
	got, err := fx.o.ImportCSV(ctx, rpc.DBStudioImportCSVParams{Site: fx.site, Table: "wp_options", Name: "in.csv"})
	if err != nil || got.Rows != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	ins := fx.f.execs[1]
	if ins.q != "INSERT INTO `wp_options` (`option_id`, `option_name`, `option_value`, `autoload`) VALUES (?, ?, ?, ?), (?, ?, ?, ?)" ||
		ins.args[6] != nil || ins.args[2] != `https://a, "b"` {
		t.Fatalf("%s %v", ins.q, ins.args)
	}
	if fx.f.execs[2].q != "COMMIT" {
		t.Fatalf("not committed: %v", fx.f.execs)
	}

	fx.f.execs = nil
	fx.f.exec = func(q string, _ []any) (int64, error) {
		if strings.HasPrefix(q, "INSERT") {
			return 0, &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
		}
		return 0, nil
	}
	if _, err := fx.o.ImportCSV(ctx, rpc.DBStudioImportCSVParams{Site: fx.site, Table: "wp_options", Name: "in.csv"}); code(err) != "invalid" {
		t.Fatalf("a failing import: %v", err)
	}
	if last := fx.f.execs[len(fx.f.execs)-1].q; last != "ROLLBACK" {
		t.Fatalf("not rolled back: %v", fx.f.execs)
	}
	os.WriteFile(filepath.Join(fx.site.RootPath, "shared/imports/bad.csv"), []byte("option_id,`x`\n1,2\n"), 0o640)
	if _, err := fx.o.ImportCSV(ctx, rpc.DBStudioImportCSVParams{Site: fx.site, Table: "wp_options", Name: "bad.csv"}); code(err) != "not_found" {
		t.Fatalf("an unknown column: %v", err)
	}
}

// WordPress tools: wp-cli with fixed options, terms that could read as
// options refused, a read-only caller only previews.
func TestWordPressTools(t *testing.T) {
	fx := newFixture(t)
	fx.wpFn = func(args []string) ([]byte, error) {
		if args[0] == "search-replace" {
			return []byte("Warning: something\n42\n"), nil
		}
		return []byte("wp_\n"), nil
	}
	res, err := fx.o.SearchReplace(ctx, rpc.DBStudioSearchReplaceParams{Site: fx.site, From: "http://old", To: "https://new", DryRun: true})
	if err != nil || res.Replacements != 42 || res.Executed {
		t.Fatalf("%+v %v", res, err)
	}
	if got := strings.Join(fx.wp[0], " "); got != "search-replace http://old https://new --all-tables-with-prefix --precise --skip-columns=guid --format=count --dry-run" {
		t.Fatalf("wp %s", got)
	}
	for _, p := range []rpc.DBStudioSearchReplaceParams{
		{From: "--require=/tmp/x.php", To: "y"}, {From: "x", To: "-y"}, {From: "x", To: "x"}, {From: "", To: "y"}, {From: "a\nb", To: "c"},
		{From: "a", To: "b", ReadOnly: true},
	} {
		p.Site = fx.site
		if _, err := fx.o.SearchReplace(ctx, p); code(err) != "invalid" {
			t.Errorf("%+v: %v", p, err)
		}
	}
	static := fx.site
	static.Type = state.SiteStatic
	if _, err := fx.o.Autoload(ctx, rpc.DBStudioSiteParams{Site: static}); code(err) != "invalid" {
		t.Errorf("a static site: %v", err)
	}
	fx.f.selectRows = [][]*string{{sp("3"), sp("1200"), sp("yes")}}
	al, err := fx.o.Autoload(ctx, rpc.DBStudioSiteParams{Site: fx.site})
	if err != nil || al.Table != "wp_options" {
		t.Fatalf("%+v %v", al, err)
	}
	if _, err := fx.o.Transients(ctx, rpc.DBStudioTransientsParams{Site: fx.site, ReadOnly: true}); code(err) != "invalid" {
		t.Fatalf("a read-only caller deleted transients: %v", err)
	}
	fx.wpFn = func([]string) ([]byte, error) { return []byte("evil`; DROP\n"), nil }
	if _, err := fx.o.Autoload(ctx, rpc.DBStudioSiteParams{Site: fx.site}); code(err) != "invalid" {
		t.Fatalf("a strange prefix: %v", err)
	}
}
