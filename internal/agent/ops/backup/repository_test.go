package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The IDs restic copy gave the seeded backup (SPEC §5): new IDs, the old
// ones in their original field.
const (
	copiedFiles = "c0c0c0c0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	copiedDB    = "c1c1c1c1bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// copied turns the seeded backup into what `restic copy` leaves in a new
// repository: restic knows it only by new IDs, whose original fields are the
// IDs the record and the db: tag still hold.
func (f *fixture) copied() {
	f.t.Helper()
	for _, p := range [][2]string{{"tree-" + filesID, "tree-" + copiedFiles}, {"dump-" + dbID, "dump-" + copiedDB}} {
		if err := os.Rename(filepath.Join(f.dir, p[0]), filepath.Join(f.dir, p[1])); err != nil {
			f.t.Fatal(err)
		}
	}
	os.Remove(filepath.Join(f.dir, "snap-"+filesID+".json"))
	os.Remove(filepath.Join(f.dir, "snap-"+dbID+".json"))
	f.siteSnapshots(`[{"id":"` + copiedFiles + `","original":"` + filesID + `","hostname":"box","tags":["site:docs.example.com","kind:full","db:` + dbID + `"]},` +
		`{"id":"` + copiedDB + `","original":"` + dbID + `","hostname":"box","tags":["site:docs.example.com","kind:full","part:database","dbname:site_3"]}]`)
}

// SPEC §5 (#192): a backup copied by restic restores, is restore-tested and
// downloads by the ID the panel recorded; restic is given the copies' IDs.
func TestCopiedBackupResolvesByOriginalID(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	f.copied()
	mk(t, f.site.RootPath, map[string]string{"releases/initial/index.html": "changed since"})
	// The scratch import's table count; the registry's scans keep their rows.
	rows := f.m.Rows
	f.m.OnQuery = func(q string, _ []any) ([]string, error) {
		if r, ok := rows[q]; ok {
			return append([]string(nil), r...), nil
		}
		return []string{"1"}, nil
	}

	if res := f.verify(); !res.Passed {
		t.Fatalf("restore test of a copied backup = %+v", res)
	}
	if err := f.restore("full"); err != nil {
		t.Fatalf("full restore of a copied backup: %v", err)
	}
	if got := f.live("current/index.html"); got != "live" {
		t.Fatalf("index.html = %q, want the snapshot's", got)
	}
	if !strings.Contains(f.m.Imported["site_3"], "CREATE TABLE good") {
		t.Fatalf("the copied database snapshot was not imported: %q", f.m.Imported)
	}
	if err := f.restore("database"); err != nil {
		t.Fatalf("database restore of a copied backup: %v", err)
	}
	_, err := f.o.RestoreSnapshot(ctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID,
		Mode: "files", Paths: []string{"releases/initial/index.html"}})
	if err != nil {
		t.Fatalf("selective restore of a copied backup: %v", err)
	}
	log := f.log()
	for _, want := range []string{"restore " + copiedFiles + " --target", "dump " + copiedDB + " /database.sql", "restore " + copiedFiles + " --target"} {
		if !strings.Contains(log, want) {
			t.Fatalf("no %q among the restic calls:\n%s", want, log)
		}
	}
	if strings.Contains(log, "restore "+filesID) || strings.Contains(log, "dump "+dbID) {
		t.Fatalf("restic was given an ID it does not know:\n%s", log)
	}
}

// SPEC §5: a copy is still checked as the site's own, made on this server;
// two copies claiming one original are ambiguous; no copy is not found.
func TestCopiedBackupIsCheckedLikeAnyOther(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	f.copied()
	f.siteSnapshots(`[{"id":"` + copiedFiles + `","original":"` + filesID + `","hostname":"elsewhere","tags":["site:docs.example.com","kind:full"]}]`)
	if err := f.restore("files"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("a copy made on another server = %v", err)
	}
	f.siteSnapshots(`[{"id":"` + copiedFiles + `","original":"` + filesID + `","hostname":"box","tags":["site:docs.example.com"]},` +
		`{"id":"` + copiedDB + `","original":"` + filesID + `","hostname":"box","tags":["site:docs.example.com"]}]`)
	if err := f.restore("files"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("two copies of one snapshot = %v", err)
	}
	f.siteSnapshots(`[]`)
	if err := f.restore("files"); agentrpc.CodeOf(err) != agentrpc.CodeNotFound {
		t.Fatalf("no snapshot at all = %v", err)
	}
}

// SPEC §9: a copied backup is deleted by the ID the panel holds, with its
// database half (named in db: by its original ID); the result names the
// originals; an orphan check counts a copied database half as named.
func TestForgetCopiedBackup(t *testing.T) {
	f := newFixture(t)
	f.siteSnapshots(`[{"id":"` + copiedFiles + `","original":"` + forgetFiles + `","tags":["site:docs.example.com","db:` + forgetDB + `"]},` +
		`{"id":"` + copiedDB + `","original":"` + forgetDB + `","tags":["site:docs.example.com","part:database"]}]`)
	res, err := f.forget(forgetFiles[:8])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Forgotten, " ") != copiedFiles+" "+copiedDB || strings.Join(res.Originals, " ") != forgetFiles+" "+forgetDB {
		t.Fatalf("forget of a copied backup = %+v", res)
	}

	f = newFixture(t)
	f.siteSnapshots(`[{"id":"` + copiedFiles + `","original":"` + forgetFiles + `","tags":["site:docs.example.com","db:` + forgetDB + `"]},` +
		`{"id":"` + copiedDB + `","original":"` + forgetDB + `","tags":["site:docs.example.com","part:database"]}]`)
	res, err = f.forget(forgetOther)
	if err != nil || !res.Missing || len(res.Forgotten) != 0 {
		t.Fatalf("a missing snapshot took a copied database half that a backup still names: %+v %v", res, err)
	}
}

// SPEC §8: retention keeps a copied snapshot the panel protects by its
// original ID, and the database half a kept copy names by its original.
func TestRetentionProtectsCopiedSnapshots(t *testing.T) {
	f := newFixture(t)
	f.write("plan", `[{"host":"box","keep":[{"id":"`+snapNew+`","tags":["db:`+dbID+`"]}],`+
		`"remove":[{"id":"`+copiedFiles+`"},{"id":"`+copiedDB+`"},{"id":"`+snapOld+`"}]}]`)
	f.siteSnapshots(`[{"id":"` + copiedFiles + `","original":"` + filesID + `"},{"id":"` + copiedDB + `","original":"` + dbID + `"},` +
		`{"id":"` + snapOld + `","original":"dddddddd00000000"}]`)
	res, err := f.o.BackupRetention(ctx, rpc.RetentionParams{Site: f.site, Repository: repoURL, Password: "pw",
		KeepWithinDays: 30, Protect: []string{filesID}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Forgotten, []string{snapOld}) || !slices.Equal(res.Originals, []string{"dddddddd00000000"}) {
		t.Fatalf("retention = %+v, want only the old snapshot forgotten, named by its original too", res)
	}
	if !slices.Contains(res.Kept, copiedFiles) || !slices.Contains(res.Kept, copiedDB) {
		t.Fatalf("kept = %v", res.Kept)
	}
}

const (
	repoA   = "/var/backups/cloudground/a"
	repoB   = "/var/backups/cloudground/b"
	cfgID   = "1234567812345678123456781234567812345678123456781234567812345678"
	keyOfA  = "_var_backups_cloudground_a"
	keyOfB  = "_var_backups_cloudground_b"
	shortID = "c0c0c0c0"
)

// SPEC §13: RepositoryInfo reads the ID and names the expected snapshots
// the repository does not hold, by ID or original; a missing repository is
// not made.
func TestRepositoryInfo(t *testing.T) {
	f := newFixture(t)
	f.write("config"+keyOfA, `{"version":2,"id":"`+cfgID+`","chunker_polynomial":"35f46ed0c4a79f"}`)
	f.write("all.json", `[{"id":"`+copiedFiles+`","original":"`+filesID+`"},{"id":"`+copiedDB+`"}]`)
	res, err := f.o.RepositoryInfo(ctx, rpc.RepositoryInfoParams{Repository: repoA, Password: "pw",
		Expect: []string{filesID, shortID, copiedDB, dbID}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != cfgID || res.Snapshots != 2 || !slices.Equal(res.Missing, []string{dbID}) {
		t.Fatalf("RepositoryInfo = %+v", res)
	}
	f.write("missing"+keyOfB, "")
	if _, err := f.o.RepositoryInfo(ctx, rpc.RepositoryInfoParams{Repository: repoB, Password: "pw"}); agentrpc.CodeOf(err) != agentrpc.CodeNotFound {
		t.Fatalf("RepositoryInfo of a missing repository = %v", err)
	}
	if strings.Contains(f.log(), "init") {
		t.Fatal("RepositoryInfo initialised a repository")
	}
	if _, err := f.o.RepositoryInfo(ctx, rpc.RepositoryInfoParams{Repository: "/etc", Password: "pw"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a repository the rules refuse = %v", err)
	}
}

// SPEC §13: a copy into a new repository makes it from the source (its
// chunker parameters), then copies with the source in RESTIC_FROM_*,
// never in argv.
func TestCopyRepositoryMakesTheTargetAndCopies(t *testing.T) {
	f := newFixture(t)
	f.write("config", `{"id":"`+cfgID+`"}`)
	f.write("missing"+keyOfB, "")
	f.write("all.json", `[{"id":"`+copiedFiles+`","original":"`+filesID+`"}]`)
	res, err := f.o.CopyRepository(ctx, rpc.CopyRepositoryParams{
		From: rpc.TestBackupParams{Repository: repoA, Password: "old-pw"},
		To:   rpc.TestBackupParams{Repository: repoB, Password: "new-pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != cfgID || res.Snapshots != 1 {
		t.Fatalf("CopyRepository = %+v", res)
	}
	log := f.log()
	if !strings.Contains(log, "init --copy-chunker-params\n") || !strings.Contains(log, "copy\n") {
		t.Fatalf("restic calls:\n%s", log)
	}
	if strings.Contains(log, repoA) || strings.Contains(log, "old-pw") || strings.Contains(log, "new-pw") {
		t.Fatalf("a repository or a password in argv:\n%s", log)
	}
	from, _ := os.ReadFile(filepath.Join(f.dir, "from-repos"))
	if !strings.Contains(string(from), "init "+repoA+" old-pw") || !strings.Contains(string(from), "copy "+repoA+" old-pw") {
		t.Fatalf("the source did not reach restic's environment: %q", from)
	}
}

// SPEC §13: what one restic run cannot copy between is refused before it
// runs: the same repository, two values of one credential, two SFTP hosts.
func TestCopyRepositoryRefusals(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name     string
		from, to rpc.TestBackupParams
		want     string
	}{
		{"same", rpc.TestBackupParams{Repository: repoA, Password: "a"}, rpc.TestBackupParams{Repository: repoA, Password: "a"}, "the same"},
		{"credentials", rpc.TestBackupParams{Repository: "b2:bucket-a:r", Password: "a", Env: map[string]string{"B2_ACCOUNT_ID": "one"}},
			rpc.TestBackupParams{Repository: "b2:bucket-b:r", Password: "b", Env: map[string]string{"B2_ACCOUNT_ID": "two"}}, "B2_ACCOUNT_ID"},
		{"sftp", rpc.TestBackupParams{Repository: "sftp:u@one.example.com:/r", Password: "a"},
			rpc.TestBackupParams{Repository: "sftp:u@two.example.com:/r", Password: "b"}, "two SFTP servers"},
	} {
		_, err := f.o.CopyRepository(ctx, rpc.CopyRepositoryParams{From: tc.from, To: tc.to})
		if agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want invalid %q", tc.name, err, tc.want)
		}
	}
	if strings.Contains(f.log(), "copy") {
		t.Fatalf("a refused copy ran restic:\n%s", f.log())
	}
}

// host SPEC: decoders: `cat config` must carry a 64-hex-digit id.
func TestDecodeRepoConfig(t *testing.T) {
	if c, err := host.DecodeRepoConfig([]byte(`{"version":2,"id":"` + cfgID + `"}`)); err != nil || c.ID != cfgID {
		t.Fatalf("decode = %+v %v", c, err)
	}
	for _, bad := range []string{``, `{}`, `{"id":"xyz"}`, `not json`} {
		if _, err := host.DecodeRepoConfig([]byte(bad)); err == nil {
			t.Errorf("decoded %q", bad)
		}
	}
}
