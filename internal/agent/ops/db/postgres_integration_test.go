package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// These tests run against a real PostgreSQL with TLS. They are skipped
// unless CG_PG_TEST names one: "host:port|admin user|admin password|path of
// the CA certificate" (the container of scripts/ in the developer's notes:
// postgres:16 with ssl=on and a certificate for the host name).

type pgTarget struct {
	host     string
	port     int
	user, pw string
	ca       string
}

func pgTestTarget(t *testing.T) pgTarget {
	t.Helper()
	v := os.Getenv("CG_PG_TEST")
	if v == "" {
		t.Skip("CG_PG_TEST is not set")
	}
	f := strings.Split(v, "|")
	if len(f) != 4 {
		t.Fatalf("CG_PG_TEST is host:port|user|password|ca file, not %q", v)
	}
	h, p, _ := strings.Cut(f[0], ":")
	var port int
	fmt.Sscan(p, &port)
	ca, err := os.ReadFile(f[3])
	if err != nil {
		t.Fatal(err)
	}
	return pgTarget{host: h, port: port, user: f[1], pw: f[2], ca: string(ca)}
}

func (g pgTarget) server() host.RemoteServer {
	return host.RemoteServer{ID: 5, Host: g.host, Port: g.port, TLS: true, CACert: g.ca, AdminUser: g.user, AdminPassword: g.pw, Kind: "postgres", BackupDumps: true}
}

// pgServers is the agent's table with one PostgreSQL server (id 5).
type pgServers struct {
	fakeServers
	pg    *host.PostgresAdmin
	admin string
}

func (s pgServers) Kind(id int64) (string, error) {
	if id == 5 {
		return "postgres", nil
	}
	return s.fakeServers.Kind(id)
}
func (s pgServers) Postgres(id int64) (*host.PostgresAdmin, error) { return s.pg, nil }
func (s pgServers) AdminUser(int64) string                         { return s.admin }

// as connects as an account of the server, verifying the certificate.
func (g pgTarget) as(t *testing.T, user, pw, database string) *pgx.Conn {
	t.Helper()
	srv := g.server()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=require", g.host, g.port, user, database))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = pw
	if cfg.TLSConfig, err = srv.TLSConfig(); err != nil {
		t.Fatal(err)
	}
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		return nil
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (g pgTarget) exec(c *pgx.Conn, q string) error {
	_, err := c.Exec(context.Background(), q)
	return err
}

func TestPostgresDatabasesAndAccounts(t *testing.T) {
	g := pgTestTarget(t)
	f := newFixture(t)
	pg := host.NewPostgres(g.server(), t.TempDir(), nil)
	defer pg.Close()
	f.o.Servers = pgServers{fakeServers: fakeServers{m: map[int64]*host.FakeMariaDB{}}, pg: pg, admin: g.user}
	ctx := context.Background()
	const db1, db2, rw, ro = "pgt_shop", "pgt_other", "pgt_rw", "pgt_ro"
	cleanup := func() {
		for _, d := range []string{db1, db2} {
			_ = pg.Exec(ctx, "", "DROP DATABASE IF EXISTS "+host.QuotePGIdent(d)+" WITH (FORCE)")
			_ = pg.Exec(ctx, "", "DROP ROLE IF EXISTS "+host.QuotePGIdent(ownerRole(d)))
		}
		for _, u := range []string{rw, ro} {
			_ = pg.Exec(ctx, "", "DROP ROLE IF EXISTS "+host.QuotePGIdent(u))
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	// The server is tested by the agent: its version, and an administrator that may create.
	r := &Remotes{WorkDir: t.TempDir()}
	res, err := r.Test(ctx, rpc.DBServerParams{Kind: "postgres", Host: g.host, Port: g.port, TLS: true, CACert: g.ca, AdminUser: g.user, AdminPassword: g.pw})
	if err != nil || !strings.Contains(res.Version, "PostgreSQL") {
		t.Fatalf("test: %+v %v", res, err)
	}
	if _, err := r.Test(ctx, rpc.DBServerParams{Kind: "postgres", Host: g.host, Port: g.port, TLS: true, CACert: g.ca, AdminUser: g.user, AdminPassword: "wrong-password"}); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if _, err := r.Test(ctx, rpc.DBServerParams{Kind: "postgres", Host: g.host, Port: g.port, TLS: true, AdminUser: g.user, AdminPassword: g.pw}); err == nil {
		t.Fatal("a certificate no authority signed was accepted")
	}

	// Two databases.
	for _, name := range []string{db1, db2} {
		if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: name, ServerID: 5}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if id, _ := f.o.registry().DatabaseServer(ctx, db1); id != 5 {
		t.Fatalf("recorded on server %d", id)
	}
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: db1, ServerID: 5}); err != nil {
		t.Fatalf("creating a database that is already the site's again: %v", err)
	}
	other := f.site
	other.ID = 4
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: other, Name: db1, ServerID: 5}); err == nil {
		t.Fatal("another site took the name")
	}
	if owner, _ := pg.QueryStrings(ctx, "", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = "+host.QuotePGLiteral(db1)); len(owner) != 1 || owner[0] != ownerRole(db1) {
		t.Fatalf("owner %v", owner)
	}

	// An account with all privileges and one that reads.
	pw := "a-good-password-1"
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: rw, Password: pw, ServerID: 5,
		Grants: []state.DBGrant{{Database: db1, Level: "all"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: ro, Password: pw, ServerID: 5,
		Grants: []state.DBGrant{{Database: db1, Level: "read"}}}); err != nil {
		t.Fatal(err)
	}
	w := g.as(t, rw, pw, db1)
	if w == nil {
		t.Fatal("the all account cannot sign in")
	}
	if err := g.exec(w, "CREATE TABLE t (id int PRIMARY KEY); INSERT INTO t VALUES (7)"); err != nil {
		t.Fatalf("the all account writes: %v", err)
	}
	reader := g.as(t, ro, pw, db1)
	if reader == nil {
		t.Fatal("the read account cannot sign in")
	}
	var n int
	if err := reader.QueryRow(ctx, "SELECT id FROM t").Scan(&n); err != nil || n != 7 {
		t.Fatalf("the read account reads: %d %v", n, err)
	}
	if err := g.exec(reader, "INSERT INTO t VALUES (8)"); err == nil {
		t.Fatal("the read account wrote")
	}
	if err := g.exec(reader, "CREATE TABLE x (a int)"); err == nil {
		t.Fatal("the read account created a table")
	}
	// What the all account creates later is readable too (default privileges, objects of the owner).
	if err := g.exec(w, "CREATE TABLE t2 (v text); INSERT INTO t2 VALUES ('later')"); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := reader.QueryRow(ctx, "SELECT v FROM t2").Scan(&v); err != nil || v != "later" {
		t.Fatalf("the read account reads a table made later: %q %v", v, err)
	}
	// Only their own database.
	if c := g.as(t, ro, pw, db2); c != nil {
		t.Fatal("the read account reached another database")
	}
	if c := g.as(t, rw, pw, db2); c != nil {
		t.Fatal("the all account reached another database")
	}
	if c := g.as(t, ro, "not-the-password", db1); c != nil {
		t.Fatal("a wrong password was accepted")
	}
	// The record: the user is on server 5, a database of another server is refused.
	if _, err := f.o.SetGrants(ctx, rpc.DBUserParams{Site: f.site, User: ro, Grants: []state.DBGrant{{Database: "site_3", Level: "read"}}}); err == nil {
		t.Fatal("a database of the box's own server was granted to an account of a PostgreSQL server")
	}

	// The list.
	list, err := f.o.List(ctx, rpc.DBListParams{Site: f.site})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, d := range list.Databases {
		if d.Name == db1 {
			seen = d.ServerID == 5 && d.Tables == 2 && d.SizeBytes > 0
		}
	}
	if !seen {
		t.Fatalf("list: %+v (%s)", list.Databases, list.StatsError)
	}
	if names, err := f.o.TableNames(ctx, db1); err != nil || strings.Join(names, ",") != "t,t2" {
		t.Fatalf("tables %v %v", names, err)
	}

	// Emptying keeps the read grant working.
	if err := f.o.empty(ctx, db1); err != nil {
		t.Fatal(err)
	}
	w.Close(ctx)
	w = g.as(t, rw, pw, db1)
	if err := g.exec(w, "CREATE TABLE fresh (v int); INSERT INTO fresh VALUES (1)"); err != nil {
		t.Fatalf("after emptying the all account writes: %v", err)
	}
	reader.Close(ctx)
	reader = g.as(t, ro, pw, db1)
	if reader == nil {
		t.Fatal("after emptying the read account cannot sign in")
	}
	if err := reader.QueryRow(ctx, "SELECT v FROM fresh").Scan(&n); err != nil || n != 1 {
		t.Fatalf("after emptying the read account reads: %d %v", n, err)
	}
	if names, _ := f.o.TableNames(ctx, db1); strings.Join(names, ",") != "fresh" {
		t.Fatalf("emptied: %v", names)
	}

	// Taking the grant away closes the door; the password changes.
	if _, err := f.o.SetGrants(ctx, rpc.DBUserParams{Site: f.site, User: ro, Grants: nil}); err != nil {
		t.Fatal(err)
	}
	if c := g.as(t, ro, pw, db1); c != nil {
		t.Fatal("the account signs in to a database it has no grant on")
	}
	if _, err := f.o.SetPassword(ctx, rpc.DBUserParams{Site: f.site, User: rw, Password: "another-good-password-2"}); err != nil {
		t.Fatal(err)
	}
	if c := g.as(t, rw, pw, db1); c != nil {
		t.Fatal("the old password still works")
	}
	if c := g.as(t, rw, "another-good-password-2", db1); c == nil {
		t.Fatal("the new password does not work")
	}

	// Dropping.
	if _, err := f.o.DropUser(ctx, rpc.DBUserParams{Site: f.site, User: ro}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.DropDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: db1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.QueryStrings(ctx, "", "SELECT datname FROM pg_database WHERE datname = "+host.QuotePGLiteral(db1)); len(got) != 0 {
		t.Fatal("the database stays")
	}
	if got, _ := pg.QueryStrings(ctx, "", "SELECT rolname FROM pg_roles WHERE rolname IN ("+host.QuotePGLiteral(ro)+", "+host.QuotePGLiteral(ownerRole(db1))+")"); len(got) != 0 {
		t.Fatalf("roles stay: %v", got)
	}
	// Deleting the site drops what is left.
	if err := f.o.DropSite(ctx, f.site.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.QueryStrings(ctx, "", "SELECT datname FROM pg_database WHERE datname = "+host.QuotePGLiteral(db2)); len(got) != 0 {
		t.Fatal("the site's other database stays")
	}
	if got, _ := pg.QueryStrings(ctx, "", "SELECT rolname FROM pg_roles WHERE rolname = "+host.QuotePGLiteral(rw)); len(got) != 0 {
		t.Fatal("the site's account stays")
	}
}
