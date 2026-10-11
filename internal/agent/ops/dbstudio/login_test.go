package dbstudio

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/go-sql-driver/mysql"
)

// loginServer is the fake server's sign-in: the passwords it takes, the
// databases each user may open, and what CURRENT_USER() says.
type loginServer struct {
	mu        sync.Mutex
	passwords map[string]string
	grants    map[string][]string // user -> databases
	anonymous bool                // the server matched an anonymous account
	dials     []db.DialSpec
}

func (l *loginServer) dial(f *fakeServer) func(context.Context, db.DialSpec) (db.Conn, error) {
	return func(_ context.Context, d db.DialSpec) (db.Conn, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.dials = append(l.dials, d)
		if d.Account != nil {
			return fakeConn{f}, nil
		}
		if pw, ok := l.passwords[d.Login.User]; !ok || pw != d.Login.Password {
			return nil, &mysql.MySQLError{Number: 1045, Message: "Access denied for user"}
		}
		if d.Database != "" {
			ok := false
			for _, g := range l.grants[d.Login.User] {
				ok = ok || g == d.Database
			}
			if !ok {
				return nil, &mysql.MySQLError{Number: 1044, Message: "Access denied for user to database"}
			}
		}
		who := d.Login.User + "@localhost"
		if l.anonymous {
			who = "@localhost"
		}
		return loginConn{fakeConn{f}, who, append([]string{"information_schema", "mysql"}, l.grants[d.Login.User]...)}, nil
	}
}

// loginConn answers the sign-in's own two questions and passes the rest on.
type loginConn struct {
	fakeConn
	who string
	dbs []string
}

func (c loginConn) Each(ctx context.Context, q string, args []any, fn func([]string, [][]byte) error) error {
	switch q {
	case "SELECT CURRENT_USER()":
		return fn(nil, [][]byte{[]byte(c.who)})
	case "SHOW DATABASES":
		for _, d := range c.dbs {
			if err := fn(nil, [][]byte{[]byte(d)}); err != nil {
				return err
			}
		}
		return nil
	}
	return c.fakeConn.Each(ctx, q, args, fn)
}

func (l *loginServer) lastDial() db.DialSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dials[len(l.dials)-1]
}

// newLoginFixture: site 4 has the database site_4 and the user site_4
// (the record is rebuilt from the server); site_9 is site 9's.
func newLoginFixture(t *testing.T) (*fixture, *loginServer, *time.Time) {
	fx := newFixture(t)
	fx.m.Rows[db.ScanDatabasesQuery] = []string{"site_4", "site_9"}
	fx.m.Rows[db.ScanUsersQuery] = []string{"site_4", "site_9"}
	ls := &loginServer{
		passwords: map[string]string{"site_4": "pw-of-site-4", "site_9": "pw-of-site-9", "root": "anything"},
		grants:    map[string][]string{"site_4": {"site_4"}, "site_9": {"site_9"}},
	}
	fx.o.DB.Dial = ls.dial(fx.f)
	fx.o.DB.Open = fx.o.Open
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	fx.o.Now = func() time.Time { return now }
	return fx, ls, &now
}

func (fx *fixture) signIn(t *testing.T, user, pw string) rpc.DBStudioOpenResult {
	t.Helper()
	res, err := fx.o.SignIn(ctx, rpc.DBStudioOpenParams{Site: fx.site, User: user, Password: pw})
	if err != nil {
		t.Fatalf("sign in as %s: %v", user, err)
	}
	return res
}

// SPEC §11: a refusal is the same whatever the reason — a wrong password,
// another site's user, a name the record does not give any site (root) —
// and keeps nothing; a sign-in the server matched to an anonymous account
// is a refusal too.
func TestSignInRefusals(t *testing.T) {
	fx, ls, _ := newLoginFixture(t)
	for _, c := range []struct{ user, pw string }{
		{"site_4", "wrong"}, {"site_9", "pw-of-site-9"}, {"root", "anything"}, {"nobody", "x"},
	} {
		res := fx.signIn(t, c.user, c.pw)
		if !res.Refused || res.Login != "" || res.Databases != nil {
			t.Fatalf("%s: %+v", c.user, res)
		}
	}
	ls.anonymous = true
	if res := fx.signIn(t, "site_4", "pw-of-site-4"); !res.Refused {
		t.Fatalf("an anonymous match signed in: %+v", res)
	}
	if fx.o.logins.count() != 0 {
		t.Fatal("a refused sign-in kept a session")
	}
	for _, d := range ls.dials {
		if d.Login.User == "root" || d.Login.User == "site_9" {
			t.Fatalf("the agent tried %s's password on the server", d.Login.User)
		}
	}
}

// A signed-in session runs as the user itself: no scoped account is made,
// the database is the one asked for, and the server's own schemas are not
// listed.
func TestSignedInSessionRunsAsTheUser(t *testing.T) {
	fx, ls, _ := newLoginFixture(t)
	res := fx.signIn(t, "site_4", "pw-of-site-4")
	if res.Refused || len(res.Login) != 64 || strings.Join(res.Databases, ",") != "site_4" {
		t.Fatalf("%+v", res)
	}
	accounts := len(fx.f.readOnly)
	if _, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Database: "site_4", Login: res.Login, Table: "wp_posts"}); err != nil {
		t.Fatal(err)
	}
	if d := ls.lastDial(); d.Account != nil || d.Login.User != "site_4" || d.Database != "site_4" {
		t.Fatalf("dialled %+v", d)
	}
	if len(fx.f.readOnly) != accounts {
		t.Fatal("a signed-in session made a scoped account")
	}
	// A write is the server's to refuse: nothing makes the session
	// read-only on the agent's side.
	if _, err := fx.o.Edit(ctx, rpc.DBStudioEditParams{Site: fx.site, Database: "site_4", Login: res.Login, Table: "wp_posts",
		Changes: []rpc.DBChange{{Op: "delete", Key: map[string]string{"ID": "1"}}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fx.sent(), "DELETE FROM `wp_posts`") || ls.lastDial().Login.User != "site_4" {
		t.Fatalf("sent %s", fx.sent())
	}
	// No database: the table methods need one.
	if _, err := fx.o.Tables(ctx, rpc.DBStudioTablesParams{Site: fx.site, Login: res.Login}); code(err) != "invalid" {
		t.Fatalf("no database: %v", err)
	}
	// A database the user lacks: the server says so, the session stays.
	if _, err := fx.o.Tables(ctx, rpc.DBStudioTablesParams{Site: fx.site, Database: "site_9", Login: res.Login}); code(err) != "invalid" ||
		!strings.Contains(err.Error(), "no access to the database site_9") || fx.o.logins.count() != 1 {
		t.Fatalf("another site's database: %v (%d sessions)", err, fx.o.logins.count())
	}
	// A session handle is the site's: given with another site, it is gone.
	other := fx.site
	other.ID = 9
	if _, err := fx.o.Databases(ctx, rpc.DBStudioLoginParams{Site: other, Login: res.Login}); code(err) != string(agentrpc.CodeUnauthorized) {
		t.Fatalf("another site's handle: %v", err)
	}
}

// The password lives for the session only: a changed password ends it,
// so do idleness, its age, an explicit end, and the pool's bounds.
func TestSignedInSessionEnds(t *testing.T) {
	fx, ls, now := newLoginFixture(t)
	res := fx.signIn(t, "site_4", "pw-of-site-4")
	ls.passwords["site_4"] = "changed"
	_, err := fx.o.Databases(ctx, rpc.DBStudioLoginParams{Site: fx.site, Login: res.Login})
	if code(err) != string(agentrpc.CodeUnauthorized) || fx.o.logins.count() != 0 {
		t.Fatalf("a changed password: %v, %d sessions", err, fx.o.logins.count())
	}
	ls.passwords["site_4"] = "pw-of-site-4"

	res = fx.signIn(t, "site_4", "pw-of-site-4")
	*now = now.Add(loginIdle + time.Second)
	if _, err := fx.o.Databases(ctx, rpc.DBStudioLoginParams{Site: fx.site, Login: res.Login}); code(err) != string(agentrpc.CodeUnauthorized) {
		t.Fatalf("an idle session: %v", err)
	}

	res = fx.signIn(t, "site_4", "pw-of-site-4")
	for range 30 {
		*now = now.Add(loginIdle - time.Minute)
		if _, err := fx.o.Databases(ctx, rpc.DBStudioLoginParams{Site: fx.site, Login: res.Login}); err != nil {
			break
		}
	}
	if _, err := fx.o.Databases(ctx, rpc.DBStudioLoginParams{Site: fx.site, Login: res.Login}); code(err) != string(agentrpc.CodeUnauthorized) {
		t.Fatalf("a session older than its life: %v", err)
	}

	res = fx.signIn(t, "site_4", "pw-of-site-4")
	fx.o.SignOut(ctx, rpc.DBStudioLoginParams{Site: fx.site, Login: res.Login})
	if _, err := fx.o.Databases(ctx, rpc.DBStudioLoginParams{Site: fx.site, Login: res.Login}); code(err) != string(agentrpc.CodeUnauthorized) {
		t.Fatalf("a session signed out: %v", err)
	}

	for range maxSiteLogins + 5 {
		*now = now.Add(time.Second)
		fx.signIn(t, "site_4", "pw-of-site-4")
	}
	if n := fx.o.logins.count(); n != maxSiteLogins {
		t.Fatalf("%d sessions for one site, want at most %d", n, maxSiteLogins)
	}
}

// The password is sealed in memory: the pool holds no copy of it in clear.
func TestSignedInPasswordIsSealed(t *testing.T) {
	fx, _, _ := newLoginFixture(t)
	res := fx.signIn(t, "site_4", "pw-of-site-4")
	fx.o.logins.mu.Lock()
	in := fx.o.logins.byID[res.Login]
	fx.o.logins.mu.Unlock()
	if bytes.Contains(in.sealed, []byte("pw-of-site-4")) {
		t.Fatal("the password is kept in clear")
	}
}

// An SQL export as a signed-in user is mariadb-dump as that user; an SQL
// import is the sandboxed client as it (host.ImportAs.Login).
func TestSignedInExportAndImport(t *testing.T) {
	fx, _, _ := newLoginFixture(t)
	res := fx.signIn(t, "site_4", "pw-of-site-4")
	fx.m.Dumps["site_4"] = "-- dump\n"
	out, err := fx.o.Export(ctx, rpc.DBStudioExportParams{Site: fx.site, Database: "site_4", Login: res.Login, Format: "sql"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fx.m.DumpedAs, ";") != "site_4 site_4" || !strings.HasPrefix(out.Name, "site_4-") {
		t.Fatalf("dumped as %v: %+v", fx.m.DumpedAs, out)
	}
	imports := filepath.Join(fx.site.RootPath, "shared", "imports")
	os.MkdirAll(imports, 0o750)
	os.WriteFile(filepath.Join(imports, "back.sql"), []byte("CREATE TABLE t (id INT);"), 0o640)
	if _, err := fx.o.ImportSQL(ctx, rpc.DBStudioImportSQLParams{Site: fx.site, Database: "site_4", Login: res.Login, Name: "back.sql"}); err != nil {
		t.Fatal(err)
	}
	if got := fx.m.ImportedAs[len(fx.m.ImportedAs)-1]; got != "site_4 `site_4`@`localhost`" {
		t.Fatalf("imported as %q", got)
	}
	if _, err := fx.o.ImportSQL(ctx, rpc.DBStudioImportSQLParams{Site: fx.site, Database: "site_4", Login: res.Login, Name: "../etc.sql"}); code(err) != "invalid" {
		t.Fatalf("a path: %v", err)
	}
}

// Operations: truncate and drop need the table's name typed exactly; a
// view is dropped as a view and never truncated.
func TestOperations(t *testing.T) {
	fx := newFixture(t)
	alter := func(kind, table, confirm string, dry bool) (rpc.DBStudioAlterResult, error) {
		return fx.o.Alter(ctx, rpc.DBStudioAlterParams{Site: fx.site, Table: table, Kind: kind, Confirm: confirm, DryRun: dry})
	}
	if _, err := alter("drop", "wp_log", "WP_LOG", false); code(err) != "invalid" {
		t.Fatalf("a wrong confirmation: %v", err)
	}
	for _, c := range []struct{ kind, table, want string }{
		{"truncate", "wp_log", "TRUNCATE TABLE `wp_log`"},
		{"drop", "wp_log", "DROP TABLE `wp_log`"},
		{"drop", "v_posts", "DROP VIEW `v_posts`"},
	} {
		res, err := alter(c.kind, c.table, c.table, true)
		if err != nil || res.SQL != c.want || res.Executed {
			t.Fatalf("%s %s: %+v %v", c.kind, c.table, res, err)
		}
	}
	if _, err := alter("truncate", "v_posts", "v_posts", true); code(err) != "invalid" {
		t.Fatalf("truncate a view: %v", err)
	}
	if len(fx.f.execs) != 0 {
		t.Fatal("a dry run or a refusal sent a statement")
	}
	if res, err := alter("drop", "wp_log", "wp_log", false); err != nil || !res.Executed || fx.f.execs[0].q != "DROP TABLE `wp_log`" {
		t.Fatalf("%+v %v %v", res, err, fx.f.execs)
	}
}

// Search: one bound COUNT(*) per table over its text columns only, the
// term as a LIKE value with its wildcards escaped; Browse's search the same
// clause, ANDed to the filters.
func TestSearch(t *testing.T) {
	fx := newFixture(t)
	fx.f.selectRows = [][]*string{{sp("3")}}
	_, err := fx.o.Search(ctx, rpc.DBStudioSearchParams{Site: fx.site, Term: "50%_off"})
	if err != nil {
		t.Fatal(err)
	}
	var counts []call
	for _, q := range fx.f.queries {
		if strings.HasPrefix(q.q, "SELECT COUNT(*)") {
			counts = append(counts, q)
		}
	}
	if len(counts) == 0 {
		t.Fatalf("no count sent: %v", fx.f.queries)
	}
	for _, q := range counts {
		if strings.Contains(q.q, "50%") || len(q.args) == 0 || q.args[0] != "%50!%!_off%" {
			t.Fatalf("the term is not a bound, escaped value: %q %v", q.q, q.args)
		}
	}
	if _, err := fx.o.Search(ctx, rpc.DBStudioSearchParams{Site: fx.site, Term: ""}); code(err) != "invalid" {
		t.Fatal("an empty search ran")
	}
	if _, err := fx.o.Search(ctx, rpc.DBStudioSearchParams{Site: fx.site, Term: "x", Tables: []string{"nope"}}); code(err) != "not_found" {
		t.Fatal("a table that is not there was searched")
	}

	fx.f.queries = nil
	if _, err := fx.o.Rows(ctx, rpc.DBStudioRowsParams{Site: fx.site, Table: "wp_options", Search: "home",
		Filters: []rpc.DBFilter{{Column: "autoload", Op: "eq", Value: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	want := "WHERE `autoload` = ? AND (`option_name` LIKE ? ESCAPE '!' OR `option_value` LIKE ? ESCAPE '!' OR `autoload` LIKE ? ESCAPE '!')"
	if !strings.Contains(fx.sent(), want) {
		t.Fatalf("browse with a search sent %s", fx.sent())
	}
}
