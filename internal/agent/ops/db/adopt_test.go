package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A lost record is rebuilt from the panel's claims and the server: what the
// server has and passes the checks is adopted with its grants; a system or
// reserved name, another site's default, a name two sites claim, one the
// server lacks and a grant on a database not adopted for the same site are
// refused. Nothing runs on the server.
func TestAdoptRebuildsALostRecord(t *testing.T) {
	f := newFixture(t)
	f.o.RegistryFile = filepath.Join(t.TempDir(), "databases.json") // lost
	f.o.reg = nil
	f.m.Rows[ScanDatabasesQuery] = []string{"site_3", "shop_data", "mysql", "twice", "logs", "site_9"}
	f.m.Rows[ScanUsersQuery] = []string{"site_3", "reports", "root", "spy"}
	claims := state.DatabaseClaims{
		Databases: []state.DatabaseClaim{
			{SiteID: 3, Name: "site_3"}, {SiteID: 3, Name: "shop_data"},
			{SiteID: 3, Name: "mysql"},                             // system
			{SiteID: 3, Name: "site_9"},                            // another site's default
			{SiteID: 3, Name: "twice"}, {SiteID: 5, Name: "twice"}, // claimed twice
			{SiteID: 3, Name: "gone"}, // not on the server
			{SiteID: 5, Name: "logs"},
		},
		Users: []state.DBUserClaim{
			{SiteID: 3, Name: "site_3", Grants: []state.DBGrant{{Database: "site_3", Level: "all"}}},
			{SiteID: 3, Name: "reports", Grants: []state.DBGrant{{Database: "shop_data", Level: "read"}, {Database: "logs", Level: "read"}}},
			{SiteID: 3, Name: "root"},
		},
	}
	// A dry run writes nothing.
	if _, err := f.o.Adopt(context.Background(), claims, true); err != nil {
		t.Fatal(err)
	}
	// Not even the record rebuilt because none was on disk.
	if _, err := os.Stat(f.o.RegistryFile); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote the record: %v", err)
	}
	if rec, _ := f.o.registry().Site(context.Background(), 3); rec.Users["reports"] != nil {
		t.Fatal("a dry run recorded an adoption")
	}
	rep, err := f.o.Adopt(context.Background(), claims, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.m.Statements()) != 0 {
		t.Fatalf("statements ran: %v", f.m.Statements())
	}
	rec, _ := f.o.registry().Site(context.Background(), 3)
	if strings.Join(rec.Databases, ",") != "shop_data,site_3" {
		t.Fatalf("site 3's databases %v", rec.Databases)
	}
	if rec.Users["reports"]["shop_data"] != "read" || rec.Users["reports"]["logs"] != "" || rec.Users["site_3"]["site_3"] != "all" {
		t.Fatalf("site 3's users %v", rec.Users)
	}
	if owner, _ := f.o.registry().DatabaseOwner(context.Background(), "logs"); owner != 5 {
		t.Fatalf("logs is site %d's", owner)
	}
	refused := strings.Join(rep.Refused, "\n")
	for _, want := range []string{"database mysql", "database site_9", "database twice (site 3): two sites claim it",
		"database twice (site 5)", "database gone (site 3): the server has no database", "user root", "grant of reports on logs"} {
		if !strings.Contains(refused, want) {
			t.Errorf("not refused: %s\n%s", want, refused)
		}
	}
	if owner, _ := f.o.registry().UserOwner(context.Background(), "root"); owner != 0 {
		t.Fatal("root was adopted")
	}
	// The record is root's.
	if fi, err := os.Stat(f.o.RegistryFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record %v %v", fi, err)
	}
	// A record that gives a name to another site is not overwritten.
	again := state.DatabaseClaims{Databases: []state.DatabaseClaim{{SiteID: 7, Name: "shop_data"}}}
	rep, err = f.o.Adopt(context.Background(), again, false)
	if err != nil || len(rep.Refused) != 1 || !strings.Contains(rep.Refused[0], "the record gives it to site 3") {
		t.Fatalf("%+v %v", rep, err)
	}
}
