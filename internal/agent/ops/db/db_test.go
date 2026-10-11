package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

// scopedLog records the mode of every scoped account handed out.
type scopedLog struct {
	*host.FakeMariaDB
	mu       sync.Mutex
	readOnly []bool
}

func (s *scopedLog) ScopedAccount(ctx context.Context, database string, readOnly bool) (*host.ScopedAccount, error) {
	s.mu.Lock()
	s.readOnly = append(s.readOnly, readOnly)
	s.mu.Unlock()
	return s.FakeMariaDB.ScopedAccount(ctx, database, readOnly)
}

// fakeConn answers Query through fn.
type fakeConn struct {
	id     int64
	fn     func(ctx context.Context, stmt string) (rpc.DBQueryResult, error)
	closed bool
}

func (c *fakeConn) ID() int64 { return c.id }
func (c *fakeConn) Query(ctx context.Context, _ Limits, stmt string, _ ...any) (rpc.DBQueryResult, error) {
	return c.fn(ctx, stmt)
}
func (c *fakeConn) Each(context.Context, string, []any, func([]string, [][]byte) error) error {
	return errors.New("not scripted")
}
func (c *fakeConn) Exec(context.Context, string, ...any) (int64, error) {
	return 0, errors.New("not scripted")
}
func (c *fakeConn) Close() error { c.closed = true; return nil }

type fixture struct {
	o     *Ops
	m     *scopedLog
	site  state.Site
	conns []*fakeConn
	stmts []string
	query func(ctx context.Context, stmt string) (rpc.DBQueryResult, error)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "docs.example.com")
	for _, d := range []string{"shared", "current"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{m: &scopedLog{FakeMariaDB: host.NewFakeMariaDB()}}
	f.site = state.Site{ID: 3, Domain: "docs.example.com", RootPath: root, SystemUser: "cg-site-3",
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "site_3", User: "site_3"}}}
	f.query = func(context.Context, string) (rpc.DBQueryResult, error) {
		return rpc.DBQueryResult{Columns: []string{"1"}, Rows: [][]string{{"1"}}}, nil
	}
	sd := host.NewFakeSystemd()
	sd.SetUnit(host.UnitProps{Name: "mariadb"})
	f.o = &Ops{
		MariaDB: f.m,
		Systemd: sd,
		WorkDir: t.TempDir(),
		Open: func(s state.Site) (*sitefs.Root, error) {
			return sitefs.Open(s.RootPath, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
		},
		Dial: func(_ context.Context, d DialSpec) (Conn, error) {
			c := &fakeConn{id: int64(40 + len(f.conns)), fn: func(ctx context.Context, stmt string) (rpc.DBQueryResult, error) {
				f.stmts = append(f.stmts, stmt)
				return f.query(ctx, stmt)
			}}
			f.conns = append(f.conns, c)
			return c, nil
		},
		Probe:        func() (int64, int, error) { return 4096, 2, nil },
		TuneFile:     filepath.Join(t.TempDir(), "99-cloudground.cnf"),
		RegistryFile: filepath.Join(t.TempDir(), "databases.json"),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// The record says site_3 and its account are site 3's, and site_4
	// site 4's (another site of the server).
	writeRecord(t, f.o.RegistryFile, registryFile{Version: registryVersion,
		Databases: map[string]int64{"site_3": 3, "site_4": 4},
		Users: map[string]registryUser{
			"site_3": {Site: 3, Grants: map[string]string{"site_3": "all"}},
			"site_4": {Site: 4, Grants: map[string]string{"site_4": "all"}},
		}})
	return f
}

// writeRecord writes the agent's record of the sites' databases.
func writeRecord(t *testing.T, path string, f registryFile) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The console runs one statement as a scoped account of the right mode on
// the site's own database, and never anywhere else.
func TestQueryIsScopedToTheSitesDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.o.Query(ctx, rpc.DBQueryParams{Site: f.site, Database: "site_3", SQL: "  select 1 ;\n", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Query(ctx, rpc.DBQueryParams{Site: f.site, Database: "site_3", SQL: "update wp_options set x=1;"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.stmts, "|") != "select 1|update wp_options set x=1" {
		t.Fatalf("statements %q", f.stmts)
	}
	if fmt.Sprint(f.m.readOnly) != "[true false]" {
		t.Fatalf("account modes %v", f.m.readOnly)
	}
	if f.m.OpenAccounts() != 0 || !f.conns[0].closed || !f.conns[1].closed {
		t.Fatal("a session was left open")
	}

	// No primary: "" names nothing (a site's other databases are named).
	off := f.site
	off.Config.Database.Enabled = false
	for _, p := range []rpc.DBQueryParams{
		{Site: f.site, Database: "site_9", SQL: "select 1"},
		{Site: f.site, Database: "site_4", SQL: "select 1"},
		{Site: f.site, Database: "mysql", SQL: "select 1"},
		{Site: off, Database: "", SQL: "select 1"},
		{Site: f.site, Database: "site_3", SQL: " ; "},
		{Site: f.site, Database: "site_3", SQL: strings.Repeat("x", maxStatement+1)},
	} {
		if _, err := f.o.Query(ctx, p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Fatalf("%s/%.20q accepted: %v", p.Database, p.SQL, err)
		}
	}
	if len(f.m.readOnly) != 2 {
		t.Fatal("an account was made for a refused statement")
	}
}

// A statement that outlives the console is killed on the server: dropping
// the scoped account does not stop it.
func TestQueryKillsAStatementThatRunsTooLong(t *testing.T) {
	f := newFixture(t)
	f.o.ConsoleTimeout = 50 * time.Millisecond
	f.query = func(ctx context.Context, _ string) (rpc.DBQueryResult, error) {
		<-ctx.Done()
		return rpc.DBQueryResult{}, ctx.Err()
	}
	res, err := f.o.Query(context.Background(), rpc.DBQueryParams{Site: f.site, Database: "site_3", SQL: "SELECT SLEEP(600)", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "ran longer than 50ms") {
		t.Fatalf("message %q", res.Message)
	}
	killed := false
	for _, s := range f.m.Statements() {
		if s == "KILL ? [40]" {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("the connection was not killed: %v", f.m.Statements())
	}
	if f.m.OpenAccounts() != 0 {
		t.Fatal("the account was not dropped")
	}

	// A statement that ends in time is not killed.
	f.o.ConsoleTimeout = time.Minute
	f.query = func(context.Context, string) (rpc.DBQueryResult, error) { return rpc.DBQueryResult{Message: "OK"}, nil }
	before := len(f.m.Statements())
	if _, err := f.o.Query(context.Background(), rpc.DBQueryParams{Site: f.site, Database: "site_3", SQL: "select 1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	for _, s := range f.m.Statements()[before:] {
		if strings.HasPrefix(s, "KILL") {
			t.Fatal("a finished statement's connection was killed")
		}
	}
}

// The server's refusal is the console's answer, not an RPC failure.
func TestQueryReturnsTheServersMessage(t *testing.T) {
	f := newFixture(t)
	f.query = func(context.Context, string) (rpc.DBQueryResult, error) {
		e := &mysql.MySQLError{Number: 1142, Message: "INSERT command denied to user"}
		copy(e.SQLState[:], "42000")
		return rpc.DBQueryResult{}, e
	}
	res, err := f.o.Query(context.Background(), rpc.DBQueryParams{Site: f.site, Database: "site_3", SQL: "insert into t values (1)", ReadOnly: true})
	if err != nil || res.Message != "ERROR 1142 (42000): INSERT command denied to user" {
		t.Fatalf("%+v %v", res, err)
	}
}

// fakeRows feeds render.
type fakeRows struct {
	cols []string
	rows [][]any
	i    int
}

func (r *fakeRows) Columns() ([]string, error) { return r.cols, nil }
func (r *fakeRows) Next() bool                 { r.i++; return r.i <= len(r.rows) }
func (r *fakeRows) Err() error                 { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	for i, v := range r.rows[r.i-1] {
		p := dest[i].(*sql.RawBytes)
		if v == nil {
			*p = nil
		} else {
			*p = []byte(v.(string))
		}
	}
	return nil
}

// A result is cut at the row cap and at the byte cap: 5000 rows of large
// cells would otherwise exceed the RPC's result limit.
func TestRenderLimits(t *testing.T) {
	many := &fakeRows{cols: []string{"v"}}
	for i := 0; i < 10; i++ {
		many.rows = append(many.rows, []any{"x"})
	}
	res, err := render(many, Limits{MaxRows: 3})
	if err != nil || len(res.Rows) != 3 || !res.Truncated {
		t.Fatalf("%+v %v", res, err)
	}
	big := &fakeRows{cols: []string{"v", "n"}}
	for i := 0; i < 10; i++ {
		big.rows = append(big.rows, []any{strings.Repeat("b", 400), nil})
	}
	res, err = render(big, Limits{MaxRows: 100, MaxBytes: 1000})
	if err != nil || len(res.Rows) != 2 || !res.Truncated || res.Rows[0][1] != "NULL" {
		t.Fatalf("%d rows, truncated %v, %v", len(res.Rows), res.Truncated, err)
	}
	none, _ := render(&fakeRows{}, ConsoleLimits)
	if none.Message != "OK" {
		t.Fatalf("%+v", none)
	}
	empty, _ := render(&fakeRows{cols: []string{"a"}}, ConsoleLimits)
	if empty.Message != "OK (no rows)" {
		t.Fatalf("%+v", empty)
	}
}

func TestTables(t *testing.T) {
	f := newFixture(t)
	f.query = func(_ context.Context, stmt string) (rpc.DBQueryResult, error) {
		if stmt != tablesQuery {
			t.Fatalf("query %q", stmt)
		}
		return rpc.DBQueryResult{Rows: [][]string{{"wp_posts", "120", "65536", "16384", "InnoDB", "utf8mb4_unicode_ci"}}}, nil
	}
	res, err := f.o.Tables(context.Background(), rpc.DBTablesParams{Site: f.site})
	if err != nil {
		t.Fatal(err)
	}
	want := rpc.DBTable{Name: "wp_posts", Rows: 120, DataBytes: 65536, IndexBytes: 16384, Engine: "InnoDB", Collation: "utf8mb4_unicode_ci"}
	if res.Database != "site_3" || len(res.Tables) != 1 || res.Tables[0] != want || f.m.readOnly[0] != true {
		t.Fatalf("%+v", res)
	}
}

// An export is a new file in shared/exports; a second one in the same
// second gets its own name; a failed dump leaves nothing.
func TestExport(t *testing.T) {
	f := newFixture(t)
	f.m.Dumps["site_3"] = "-- MariaDB dump\n"
	ctx := context.Background()
	if _, err := f.o.Export(ctx, rpc.DBExportParams{Site: f.site, Database: "site_4"}); err == nil || !strings.Contains(err.Error(), "is not a database of") {
		t.Fatalf("another database exported: %v", err)
	}
	a, err := f.o.Export(ctx, rpc.DBExportParams{Site: f.site, Database: "site_3"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.o.Export(ctx, rpc.DBExportParams{Site: f.site, Database: "site_3"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path || !strings.HasPrefix(a.Path, filepath.Join(f.site.RootPath, "shared/exports/site_3-")) {
		t.Fatalf("paths %q %q", a.Path, b.Path)
	}
	got, _ := os.ReadFile(b.Path)
	st, _ := os.Stat(b.Path)
	if string(got) != "-- MariaDB dump\n" || b.SizeBytes != int64(len(got)) || st.Mode().Perm() != 0o640 {
		t.Fatalf("%q %d %v", got, b.SizeBytes, st.Mode())
	}

	f.m.OnDump = func(string, []string, io.Writer) error { return errors.New("mariadb-dump: exit status 2") }
	if _, err := f.o.Export(ctx, rpc.DBExportParams{Site: f.site, Database: "site_3"}); err == nil {
		t.Fatal("a failed dump succeeded")
	}
	f.m.OnDump = nil
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.o.Export(cancelled, rpc.DBExportParams{Site: f.site, Database: "site_3"}); err == nil {
		t.Fatal("an export ran on a cancelled request")
	}
	left, _ := os.ReadDir(filepath.Join(f.site.RootPath, "shared/exports"))
	if len(left) != 2 {
		t.Fatalf("exports left: %d", len(left))
	}

	// shared/exports planted as a link is refused, not followed.
	os.RemoveAll(filepath.Join(f.site.RootPath, "shared/exports"))
	os.Symlink(t.TempDir(), filepath.Join(f.site.RootPath, "shared/exports"))
	if _, err := f.o.Export(ctx, rpc.DBExportParams{Site: f.site, Database: "site_3"}); err == nil {
		t.Fatal("exported through a symlink")
	}
}

// An import is staged out of the site's tree, never through a link, and
// imported over the database with a rollback point.
func TestImport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	write := func(name, body string) {
		os.WriteFile(filepath.Join(f.site.RootPath, "shared", name), []byte(body), 0o644)
	}
	write("good.sql", "CREATE TABLE t (id int);")
	write("dump.txt", "x")
	os.Symlink("/etc/hostname", filepath.Join(f.site.RootPath, "shared", "link.sql"))
	for _, rel := range []string{"shared/dump.txt", "shared/link.sql", "shared/missing.sql", "../x.sql"} {
		if _, err := f.o.Import(ctx, rpc.DBImportParams{Site: f.site, Database: "site_3", RelPath: rel}); err == nil {
			t.Fatalf("%s imported", rel)
		}
	}
	if len(f.m.Imported) != 0 {
		t.Fatal("a refused file reached the database")
	}
	res, err := f.o.Import(ctx, rpc.DBImportParams{Site: f.site, Database: "site_3", RelPath: "shared/good.sql"})
	if err != nil || res.SizeBytes != 24 {
		t.Fatalf("%+v %v", res, err)
	}
	if f.m.Imported["site_3"] != "CREATE TABLE t (id int);" {
		t.Fatalf("imported %q", f.m.Imported)
	}
	for _, s := range f.m.Statements() {
		if strings.HasPrefix(s, "DROP DATABASE") {
			t.Fatal("an import over the database emptied it")
		}
	}
	if left, _ := filepath.Glob(filepath.Join(f.o.WorkDir, "rollback", "*")); len(left) != 0 {
		t.Fatalf("rollback point kept after success: %v", left)
	}
	if left, _ := filepath.Glob(filepath.Join(f.o.WorkDir, "db-import-*")); len(left) != 0 {
		t.Fatalf("staged copy kept: %v", left)
	}
}

// A failed import is rolled back exactly: the database is emptied before
// the rollback point goes back in, so tables the failed import created do
// not stay. Before, the rollback point was imported on top.
func TestFailedImportRollsBackExactly(t *testing.T) {
	f := newFixture(t)
	f.m.Dumps["site_3"] = "-- the original"
	var imports []string
	f.m.OnImport = func(_, sql string) error {
		imports = append(imports, sql)
		if len(imports) == 1 {
			return errors.New("ERROR at line 2: Not allowed in the sandbox mode")
		}
		return nil
	}
	f.m.OnQuery = func(q string, _ []any) ([]string, error) {
		if strings.Contains(q, "SCHEMATA") {
			return []string{"latin1 latin1_swedish_ci"}, nil
		}
		return nil, nil
	}
	dump := filepath.Join(t.TempDir(), "in.sql")
	os.WriteFile(dump, []byte("CREATE TABLE cg_e2e_import (id INT);\nsystem id\n"), 0o600)
	_, err := f.o.Replace(context.Background(), "site_3", dump, Over)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("err = %v", err)
	}
	if len(imports) != 2 || imports[1] != "-- the original" {
		t.Fatalf("imports %q", imports)
	}
	st := strings.Join(f.m.Statements(), "\n")
	if !strings.Contains(st, "DROP DATABASE IF EXISTS `site_3`\nCREATE DATABASE `site_3` CHARACTER SET latin1 COLLATE latin1_swedish_ci") {
		t.Fatalf("the database was not emptied before the rollback:\n%s", st)
	}
	if left, _ := filepath.Glob(filepath.Join(f.o.WorkDir, "rollback", "*")); len(left) != 0 {
		t.Fatalf("rollback point kept after a good rollback: %v", left)
	}
}

// A rollback that fails too keeps the only copy of the original and says
// where it is.
func TestFailedRollbackKeepsTheOriginal(t *testing.T) {
	f := newFixture(t)
	f.m.Dumps["site_3"] = "-- the original"
	f.m.OnImport = func(string, string) error { return errors.New("import exploded") }
	dump := filepath.Join(t.TempDir(), "in.sql")
	os.WriteFile(dump, []byte("x"), 0o600)
	_, err := f.o.Replace(context.Background(), "site_3", dump, Fresh)
	if err == nil {
		t.Fatal("a failed import succeeded")
	}
	left, _ := filepath.Glob(filepath.Join(f.o.WorkDir, "rollback", "site_3-*.sql"))
	if len(left) != 1 || !strings.Contains(err.Error(), left[0]) {
		t.Fatalf("kept %v, error %v", left, err)
	}
	if b, _ := os.ReadFile(left[0]); string(b) != "-- the original" {
		t.Fatalf("kept %q", b)
	}
}

// Fresh empties the database before the import; Rollback after a later
// failure (a restore's file swap) also puts the original back.
func TestReplaceFreshAndLaterRollback(t *testing.T) {
	f := newFixture(t)
	f.m.Dumps["site_3"] = "-- the original"
	dump := filepath.Join(t.TempDir(), "in.sql")
	os.WriteFile(dump, []byte("-- the snapshot"), 0o600)
	tx, err := f.o.Replace(context.Background(), "site_3", dump, Fresh)
	if err != nil {
		t.Fatal(err)
	}
	st := f.m.Statements()
	if len(st) < 2 || st[0] != "DROP DATABASE IF EXISTS `site_3`" || !strings.HasPrefix(st[1], "CREATE DATABASE `site_3` CHARACTER SET utf8mb4") {
		t.Fatalf("not emptied first: %v", st)
	}
	cause := errors.New("swap failed")
	if err := tx.Rollback(cause); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if f.m.Imported["site_3"] != "-- the snapshot-- the original" {
		t.Fatalf("imports %q", f.m.Imported["site_3"])
	}
	if _, err := os.Stat(tx.RollbackPoint()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("rollback point kept")
	}
}

// A site's primary database and account are made together, the account
// granted all of the database (escaped), and both recorded; a refused
// request runs no SQL.
func TestProvisionSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Quotes and backslashes are refused (TestPasswordsWPCLICannotWrite);
	// the rest of what SQL would care about is bound like any value.
	pw := strings.Repeat("s", 20) + "`;$%"
	s := state.Site{ID: 5, Domain: "new.example.com", Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "site_5", User: "site_5"}}}
	for _, bad := range []rpc.DBUserParams{
		{Site: s, User: "u'ser", Password: pw},
		{Site: s, User: "ok_user", Password: "short"},
		{Site: s, User: "ok_user", Password: strings.Repeat("p", 20) + "\n"},
		{Site: s, User: "ok_user", Password: pw, Grants: []state.DBGrant{{Database: "x; DROP TABLE users", Level: "all"}}},
		{Site: s, User: "ok_user", Password: pw, Grants: []state.DBGrant{{Database: "site_3", Level: "all"}}},
	} {
		if _, err := f.o.CreateUser(ctx, bad); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: s, Name: "x; DROP TABLE users"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatal(err)
	}
	if len(f.m.Statements()) != 0 {
		t.Fatalf("SQL ran for a refused request: %v", f.m.Statements())
	}
	if err := f.o.ProvisionSite(ctx, s, pw); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE DATABASE IF NOT EXISTS `site_5` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		"CREATE USER IF NOT EXISTS ?@'localhost' IDENTIFIED BY ? [site_5 " + pw + "]",
		"ALTER USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 50 [site_5 " + pw + "]",
		"GRANT ALL PRIVILEGES ON `site\\_5`.* TO ?@'localhost' [site_5]",
	}
	got := f.m.Statements()
	got = got[len(got)-4:]
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements:\n%s", strings.Join(got, "\n"))
	}
	rec, err := f.o.registry().Site(ctx, 5)
	if err != nil || strings.Join(rec.Databases, ",") != "site_5" || rec.Users["site_5"]["site_5"] != "all" {
		t.Fatalf("record %+v %v", rec, err)
	}
	// Deleting the site drops every database and account recorded for it,
	// and nothing of another site.
	if err := f.o.DropSite(ctx, 5); err != nil {
		t.Fatal(err)
	}
	got = f.m.Statements()
	if strings.Join(got[len(got)-2:], "\n") != "DROP USER IF EXISTS ?@'localhost' [site_5]\nDROP DATABASE IF EXISTS `site_5`" {
		t.Fatalf("drop: %v", got)
	}
	if rec, _ := f.o.registry().Site(ctx, 5); len(rec.Databases)+len(rec.Users) != 0 {
		t.Fatalf("still recorded: %+v", rec)
	}
	if owner, _ := f.o.registry().DatabaseOwner(ctx, "site_3"); owner != 3 {
		t.Fatal("another site's database left the record")
	}
}

// The first database tunes and restarts MariaDB once, and every concurrent
// caller waits until the server answers again before sending SQL.
func TestTuneOnceAndWaitForTheServer(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	down := 0
	sqlWhileDown := 0
	f.m.OnExec = func(q string, _ []any) error {
		mu.Lock()
		defer mu.Unlock()
		if q == "SELECT 1" && down > 0 {
			down--
			return errors.New("connect: no such file")
		}
		if q != "SELECT 1" && down > 0 {
			sqlWhileDown++
		}
		return nil
	}
	sd := f.o.Systemd.(*host.FakeSystemd)
	f.o.Systemd = restartHook{sd, func() { mu.Lock(); down = 3; mu.Unlock() }}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := state.Site{ID: int64(i + 10)}
			if _, err := f.o.CreateDB(context.Background(), rpc.SiteDatabaseParams{Site: s, Name: fmt.Sprintf("site_%d", i+10)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	n := 0
	for _, op := range sd.Ops() {
		if op == "restart mariadb.service" {
			n++
		}
	}
	if n != 1 || sqlWhileDown != 0 || down != 0 {
		t.Fatalf("restarts %d, statements while down %d, left down %d", n, sqlWhileDown, down)
	}
	b, err := os.ReadFile(f.o.TuneFile)
	if err != nil || configValue(string(b), flushKey) != "2" {
		t.Fatalf("tuning %q %v", b, err)
	}

	// A store raises durability: rendered, not text-patched, and set live.
	if err := f.o.Tune(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(f.o.TuneFile)
	if configValue(string(b), flushKey) != "1" || !strings.Contains(strings.Join(f.m.Statements(), "\n"), "SET GLOBAL innodb_flush_log_at_trx_commit = 1") {
		t.Fatalf("durability not raised: %s", b)
	}
	before := len(f.m.Statements())
	if err := f.o.Tune(context.Background(), true); err != nil || len(f.m.Statements()) != before {
		t.Fatal("durability raised twice")
	}
	if n := len(sd.Ops()); n != 1 {
		t.Fatalf("systemd ops %v", sd.Ops())
	}
	// A durability raise that fails fails the store's database.
	f.o.TuneFile = filepath.Join(t.TempDir(), "x.cnf")
	os.WriteFile(f.o.TuneFile, []byte("[mysqld]\ninnodb_flush_log_at_trx_commit = 2\n"), 0o644)
	f.m.OnExec = func(q string, _ []any) error {
		if strings.HasPrefix(q, "SET GLOBAL") {
			return errors.New("refused")
		}
		return nil
	}
	store := state.Site{ID: 20, Type: state.SiteWooCommerce}
	if _, err := f.o.CreateDB(context.Background(), rpc.SiteDatabaseParams{Site: store, Name: "site_20"}); err == nil {
		t.Fatal("a store's database was made without durability")
	}
}

type restartHook struct {
	*host.FakeSystemd
	after func()
}

func (r restartHook) Restart(ctx context.Context, unit string) error {
	err := r.FakeSystemd.Restart(ctx, unit)
	r.after()
	return err
}

func TestConfigValue(t *testing.T) {
	c := "# innodb_flush_log_at_trx_commit = 9\n[mysqld]\ninnodb-flush-log-at-trx-commit=2\n innodb_flush_log_at_trx_commit = 1 \n"
	if v := configValue(c, flushKey); v != "1" {
		t.Fatalf("%q", v)
	}
	if v := configValue("[mysqld]\n", flushKey); v != "" {
		t.Fatalf("%q", v)
	}
}

// The sweep drops exactly what the agent creates.
func TestSweep(t *testing.T) {
	f := newFixture(t)
	f.m.OnQuery = func(q string, _ []any) ([]string, error) {
		if strings.Contains(q, "mysql.user") {
			return []string{"cgq_0123456789abcdef", "cgq_short", "site_3"}, nil
		}
		return []string{"cgv_0123456789abcdef", "site_3", "cgv_short"}, nil
	}
	cnf := filepath.Join(f.o.WorkDir, ".dbq-1.cnf")
	os.WriteFile(cnf, []byte("x"), 0o600)
	keep := filepath.Join(f.o.WorkDir, "rollback", "site_3-1.sql")
	os.MkdirAll(filepath.Dir(keep), 0o700)
	os.WriteFile(keep, []byte("x"), 0o600)
	if err := f.o.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := strings.Join(f.m.Statements(), "\n")
	if st != "DROP USER IF EXISTS ?@'localhost' [cgq_0123456789abcdef]\nDROP DATABASE IF EXISTS `cgv_0123456789abcdef`\nUNBORROW" {
		t.Fatalf("sweep:\n%s", st)
	}
	if _, err := os.Stat(cnf); err == nil {
		t.Fatal("credential file left")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("a kept rollback point was swept")
	}
}

// An import replaces the database, as a restore, a migration and a staging
// push do: it holds the site's lock like them, so none interleaves with it.
func TestImportHoldsTheSiteLock(t *testing.T) {
	if l := agentrpc.DBImport.Spec().Lock; l != agentrpc.LockLiveSite {
		t.Fatalf("DBImport lock = %v", l)
	}
	if !agentrpc.DBTables.Spec().ReadOnly {
		t.Fatal("DBTables is not read-only")
	}
}
