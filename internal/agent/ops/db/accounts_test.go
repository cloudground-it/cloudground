package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

// Fase 9b: a second database and two accounts with different grants; the
// read account gets SELECT and SHOW VIEW, the other everything, each on
// the escaped name; changing a grant revokes before granting; dropping the
// database revokes every grant on it first; the record follows.
func TestDatabasesAndAccounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pw := strings.Repeat("p", 12)
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "reader", Password: pw,
		Grants: []state.DBGrant{{Database: "shop_data", Level: "read"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "writer", Password: pw,
		Grants: []state.DBGrant{{Database: "shop_data", Level: "all"}, {Database: "site_3", Level: "read"}}}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.m.Statements(), "\n")
	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS `shop_data` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		"GRANT SELECT, SHOW VIEW ON `shop\\_data`.* TO ?@'localhost' [reader]",
		"GRANT ALL PRIVILEGES ON `shop\\_data`.* TO ?@'localhost' [writer]",
		"GRANT SELECT, SHOW VIEW ON `site\\_3`.* TO ?@'localhost' [writer]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in\n%s", want, all)
		}
	}
	// The password is bound, never in the text of a statement.
	for _, s := range f.m.Statements() {
		if strings.Contains(s, pw) && !strings.Contains(s, "IDENTIFIED BY ?") {
			t.Fatalf("password in %s", s)
		}
	}
	list, err := f.o.List(ctx, rpc.DBListParams{Site: f.site})
	if err != nil || len(list.Databases) != 2 || len(list.Users) != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
	if got := list.Users[0]; got.Name != "reader" || len(got.Grants) != 1 || got.Grants[0].Level != "read" {
		t.Fatalf("reader %+v", got)
	}

	// read -> all: the old grant goes first.
	before := len(f.m.Statements())
	if _, err := f.o.SetGrants(ctx, rpc.DBUserParams{Site: f.site, User: "reader", Grants: []state.DBGrant{{Database: "shop_data", Level: "all"}}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.m.Statements()[before:], "\n"); got != "REVOKE ALL PRIVILEGES ON `shop\\_data`.* FROM ?@'localhost' [reader]\nGRANT ALL PRIVILEGES ON `shop\\_data`.* TO ?@'localhost' [reader]" {
		t.Fatalf("grant change:\n%s", got)
	}

	// The primary account keeps all of the primary database; the primary
	// database and account are not dropped.
	if _, err := f.o.SetGrants(ctx, rpc.DBUserParams{Site: f.site, User: "site_3", Grants: []state.DBGrant{{Database: "site_3", Level: "read"}}}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("primary account demoted: %v", err)
	}
	if _, err := f.o.DropDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "site_3"}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("primary database dropped: %v", err)
	}
	if _, err := f.o.DropUser(ctx, rpc.DBUserParams{Site: f.site, User: "site_3"}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("primary account dropped: %v", err)
	}

	// Dropping the database revokes every grant on it, then drops it.
	before = len(f.m.Statements())
	if _, err := f.o.DropDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data"}); err != nil {
		t.Fatal(err)
	}
	got := f.m.Statements()[before:]
	if len(got) != 3 || !strings.HasPrefix(got[0], "REVOKE ALL PRIVILEGES ON `shop\\_data`") || !strings.HasPrefix(got[1], "REVOKE ALL PRIVILEGES ON `shop\\_data`") || got[2] != "DROP DATABASE IF EXISTS `shop_data`" {
		t.Fatalf("drop:\n%s", strings.Join(got, "\n"))
	}
	rec, _ := f.o.registry().Site(ctx, 3)
	if strings.Join(rec.Databases, ",") != "site_3" || rec.Users["writer"]["shop_data"] != "" || rec.Users["writer"]["site_3"] != "read" {
		t.Fatalf("record after the drop %+v", rec)
	}
	if _, err := f.o.DropUser(ctx, rpc.DBUserParams{Site: f.site, User: "reader"}); err != nil {
		t.Fatal(err)
	}
	if owner, _ := f.o.registry().UserOwner(ctx, "reader"); owner != 0 {
		t.Fatal("a dropped account stays recorded")
	}
	// The record is root's alone.
	if fi, err := os.Stat(f.o.RegistryFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file %v %v", fi.Mode(), err)
	}
}

// A name the server has that the record gives no site, or gives another
// site, is a conflict: it is never taken over.
func TestCreateRefusesNamesInUse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.OnQuery = func(q string, args []any) ([]string, error) {
		if strings.Contains(q, "SCHEMATA WHERE SCHEMA_NAME") && args[0] == "legacy_app" {
			return []string{"legacy_app"}, nil
		}
		if strings.Contains(q, "global_priv WHERE User = ?") && args[0] == "legacy_user" {
			return []string{"legacy_user"}, nil
		}
		return nil, nil
	}
	for _, name := range []string{"legacy_app"} {
		if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: name}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := state.Site{ID: 4, Domain: "other.example.com"}
	if _, err := f.o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: other, Name: "site_3"}); err == nil {
		t.Error("site 4 took site_3")
	}
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "legacy_user", Password: strings.Repeat("p", 12)}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Errorf("legacy_user: %v", err)
	}
	// site 4's account is recorded for site 4.
	if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: other, User: "site_3", Password: strings.Repeat("p", 12)}); err == nil {
		t.Error("site 4 took site 3's account")
	}
	if len(f.m.Statements()) != 0 {
		t.Fatalf("statements ran: %v", f.m.Statements())
	}
}

// An account that is granted, and whose grant the server says does not
// exist, is not a failure (the record and the server may disagree after a
// rebuild).
func TestRevokeOfAMissingGrant(t *testing.T) {
	f := newFixture(t)
	f.m.OnExec = func(q string, _ []any) error {
		if strings.HasPrefix(q, "REVOKE") {
			return &mysql.MySQLError{Number: 1141, Message: "There is no such grant defined"}
		}
		return nil
	}
	if err := f.o.revoke(context.Background(), "site_3", "site_3"); err != nil {
		t.Fatal(err)
	}
	f.m.OnExec = func(string, []any) error { return errors.New("down") }
	if err := f.o.revoke(context.Background(), "site_3", "site_3"); err == nil {
		t.Fatal("a failed revoke passed")
	}
}

// An install from before the record gets it rebuilt from the server: every
// site_<id> database and account is site <id>'s, the account granted all
// of its namesake; nothing else is anyone's.
func TestRegistryRebuild(t *testing.T) {
	f := newFixture(t)
	f.o.RegistryFile = filepath.Join(t.TempDir(), "sub", "databases.json")
	f.o.reg = nil
	f.o.regOnce = sync.Once{}
	f.m.Rows[ScanDatabasesQuery] = []string{"site_7", "site_8", "mysql", "wordpress", "site_07", "cgv_0123456789abcdef"}
	f.m.Rows[ScanUsersQuery] = []string{"site_7", "root", "mariadb.sys", "site_9"}
	if err := f.o.LoadRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.o.RegistryFile)
	if err != nil {
		t.Fatal(err)
	}
	var got registryFile
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Databases) != 2 || got.Databases["site_7"] != 7 || got.Databases["site_8"] != 8 {
		t.Fatalf("databases %v", got.Databases)
	}
	if len(got.Users) != 2 || got.Users["site_7"].Grants["site_7"] != "all" || got.Users["site_9"].Site != 9 || len(got.Users["site_9"].Grants) != 0 {
		t.Fatalf("users %v", got.Users)
	}
	// A damaged record is not rebuilt over: every request is refused until
	// someone looks.
	os.WriteFile(f.o.RegistryFile, []byte("{"), 0o600)
	f.o.reg = nil
	f.o.regOnce = sync.Once{}
	if _, err := f.o.SiteDatabase(context.Background(), state.Site{ID: 7, Domain: "a.example.com"}, "site_7"); !errors.Is(err, errNoRecord) {
		t.Fatalf("a damaged record: %v", err)
	}
}

// A password wp-cli cannot write into wp-config.php as it is never
// reaches MariaDB: neither a new account nor a new password takes it.
func TestPasswordsWPCLICannotWrite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, pw := range []string{`x\'.system(chr(105)).\'`, `with "double" quotes`, `it's a password`, `ends in a backslash\`} {
		before := len(f.m.Statements())
		if _, err := f.o.CreateUser(ctx, rpc.DBUserParams{Site: f.site, User: "quoted", Password: pw}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("CreateUser with %q: %v", pw, err)
		}
		if _, err := f.o.SetPassword(ctx, rpc.DBUserParams{Site: f.site, User: "site_3", Password: pw}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("SetPassword with %q: %v", pw, err)
		}
		if got := f.m.Statements()[before:]; len(got) != 0 {
			t.Errorf("%q reached MariaDB: %v", pw, got)
		}
	}
}
