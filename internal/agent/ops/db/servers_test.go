package db

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// fakeServers hands the agent a fake MariaDB for each external server.
type fakeServers struct {
	m map[int64]*host.FakeMariaDB
}

func (f fakeServers) Maria(id int64) (host.MariaDB, error) {
	if m, ok := f.m[id]; ok {
		return m, nil
	}
	return nil, UnknownServer(id)
}

func (f fakeServers) Target(id int64) (Target, error) {
	if _, ok := f.m[id]; ok {
		return Target{Net: "tcp", Addr: "db.example.com:3306"}, nil
	}
	return Target{}, UnknownServer(id)
}

func externalFixture(t *testing.T) (*fixture, *host.FakeMariaDB) {
	t.Helper()
	f := newFixture(t)
	ext := host.NewFakeMariaDB()
	f.o.Servers = fakeServers{m: map[int64]*host.FakeMariaDB{7: ext}}
	return f, ext
}

func has(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A database made on an external server is made there, recorded with its
// server, and nothing is run on the box's own server (not even its tuning).
func TestCreateDatabaseOnAnExternalServer(t *testing.T) {
	f, ext := externalFixture(t)
	ctx := context.Background()
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data", ServerID: 7}); err != nil {
		t.Fatal(err)
	}
	if !has(ext.Statements(), "CREATE DATABASE IF NOT EXISTS `shop_data`") {
		t.Fatalf("the external server was not asked: %v", ext.Statements())
	}
	if has(f.m.Statements(), "shop_data") || has(f.m.Statements(), "SET GLOBAL") {
		t.Fatalf("the box's server was touched: %v", f.m.Statements())
	}
	if id, _ := f.o.registry().DatabaseServer(ctx, "shop_data"); id != 7 {
		t.Fatalf("recorded on server %d", id)
	}
	if owner, _ := f.o.registry().DatabaseOwner(ctx, "shop_data"); owner != f.site.ID {
		t.Fatalf("owner %d", owner)
	}
	// The same name again, on the box's server or another one, is a conflict.
	for _, id := range []int64{0, 9} {
		if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data", ServerID: id}); err == nil {
			t.Errorf("server %d: a database of another server's name was made", id)
		}
	}
	// A name the other site has, on any server, is a conflict.
	other := f.site
	other.ID = 4
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: other, Name: "shop_data", ServerID: 7}); err == nil {
		t.Error("another site took the name")
	}
	// An unknown server is unavailable, so the panel pushes its list.
	_, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_two", ServerID: 9})
	var ae *agentrpc.Error
	if !errors.As(err, &ae) || ae.Code != agentrpc.CodeUnavailable || !strings.Contains(ae.Message, "unknown database server 9") {
		t.Fatalf("unknown server: %v", err)
	}
	if owner, _ := f.o.registry().DatabaseOwner(ctx, "shop_two"); owner != 0 {
		t.Fatal("a database on an unknown server was recorded")
	}
}

// Everything that names the database finds its server from the record.
func TestExternalDatabaseIsRoutedByTheRecord(t *testing.T) {
	f, ext := externalFixture(t)
	ctx := context.Background()
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data", ServerID: 7}); err != nil {
		t.Fatal(err)
	}
	ext.Dumps["shop_data"] = "-- dump of shop_data"
	var w bytes.Buffer
	if err := f.o.Dump(ctx, "shop_data", &w); err != nil || w.String() != "-- dump of shop_data" {
		t.Fatalf("dump %q %v", w.String(), err)
	}
	if err := f.o.Dump(ctx, "site_3", &w); err != nil {
		t.Fatal(err) // the box's own database still dumps from the box's server
	}
	path := t.TempDir() + "/in.sql"
	if err := os.WriteFile(path, []byte("CREATE TABLE t (a int);"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.o.ImportFile(ctx, "shop_data", path); err != nil {
		t.Fatal(err)
	}
	if ext.Imported["shop_data"] == "" || f.m.Imported["shop_data"] != "" {
		t.Fatalf("the import went to the wrong server: ext %v box %v", ext.Imported, f.m.Imported)
	}
	// The site's account (all privileges on the database) is who the
	// import runs as, with its definers; the fake records the choice.
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "shop_user", Password: "a-good-password-1", ServerID: 7,
		Grants: []state.DBGrant{{Database: "shop_data", Level: "all"}}}); err != nil {
		t.Fatal(err)
	}
	if !has(ext.Statements(), "CREATE USER IF NOT EXISTS") || has(f.m.Statements(), "shop_user") {
		t.Fatalf("the account was made on the wrong server: ext %v box %v", ext.Statements(), f.m.Statements())
	}
	if id, _ := f.o.registry().UserServer(ctx, "shop_user"); id != 7 {
		t.Fatalf("the account is on server %d", id)
	}
	// The console's session: the scoped account is made on the server.
	n := ext.OpenAccounts()
	s, err := f.o.OpenSession(ctx, f.site, "shop_data", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ext.OpenAccounts() != n+1 || f.m.OpenAccounts() != 0 {
		t.Fatalf("the scoped account is not on the external server: ext %d, box %d", ext.OpenAccounts(), f.m.OpenAccounts())
	}
	s.Close()
	// The list names each database's server.
	res, err := f.o.List(ctx, rpc.DBListParams{Site: f.site})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, d := range res.Databases {
		if d.Name == "shop_data" && d.ServerID == 7 {
			seen = true
		}
		if d.Name == "site_3" && d.ServerID != 0 {
			t.Errorf("the primary is on server %d", d.ServerID)
		}
	}
	if !seen {
		t.Fatalf("list: %+v", res.Databases)
	}
	// Dropping removes it on its server and from the record.
	if _, err := f.o.DropDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data"}); err != nil {
		t.Fatal(err)
	}
	if !has(ext.Statements(), "DROP DATABASE IF EXISTS `shop_data`") || has(f.m.Statements(), "shop_data") {
		t.Fatalf("drop: ext %v box %v", ext.Statements(), f.m.Statements())
	}
	if id, _ := f.o.registry().DatabaseServer(ctx, "shop_data"); id != 0 {
		t.Fatal("the server stays in the record")
	}
}

// An account's databases are on its server alone.
func TestGrantsStayOnOneServer(t *testing.T) {
	f, _ := externalFixture(t)
	ctx := context.Background()
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data", ServerID: 7}); err != nil {
		t.Fatal(err)
	}
	// A user on the box's server cannot be granted a database of server 7.
	_, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "mixed_user", Password: "a-good-password-1",
		Grants: []state.DBGrant{{Database: "shop_data", Level: "read"}}})
	var ae *agentrpc.Error
	if !errors.As(err, &ae) || ae.Code != agentrpc.CodeInvalid {
		t.Fatalf("grant across servers: %v", err)
	}
	if owner, _ := f.o.registry().UserOwner(ctx, "mixed_user"); owner != 0 {
		t.Fatal("the refused account was recorded")
	}
	// A user of server 7 cannot be granted the primary database.
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "shop_user", Password: "a-good-password-1", ServerID: 7,
		Grants: []state.DBGrant{{Database: "shop_data", Level: "read"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.SetGrants(ctx, rpc.DBUserParams{Site: f.site, User: "shop_user",
		Grants: []state.DBGrant{{Database: "site_3", Level: "read"}}}); err == nil {
		t.Fatal("grants on the box's database were given to an account of an external server")
	}
	if _, err := f.o.SetGrants(ctx, rpc.DBUserParams{Site: f.site, User: "shop_user",
		Grants: []state.DBGrant{{Database: "shop_data", Level: "all"}}}); err != nil {
		t.Fatal(err)
	}
}

// Without the table (a restart) the call is unavailable; the staged
// replacement's scratch databases follow the database they replace.
func TestStagedReplacementIsOnTheDatabasesServer(t *testing.T) {
	f, ext := externalFixture(t)
	ctx := context.Background()
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data", ServerID: 7}); err != nil {
		t.Fatal(err)
	}
	ext.OnQuery = func(q string, args []any) ([]string, error) {
		switch {
		case strings.Contains(q, "TABLE_TYPE = 'BASE TABLE'"):
			return []string{"t"}, nil
		case strings.Contains(q, "DEFAULT_CHARACTER_SET_NAME"):
			return []string{"utf8mb4 utf8mb4_unicode_ci"}, nil
		}
		return nil, nil
	}
	path := t.TempDir() + "/in.sql"
	if err := os.WriteFile(path, []byte("CREATE TABLE t (a int);"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := f.o.Stage(ctx, "shop_data", path)
	if err != nil {
		t.Fatal(err)
	}
	if !has(ext.Statements(), "CREATE DATABASE `cgm_") || has(f.m.Statements(), "cgm_") {
		t.Fatalf("scratch: ext %v box %v", ext.Statements(), f.m.Statements())
	}
	if err := st.Swap(ctx); err != nil {
		t.Fatal(err)
	}
	if !has(ext.Statements(), "RENAME TABLE") || has(f.m.Statements(), "RENAME TABLE") {
		t.Fatalf("rename: ext %v box %v", ext.Statements(), f.m.Statements())
	}
	st.Commit(ctx)
	if got := f.o.scratch.get(st.scratch); got != 0 {
		t.Fatal("the scratch database is still routed after Commit")
	}

	f.o.Servers = fakeServers{}
	var w bytes.Buffer
	err = f.o.Dump(ctx, "shop_data", &w)
	var ae *agentrpc.Error
	if !errors.As(err, &ae) || ae.Code != agentrpc.CodeUnavailable {
		t.Fatalf("no table: %v", err)
	}
}

// The agent's table of servers: set whole, a gone server is unknown.
func TestRemotesTable(t *testing.T) {
	r := &Remotes{WorkDir: t.TempDir()}
	if _, err := r.Maria(1); err == nil {
		t.Fatal("a server nobody set is known")
	}
	set := func(ids ...int64) {
		var servers []rpc.DBServerParams
		for _, id := range ids {
			servers = append(servers, rpc.DBServerParams{ID: id, Host: "db.example.com", Port: 3306, AdminUser: "admin", AdminPassword: "pw", TLS: true})
		}
		if _, err := r.Set(context.Background(), rpc.SetDBServersParams{Servers: servers}); err != nil {
			t.Fatal(err)
		}
	}
	set(1, 2)
	a, err := r.Maria(1)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := r.Maria(1); a != b {
		t.Fatal("the adapter is not kept")
	}
	if tgt, err := r.Target(2); err != nil || tgt.Net != "tcp" || tgt.Addr != "db.example.com:3306" || tgt.TLS == nil {
		t.Fatalf("%+v %v", tgt, err)
	}
	set(2)
	if _, err := r.Maria(1); err == nil {
		t.Fatal("a server that was taken out is still known")
	}
	if _, err := r.Target(1); err == nil {
		t.Fatal("a server that was taken out has a target")
	}
}
