package state

import (
	"errors"
	"testing"
)

// A server is registered with a sealed password; a database or an account on
// it keeps it from being deleted; deleting one takes its secret.
func TestDBServers(t *testing.T) {
	s := testStore(t)
	a := seedSite(t, s, "a.example.com", SitePHP, SiteActive, 0)
	srv, err := s.CreateDBServer(DBServer{Name: "managed", Host: "db.example.com", Port: 3306, TLS: DBServerTLSRequired, AdminUser: "admin"}, "pw-1")
	if err != nil || srv.ID == 0 || srv.Kind != "mariadb" {
		t.Fatalf("%+v %v", srv, err)
	}
	if _, err := s.CreateDBServer(DBServer{Name: "managed", Host: "x", Port: 1, TLS: DBServerTLSOff, AdminUser: "a"}, "p"); !errors.Is(err, ErrDBServerNameTaken) {
		t.Fatalf("a second server of the same name: %v", err)
	}
	if pw, _ := s.DBServerPassword(srv.ID); pw != "pw-1" {
		t.Fatalf("password %q", pw)
	}
	got, err := s.GetDBServer(srv.ID)
	if err != nil || got != srv {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetDBServer(999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// An update keeps the password unless a new one is given.
	srv.Host, srv.Port = "db2.example.com", 3307
	if err := s.UpdateDBServer(srv, ""); err != nil {
		t.Fatal(err)
	}
	if pw, _ := s.DBServerPassword(srv.ID); pw != "pw-1" {
		t.Fatalf("the password changed: %q", pw)
	}
	if err := s.UpdateDBServer(srv, "pw-2"); err != nil {
		t.Fatal(err)
	}
	if pw, _ := s.DBServerPassword(srv.ID); pw != "pw-2" {
		t.Fatalf("password %q", pw)
	}
	if err := s.UpdateDBServer(DBServer{ID: 999, Name: "x", Host: "h", Port: 1, TLS: DBServerTLSOff, AdminUser: "a"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// A database on it, and an account.
	d, err := s.AddSiteDatabaseOn(a.ID, "shop_data", srv.ID)
	if err != nil || d.ServerID != srv.ID {
		t.Fatalf("%+v %v", d, err)
	}
	if loc, _ := s.AddSiteDatabase(a.ID, "local_data"); loc.ServerID != 0 {
		t.Fatalf("the default is the box's server: %+v", loc)
	}
	u, err := s.AddDBUserOn(a.ID, "shop_user", DBUserSecret(a.ID, "shop_user"), "pw", []DBGrant{{Database: "shop_data", Level: "all"}}, srv.ID)
	if err != nil || u.ServerID != srv.ID {
		t.Fatalf("%+v %v", u, err)
	}
	list, _ := s.ListSiteDatabases(a.ID)
	if len(list) != 2 || list[1].Name != "shop_data" || list[1].ServerID != srv.ID || list[0].ServerID != 0 {
		t.Fatalf("%+v", list)
	}
	servers, err := s.ListDBServers()
	if err != nil || len(servers) != 1 || servers[0].Databases != 1 || servers[0].Users != 1 {
		t.Fatalf("%+v %v", servers, err)
	}
	if err := s.DeleteDBServer(srv.ID); !errors.Is(err, ErrDBServerInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	if err := s.RemoveDBUser(a.ID, "shop_user"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSiteDatabase(a.ID, "shop_data"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDBServer(srv.ID); err != nil {
		t.Fatal(err)
	}
	if pw, _ := s.DBServerPassword(srv.ID); pw != "" {
		t.Fatal("the secret survives its server")
	}
	if err := s.DeleteDBServer(srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
