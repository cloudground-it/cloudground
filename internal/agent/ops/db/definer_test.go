package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

// SR-T7, Fase 9b: a site's database is imported as an account of that site
// with all privileges on it (the database's namesake first, then the first
// by name; none: a scoped account without TRIGGER), the restore test's
// scratch database as a scoped account that may create triggers, a staged
// migration's as one that may not.
func TestImportAs(t *testing.T) {
	f := newFixture(t)
	writeRecord(t, f.o.RegistryFile, registryFile{Version: registryVersion,
		Databases: map[string]int64{"site_3": 3, "shop": 3, "logs": 3},
		Users: map[string]registryUser{
			"site_3":  {Site: 3, Grants: map[string]string{"site_3": "all", "logs": "read"}},
			"reports": {Site: 3, Grants: map[string]string{"shop": "read"}},
			"admin":   {Site: 3, Grants: map[string]string{"shop": "all"}},
			"zed":     {Site: 3, Grants: map[string]string{"shop": "all"}},
			"other":   {Site: 4, Grants: map[string]string{"logs": "all"}},
		}})
	for db, want := range map[string]host.ImportAs{
		"site_3":               {Site: "site_3"},
		"shop":                 {Site: "admin"},
		"logs":                 {},
		"cgv_0123456789abcdef": {Triggers: true},
		"cgm_0123456789abcdef": {},
	} {
		if got := f.o.importAs(context.Background(), db); got != want {
			t.Errorf("%s: %+v, want %+v", db, got, want)
		}
	}
	f = newFixture(t)
	dump := filepath.Join(t.TempDir(), "d.sql")
	os.WriteFile(dump, []byte("CREATE TABLE t (i INT);"), 0o600)
	tx, err := f.o.Replace(context.Background(), "site_3", dump, Over)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if got := strings.Join(f.m.ImportedAs, "|"); got != "site_3 `site_3`@`localhost`" {
		t.Fatalf("imports %q", got)
	}
}

// SR-T7: for a database on an external server the agent takes the password
// of the account its own record picks from the request, and only for that
// user — a password the request supplies under any other name is ignored,
// and the import falls back to a throwaway account.
func TestImportAsUsesSuppliedCredentials(t *testing.T) {
	f := newFixture(t)
	writeRecord(t, f.o.RegistryFile, registryFile{Version: registryVersion,
		Databases: map[string]int64{"shop": 3},
		Users: map[string]registryUser{
			"owner": {Site: 3, Grants: map[string]string{"shop": "all"}},
		}})
	// The password of the user the record picks (owner) is used.
	ctx := WithImportAccounts(context.Background(), map[string]string{"owner": "owner-pw", "someone": "other-pw"})
	if got := f.o.importAs(ctx, "shop"); got.Site != "owner" || got.SitePassword != "owner-pw" {
		t.Fatalf("picked %+v, want owner with its password", got)
	}
	// A password supplied only under another user's name is not used: the
	// agent never signs in as a user the record did not pick.
	ctx = WithImportAccounts(context.Background(), map[string]string{"someone": "other-pw"})
	if got := f.o.importAs(ctx, "shop"); got.Site != "owner" || got.SitePassword != "" {
		t.Fatalf("picked %+v, want owner with no password (fallback)", got)
	}
}
