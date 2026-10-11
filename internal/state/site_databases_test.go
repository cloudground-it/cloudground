package state

import (
	"errors"
	"path/filepath"
	"testing"
)

// Migration 22: every site with a local managed database gets it as its
// first database and its account as its first account, granted all of it,
// names and secret unchanged; a site without one (static, external) gets
// nothing.
func TestMigration22MovesTheSiteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openAtVersion(path, 21)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sites (id, domain, type, profile, system_user, root_path, status, config, created_at, updated_at) VALUES
			(7, 'wp.example.com', 'wordpress', 'balanced', 'cg-site-7', '/srv/sites/wp', 'active',
			 '{"database":{"enabled":true,"external":false,"host":"127.0.0.1","port":3306,"name":"site_7","user":"site_7"}}', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
			(8, 'static.example.com', 'static', 'balanced', 'cg-site-8', '/srv/sites/st', 'active',
			 '{"database":{"enabled":false,"name":"","user":""}}', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
			(9, 'ext.example.com', 'wordpress', 'balanced', 'cg-site-9', '/srv/sites/ext', 'active',
			 '{"database":{"enabled":true,"external":true,"name":"remote","user":"remote"}}', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSecret(SiteSecret(7, SiteSecretDBPassword), "the-old-password"); err != nil {
		t.Fatal(err)
	}
	dbs, err := s.ListSiteDatabases(7)
	if err != nil || len(dbs) != 1 || dbs[0].Name != "site_7" {
		t.Fatalf("databases %+v %v", dbs, err)
	}
	users, err := s.ListDBUsers(7)
	if err != nil || len(users) != 1 || users[0].Name != "site_7" || users[0].Secret != "site.7.db_password" ||
		len(users[0].Grants) != 1 || users[0].Grants[0] != (DBGrant{Database: "site_7", Level: "all"}) {
		t.Fatalf("users %+v %v", users, err)
	}
	if pw, err := s.DBUserPassword(7, "site_7"); err != nil || pw != "the-old-password" {
		t.Fatalf("password %q %v", pw, err)
	}
	for _, id := range []int64{8, 9} {
		if d, _ := s.ListSiteDatabases(id); len(d) != 0 {
			t.Errorf("site %d got %+v", id, d)
		}
		if u, _ := s.ListDBUsers(id); len(u) != 0 {
			t.Errorf("site %d got %+v", id, u)
		}
	}
}

// Names are unique across the server, grants name only the site's own
// databases, and removing a database takes its grants; deleting the site
// takes everything.
func TestSiteDatabaseRecords(t *testing.T) {
	s := testStore(t)
	a := seedSite(t, s, "a.example.com", SitePHP, SiteActive, 0)
	b := seedSite(t, s, "b.example.com", SitePHP, SiteActive, 0)
	if _, err := s.AddSiteDatabase(a.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSiteDatabase(a.ID, "logs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSiteDatabase(b.ID, "shop"); !errors.Is(err, ErrDBNameTaken) {
		t.Fatalf("a name taken on the server: %v", err)
	}
	if _, err := s.AddSiteDatabase(b.ID, "b_data"); err != nil {
		t.Fatal(err)
	}
	u, err := s.AddDBUser(a.ID, "reports", DBUserSecret(a.ID, "reports"), "a-password-123", []DBGrant{{Database: "shop", Level: "read"}, {Database: "logs", Level: "all"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Grants) != 2 {
		t.Fatalf("grants %+v", u.Grants)
	}
	if id, err := s.DBUserSite("reports"); err != nil || id != a.ID {
		t.Fatalf("DBUserSite(reports) = %d, %v", id, err)
	}
	if _, err := s.DBUserSite("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DBUserSite(nobody): %v", err)
	}
	if _, err := s.AddDBUser(b.ID, "reports", DBUserSecret(b.ID, "reports"), "x", nil); !errors.Is(err, ErrDBNameTaken) {
		t.Fatalf("an account name taken on the server: %v", err)
	}
	if _, err := s.AddDBUser(a.ID, "spy", DBUserSecret(a.ID, "spy"), "x", []DBGrant{{Database: "b_data", Level: "read"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a grant on another site's database: %v", err)
	}
	if _, err := s.DBUserByName(a.ID, "spy"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refused account was kept")
	}
	if _, err := s.AddDBUser(a.ID, "bad", DBUserSecret(b.ID, "bad"), "x", nil); err == nil {
		t.Fatal("an account's password stored as another site's secret")
	}
	if pw, _ := s.DBUserPassword(a.ID, "reports"); pw != "a-password-123" {
		t.Fatalf("password %q", pw)
	}
	if err := s.RemoveSiteDatabase(a.ID, "logs"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.DBUserByName(a.ID, "reports"); len(u.Grants) != 1 || u.Grants[0].Database != "shop" {
		t.Fatalf("grants after the database went: %+v", u.Grants)
	}
	if _, err := s.SetDBUserGrants(a.ID, "reports", []DBGrant{{Database: "shop", Level: "all"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDBUser(a.ID, "reports"); err != nil {
		t.Fatal(err)
	}
	if pw, _ := s.Secret(DBUserSecret(a.ID, "reports")); pw != "" {
		t.Fatal("a removed account's password stayed")
	}
	if err := s.DeleteSite(b.ID); err != nil {
		t.Fatal(err)
	}
	if inUse, _ := s.DBNameInUse("b_data", false); inUse {
		t.Fatal("a deleted site's database is still recorded")
	}
}
