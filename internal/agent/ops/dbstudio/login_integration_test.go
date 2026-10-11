//go:build studiointegration

// Quarry's signed-in users against a real MariaDB (ops/dbstudio SPEC
// §11): run as root where CG_TEST_MYSQL_SOCKET is the server's socket and
// mariadb-dump and mariadb are installed, in a throwaway container only:
//
//	go test -tags studiointegration -run Integration ./internal/agent/ops/dbstudio/
package dbstudio

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestIntegrationSignedInUsers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	socket := os.Getenv("CG_TEST_MYSQL_SOCKET")
	if socket == "" {
		t.Skip("CG_TEST_MYSQL_SOCKET not set")
	}
	ctx := context.Background()
	work := t.TempDir()
	m := host.NewMariaDB(socket, work, host.OSExec{Timeout: time.Minute})
	defer m.Close()
	tuned := filepath.Join(t.TempDir(), "tune.cnf")
	os.WriteFile(tuned, []byte("[mysqld]\ninnodb_flush_log_at_trx_commit = 1\n"), 0o644)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := filepath.Join(t.TempDir(), "shop.example.com")
	os.MkdirAll(filepath.Join(root, "shared", "imports"), 0o755)
	open := func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, sitefs.Owner{UID: 0, GID: 0}) }
	dbo := &db.Ops{MariaDB: m, Socket: socket, WorkDir: work, TuneFile: tuned, Log: quiet, Open: open}
	o := &Ops{DB: dbo, Log: quiet, Open: open}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	shop := state.Site{ID: 9, Domain: "shop.example.com", Type: state.SiteWordPress, RootPath: root,
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "cgit_shop", User: "cgit_shop"}}}
	other := state.Site{ID: 10, Domain: "other.example.com", Type: state.SiteWordPress, RootPath: root,
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "cgit_other", User: "cgit_other"}}}
	const rpw, wpw, opw = "reader-password-1#with spaces ", "writer-password-1", "other-password-12"
	t.Cleanup(func() {
		for _, u := range []string{"cgit_reader", "cgit_writer", "cgit_other"} {
			m.Exec(ctx, "DROP USER IF EXISTS ?@'localhost'", u)
		}
		m.Exec(ctx, "DROP DATABASE IF EXISTS cgit_shop")
		m.Exec(ctx, "DROP DATABASE IF EXISTS cgit_other")
	})
	_, err := dbo.CreateDB(ctx, rpc.SiteDatabaseParams{Site: shop, Name: "cgit_shop"})
	must(err)
	_, err = dbo.CreateDB(ctx, rpc.SiteDatabaseParams{Site: other, Name: "cgit_other"})
	must(err)
	_, err = dbo.CreateUser(ctx, rpc.DBUserParams{Site: shop, User: "cgit_reader", Password: rpw, Grants: []state.DBGrant{{Database: "cgit_shop", Level: "read"}}})
	must(err)
	_, err = dbo.CreateUser(ctx, rpc.DBUserParams{Site: shop, User: "cgit_writer", Password: wpw, Grants: []state.DBGrant{{Database: "cgit_shop", Level: "all"}}})
	must(err)
	_, err = dbo.CreateUser(ctx, rpc.DBUserParams{Site: other, User: "cgit_other", Password: opw, Grants: []state.DBGrant{{Database: "cgit_other", Level: "all"}}})
	must(err)
	must(m.Exec(ctx, "CREATE TABLE cgit_shop.orders (id INT PRIMARY KEY, note TEXT)"))
	must(m.Exec(ctx, "INSERT INTO cgit_shop.orders VALUES (1, 'first order'), (2, '50% off_now')"))
	must(m.Exec(ctx, "CREATE TABLE cgit_other.secret (v TEXT)"))
	must(m.Exec(ctx, "INSERT INTO cgit_other.secret VALUES ('other tenant')"))

	signIn := func(s state.Site, user, pw string) rpc.DBStudioOpenResult {
		t.Helper()
		res, err := o.SignIn(ctx, rpc.DBStudioOpenParams{Site: s, User: user, Password: pw})
		must(err)
		return res
	}
	// Refusals: a wrong password, another site's user, root (whose
	// unix_socket sign-in the agent, as root, would pass), a name nobody has.
	for _, c := range []struct {
		s        state.Site
		user, pw string
	}{{shop, "cgit_reader", "wrong"}, {shop, "cgit_other", opw}, {shop, "root", ""}, {shop, "root", "x"}, {shop, "nobody_here", "x"}} {
		if res := signIn(c.s, c.user, c.pw); !res.Refused {
			t.Fatalf("%s signed in: %+v", c.user, res)
		}
	}
	reader := signIn(shop, "cgit_reader", rpw)
	if reader.Refused || strings.Join(reader.Databases, ",") != "cgit_shop" {
		t.Fatalf("the reader: %+v", reader)
	}
	writer := signIn(shop, "cgit_writer", wpw)
	if writer.Refused {
		t.Fatal("the writer was refused")
	}

	// The reader browses; the server refuses its writes.
	page, err := o.Rows(ctx, rpc.DBStudioRowsParams{Site: shop, Database: "cgit_shop", Login: reader.Login, Table: "orders", Search: "% off_"})
	must(err)
	if page.Total != 1 || *page.Rows[0].Values[1] != "50% off_now" {
		t.Fatalf("search as the reader: %+v", page)
	}
	_, err = o.Edit(ctx, rpc.DBStudioEditParams{Site: shop, Database: "cgit_shop", Login: reader.Login, Table: "orders",
		Changes: []rpc.DBChange{{Op: "insert", Values: map[string]*string{"id": sp("3"), "note": sp("ro")}}}})
	if code(err) != "invalid" || !strings.Contains(err.Error(), "1142") {
		t.Fatalf("the reader's insert: %v", err)
	}
	q, err := o.Query(ctx, rpc.DBStudioQueryParams{Site: shop, Database: "cgit_shop", Login: reader.Login, SQL: "DELETE FROM orders"})
	must(err)
	if !strings.Contains(q.Message, "1142") {
		t.Fatalf("the reader's DELETE: %+v", q)
	}
	if _, err := o.Alter(ctx, rpc.DBStudioAlterParams{Site: shop, Database: "cgit_shop", Login: reader.Login, Table: "orders", Kind: "truncate", Confirm: "orders"}); code(err) != "invalid" {
		t.Fatalf("the reader's truncate: %v", err)
	}
	// Another site's database, in every form.
	if _, err := o.Tables(ctx, rpc.DBStudioTablesParams{Site: shop, Database: "cgit_other", Login: reader.Login}); code(err) != "invalid" {
		t.Fatalf("another site's database: %v", err)
	}
	q, err = o.Query(ctx, rpc.DBStudioQueryParams{Site: shop, Database: "cgit_shop", Login: writer.Login, SQL: "SELECT * FROM cgit_other.secret"})
	must(err)
	if len(q.Rows) != 0 || !strings.HasPrefix(q.Message, "ERROR ") {
		t.Fatalf("another site's table through SQL: %+v", q)
	}
	// LOAD DATA LOCAL reads nothing.
	q, err = o.Query(ctx, rpc.DBStudioQueryParams{Site: shop, Database: "cgit_shop", Login: writer.Login, SQL: "LOAD DATA LOCAL INFILE '/etc/passwd' INTO TABLE orders"})
	must(err)
	if !strings.HasPrefix(q.Message, "ERROR ") && !strings.Contains(q.Message, "the query failed") {
		t.Fatalf("LOAD DATA LOCAL: %+v", q)
	}

	// The writer writes, exports and imports as itself.
	_, err = o.Edit(ctx, rpc.DBStudioEditParams{Site: shop, Database: "cgit_shop", Login: writer.Login, Table: "orders",
		Changes: []rpc.DBChange{{Op: "insert", Values: map[string]*string{"id": sp("3"), "note": sp("rw")}}}})
	must(err)
	ex, err := o.Export(ctx, rpc.DBStudioExportParams{Site: shop, Database: "cgit_shop", Login: reader.Login, Format: "sql"})
	must(err)
	dump, _ := os.ReadFile(filepath.Join(root, "shared", "exports", ex.Name))
	if !strings.Contains(string(dump), "CREATE TABLE `orders`") || !strings.Contains(string(dump), "first order") {
		t.Fatalf("the reader's dump: %.300s", dump)
	}
	os.WriteFile(filepath.Join(root, "shared", "imports", "views.sql"),
		[]byte("CREATE DEFINER=`root`@`localhost` VIEW v_orders AS SELECT id FROM orders;\n"), 0o644)
	_, err = o.ImportSQL(ctx, rpc.DBStudioImportSQLParams{Site: shop, Database: "cgit_shop", Login: writer.Login, Name: "views.sql"})
	must(err)
	def, err := m.QueryStrings(ctx, "SELECT DEFINER FROM information_schema.VIEWS WHERE TABLE_SCHEMA = 'cgit_shop' AND TABLE_NAME = 'v_orders'")
	must(err)
	if len(def) != 1 || def[0] != "cgit_writer@localhost" {
		t.Fatalf("the view's definer: %v", def)
	}
	if _, err := o.ImportSQL(ctx, rpc.DBStudioImportSQLParams{Site: shop, Database: "cgit_shop", Login: reader.Login, Name: "views.sql"}); code(err) != "invalid" {
		t.Fatalf("the reader's import: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(work, ".dbq-*.cnf"))
	if len(left) != 0 {
		t.Fatalf("defaults files left: %v", left)
	}
	if res, err := o.Alter(ctx, rpc.DBStudioAlterParams{Site: shop, Database: "cgit_shop", Login: writer.Login, Table: "v_orders", Kind: "drop", Confirm: "v_orders"}); err != nil || !res.Executed {
		t.Fatalf("the writer's drop: %+v %v", res, err)
	}

	// A changed password ends the session.
	_, err = dbo.SetPassword(ctx, rpc.DBUserParams{Site: shop, User: "cgit_reader", Password: "a-new-password-12"})
	must(err)
	if _, err := o.Databases(ctx, rpc.DBStudioLoginParams{Site: shop, Login: reader.Login}); code(err) != string(agentrpc.CodeUnauthorized) {
		t.Fatalf("after the password changed: %v", err)
	}
}
