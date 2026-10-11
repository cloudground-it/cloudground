package backup

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
)

const db2ID = "dbdb2222dbdb2222"

// part is one database of a seeded backup.
type part struct {
	id, name, dump string
	tables         int // -1: no tables: tag (a backup from before)
}

// seedParts lays out a backup of the site with these database snapshots,
// each named by a db: tag of the files snapshot.
func (f *fixture) seedParts(parts ...part) {
	f.t.Helper()
	f.seed(false, "")
	tags := `"site:docs.example.com","kind:full"`
	for _, p := range parts {
		tags += `,"db:` + p.id + `"`
		dt := `"site:docs.example.com","kind:full","part:database","dbname:` + p.name + `"`
		if p.tables >= 0 {
			dt += `,"tables:` + strconv.Itoa(p.tables) + `"`
		}
		f.write("snap-"+p.id+".json", `[{"id":"`+p.id+`","hostname":"box","tags":[`+dt+`]}]`)
		f.write("dump-"+p.id, p.dump)
	}
	f.write("snap-"+filesID+".json", `[{"id":"`+filesID+`","hostname":"box","tags":[`+tags+`]}]`)
}

// addDatabase gives the site a second database, shop_data.
func (f *fixture) addDatabase() {
	f.t.Helper()
	tune := filepath.Join(f.t.TempDir(), "tuned.cnf")
	os.WriteFile(tune, []byte("[mysqld]\n"), 0o644) // tuned already: no restart
	f.o.DB.TuneFile = tune
	if _, err := f.o.DB.CreateDB(ctx, rpc.SiteDatabaseParams{Site: f.site, Name: "shop_data"}); err != nil {
		f.t.Fatal(err)
	}
}

// Fase 9b: a backup dumps every database of the site, each into a snapshot
// of its own named after it, with its table count; the files snapshot
// names all of them.
func TestBackupDumpsEveryDatabase(t *testing.T) {
	f := newFixture(t)
	f.addDatabase()
	f.m.Dumps["site_3"] = "-- MariaDB dump\nCREATE TABLE a (id int);\n"
	f.m.Dumps["shop_data"] = "-- MariaDB dump\nCREATE TABLE b (id int);\n"
	if _, err := f.backup("full"); err != nil {
		t.Fatal(err)
	}
	log := f.log()
	for _, name := range []string{"site_3", "shop_data"} {
		if !strings.Contains(log, "--tag part:database --tag dbname:"+name+" --tag tables:0 --json") {
			t.Errorf("no snapshot of %s:\n%s", name, log)
		}
	}
	args, _ := os.ReadFile(filepath.Join(f.dir, "files-args"))
	if n := strings.Count(string(args), "--tag db:"); n != 2 {
		t.Fatalf("the files snapshot names %d database snapshots: %s", n, args)
	}
}

// A site with no database backs up its files alone, and its restore test
// checks the files.
func TestBackupOfASiteWithoutDatabases(t *testing.T) {
	f := newFixture(t)
	f.site.ID, f.site.SystemUser = 9, "cg-site-9"
	f.site.Config.Database.Enabled = false
	if _, err := f.backup("full"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.log(), "--stdin") {
		t.Fatalf("a database was dumped:\n%s", f.log())
	}
	f.seed(false, "")
	if res := f.verify(); !res.Passed || !strings.Contains(res.Detail, "files restored and verified") {
		t.Fatalf("verify = %+v", res)
	}
}

// A backup of two databases restores both, each into its own, and its
// restore test imports both: one that does not import fails it. A restore
// may name one.
func TestRestoreAndVerifyTwoDatabases(t *testing.T) {
	f := newFixture(t)
	f.addDatabase()
	f.seedParts(
		part{dbID, "site_3", "-- MariaDB dump\nCREATE TABLE a (id int);\n", 1},
		part{db2ID, "shop_data", "-- MariaDB dump\nCREATE TABLE b (id int);\n", 1},
	)
	imported := map[string]string{}
	f.m.OnImport = func(database, sql string) error {
		imported[database] = sql
		return nil
	}
	if err := f.restore("database"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(imported["site_3"], "TABLE a") || !strings.Contains(imported["shop_data"], "TABLE b") {
		t.Fatalf("imported %v", imported)
	}

	clear(imported)
	if _, err := f.o.RestoreSnapshot(ctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID,
		Mode: "database", Databases: []string{"shop_data"}}); err != nil {
		t.Fatal(err)
	}
	if len(imported) != 1 || imported["shop_data"] == "" {
		t.Fatalf("restore of one database imported %v", imported)
	}
	if _, err := f.o.RestoreSnapshot(ctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID,
		Mode: "database", Databases: []string{"site_4"}}); err == nil {
		t.Fatal("restored a database the backup does not hold")
	}

	f.m.OnQuery = func(string, []any) ([]string, error) { return []string{"1"}, nil }
	var tested []string
	f.m.OnImport = func(database, sql string) error {
		tested = append(tested, sql)
		return nil
	}
	res := f.verify()
	if !res.Passed || !strings.Contains(res.Detail, "2 databases imported") || len(tested) != 2 {
		t.Fatalf("verify = %+v, imported %d", res, len(tested))
	}
	f.m.OnImport = func(database, sql string) error {
		if strings.Contains(sql, "TABLE b") {
			return os.ErrInvalid
		}
		return nil
	}
	if res := f.verify(); res.Passed || !strings.Contains(res.Detail, "shop_data") {
		t.Fatalf("a backup whose second database does not import passed: %+v", res)
	}
}

// An empty database is a valid dump of an empty database: its restore test
// passes with no table. A dump that brings back fewer tables than the
// database had fails, and so does an empty one from a backup that did not
// record the count.
func TestVerifyEmptyDatabase(t *testing.T) {
	for _, c := range []struct {
		recorded, imported int
		pass               bool
	}{{0, 0, true}, {3, 3, true}, {3, 0, false}, {3, 2, false}, {-1, 0, false}, {-1, 2, true}} {
		f := newFixture(t)
		f.seedParts(part{dbID, "site_3", "-- MariaDB dump\n", c.recorded})
		n := strconv.Itoa(c.imported)
		f.m.OnQuery = func(string, []any) ([]string, error) { return []string{n}, nil }
		if res := f.verify(); res.Passed != c.pass {
			t.Errorf("recorded %d, imported %d: %+v", c.recorded, c.imported, res)
		}
	}
}
