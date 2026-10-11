package state

import (
	"os"
	"path/filepath"
	"testing"
)

// The claims are read read-only: every database and user with its grants,
// and the file is not written (its modification time and size stay). A
// state from before migration 22 is an error that says what to do.
func TestReadDatabaseClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := seedSite(t, s, "a.example.com", SitePHP, SiteActive, 0)
	if _, err := s.AddSiteDatabase(a.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDBUser(a.ID, "reports", DBUserSecret(a.ID, "reports"), "pw-123456789012", []DBGrant{{Database: "shop", Level: "read"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, _ := os.Stat(path)
	c, err := ReadDatabaseClaims(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Databases) != 1 || c.Databases[0] != (DatabaseClaim{SiteID: a.ID, Name: "shop"}) ||
		len(c.Users) != 1 || c.Users[0].Name != "reports" || len(c.Users[0].Grants) != 1 || c.Users[0].Grants[0].Level != "read" {
		t.Fatalf("claims %+v", c)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("reading the claims wrote the state")
	}

	old := filepath.Join(t.TempDir(), "old.db")
	o, err := openAtVersion(old, 21)
	if err != nil {
		t.Fatal(err)
	}
	o.Close()
	if _, err := ReadDatabaseClaims(old); err == nil {
		t.Fatal("a state before migration 22 passed")
	}
}
