package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// reservedNames are never a site's database or account: the server's own,
// the agent's, another site's default name and anything off the shape.
var reservedNames = []string{"mysql", "root", "sys", "performance_schema", "information_schema",
	"debian_sys_maint", "mariadb.sys", "test", "site_01", "site_4", "Shop", "-x", "cgq_0123456789abcdef", "cgv_0123456789abcdef", ""}

func isInvalid(err error) bool {
	var e *agentrpc.Error
	return errors.As(err, &e) && e.Code == agentrpc.CodeInvalid
}

// SR-R1, Fase 9b: creating a database or an account refuses every reserved
// name, for itself and as a granted database, before any statement.
func TestCreateRefusesReservedNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pw := strings.Repeat("p", 20)
	for _, name := range reservedNames {
		if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: name}); !isInvalid(err) {
			t.Errorf("CreateDB %q: %v", name, err)
		}
		if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: name, Password: pw}); !isInvalid(err) {
			t.Errorf("CreateUser %q: %v", name, err)
		}
		g := []state.DBGrant{{Database: name, Level: "all"}}
		if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "reports", Password: pw, Grants: g}); !isInvalid(err) {
			t.Errorf("CreateUser granted on %q: %v", name, err)
		}
	}
	if s := f.m.Statements(); len(s) != 0 {
		t.Fatalf("statements ran: %v", s)
	}
}

// Fase 9b: every handler that names a database or an account acts only on
// one the agent's record gives the site; another site's (site_4), one no
// site has (shop) and a reserved name are refused before any session,
// file or statement.
func TestSiteHandlersRefuseAnotherName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, name := range append(reservedNames, "shop") {
		s := f.site
		if name == "" {
			continue // the primary, below
		}
		if _, err := f.o.Query(ctx, rpc.DBQueryParams{Site: s, Database: name, SQL: "select 1", ReadOnly: true}); name != "" && !isInvalid(err) {
			t.Errorf("Query on %q: %v", name, err)
		}
		if _, err := f.o.Tables(ctx, rpc.DBTablesParams{Site: s, Database: name}); name != "" && !isInvalid(err) {
			t.Errorf("Tables on %q: %v", name, err)
		}
		if _, err := f.o.Export(ctx, rpc.DBExportParams{Site: s, Database: name}); name != "" && !isInvalid(err) {
			t.Errorf("Export on %q: %v", name, err)
		}
		if _, err := f.o.Import(ctx, rpc.DBImportParams{Site: s, Database: name, RelPath: "x.sql"}); name != "" && !isInvalid(err) {
			t.Errorf("Import on %q: %v", name, err)
		}
		if _, err := f.o.OpenSession(ctx, s, name, true, 0); name != "" && !isInvalid(err) {
			t.Errorf("OpenSession on %q: %v", name, err)
		}
		if _, err := f.o.DropDB(ctx, rpc.SiteDatabaseParams{Site: s, Name: name}); !isInvalid(err) {
			t.Errorf("DropDB %q: %v", name, err)
		}
		for _, call := range []func(context.Context, rpc.DBUserParams) (map[string]string, error){f.o.DropUser, f.o.SetPassword, f.o.SetGrants} {
			if _, err := call(ctx, rpc.DBUserParams{Site: s, User: name, Password: strings.Repeat("p", 20)}); !isInvalid(err) {
				t.Errorf("an account call on %q: %v", name, err)
			}
		}
	}
	// A primary database the record does not give the site is refused too,
	// whatever its record says.
	s := f.site
	s.Config.Database.Name = "site_4"
	if _, err := f.o.Tables(ctx, rpc.DBTablesParams{Site: s}); !isInvalid(err) {
		t.Errorf("Tables on another site's database named as the primary: %v", err)
	}
	if f.m.OpenAccounts() != 0 || len(f.conns) != 0 || len(f.m.Statements()) != 0 {
		t.Fatalf("something ran: %v", f.m.Statements())
	}
}

func TestValidNames(t *testing.T) {
	for _, ok := range []string{"site_1", "site_42", "shop_data", "cgv_0123456789abcdef", "cgm_0123456789abcdef", "cgh_0123456789abcdef"} {
		if !ValidName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"mysql", "root", "sys", "performance_schema", "information_schema", "debian_sys_maint",
		"mariadb.sys", "test", "Shop", "-x", "cgq_0123456789abcdef", "cgv_0123", ""} {
		if ValidName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
