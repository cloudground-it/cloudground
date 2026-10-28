package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// SPEC §4 step 2b (#193): every backup stores the site's description as a
// snapshot of its own, which the files snapshot names; no secret is in it.
func TestBackupStoresTheSiteDescription(t *testing.T) {
	f := newFixture(t)
	f.site.Aliases = []string{"www.docs.example.com"}
	f.site.Config.CacheEnabled = true
	if _, err := f.backup("full"); err != nil {
		t.Fatal(err)
	}
	log := f.log()
	if !strings.Contains(log, "backup --stdin --stdin-filename cloudground-site.json --tag site:docs.example.com --tag kind:full --tag part:manifest --json --quiet") {
		t.Fatalf("no description snapshot:\n%s", log)
	}
	args, _ := os.ReadFile(filepath.Join(f.dir, "files-args"))
	if !strings.Contains(string(args), "--tag manifest:"+manifestSnap) {
		t.Fatalf("the files snapshot does not name its description: %q", args)
	}
	raw, _ := os.ReadFile(filepath.Join(f.dir, "manifest"))
	var m rpc.SiteManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("description %q: %v", raw, err)
	}
	if m.Format != 1 || m.Domain != "docs.example.com" || m.Type != state.SiteStatic || m.PHPVersion != "8.4" ||
		!slices.Equal(m.Aliases, f.site.Aliases) || !m.Cache.Enabled || m.Root != f.site.RootPath ||
		len(m.Databases) != 1 || m.Databases[0] != (rpc.ManifestDatabase{Name: "site_3", Engine: "mariadb", Primary: true}) {
		t.Fatalf("description = %+v", m)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatalf("a secret in the description: %s", raw)
	}
}

// SPEC §4 step 4: a backup whose files snapshot fails leaves no description.
func TestFailedBackupForgetsItsDescription(t *testing.T) {
	f := newFixture(t)
	f.write("exit-backup-files", "3")
	if _, err := f.backup("files"); err == nil {
		t.Fatal("a backup that could not read every file succeeded")
	}
	if !strings.Contains(f.log(), "forget "+manifestSnap) {
		t.Fatalf("the description stayed:\n%s", f.log())
	}
}

// seedManifest gives the seeded backup a description snapshot.
func (f *fixture) seedManifest(host, body string) {
	f.t.Helper()
	tags := `"site:docs.example.com","kind:full","db:` + dbID + `","manifest:` + manifestSnap + `"`
	f.write("snap-"+filesID+".json", `[{"id":"`+filesID+`","hostname":"`+host+`","tags":[`+tags+`]}]`)
	f.write("snap-"+manifestSnap+".json", `[{"id":"`+manifestSnap+`","hostname":"`+host+`","tags":["site:docs.example.com","part:manifest"]}]`)
	f.write("dump-"+manifestSnap, body)
}

// SPEC §6: a description that does not read fails the restore test; one
// that reads passes.
func TestVerifyReadsTheDescription(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	f.m.OnQuery = func(q string, _ []any) ([]string, error) { return []string{"1"}, nil }
	f.seedManifest("box", `{"format":1,"domain":"docs.example.com"}`)
	if res := f.verify(); !res.Passed {
		t.Fatalf("restore test with a description = %+v", res)
	}
	for _, bad := range []string{`not json`, `{"format":1,"domain":"other.example.com"}`} {
		f.write("dump-"+manifestSnap, bad)
		if res := f.verify(); res.Passed || !strings.Contains(res.Detail, "description") {
			t.Fatalf("restore test with description %q = %+v", bad, res)
		}
	}
}

// SPEC §5: a restore from another server takes that server's snapshot, maps
// the primary into the new site's database and writes the new site's
// credentials before PHP starts; without Foreign the same snapshot is
// refused, and another server than the one named is refused too.
func TestForeignRestore(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	f.write("snap-"+dbID+".json", `[{"id":"`+dbID+`","hostname":"old-box","tags":["site:docs.example.com","part:database","dbname:site_12"]}]`)
	f.seedManifest("old-box", `{"format":1,"domain":"docs.example.com"}`)
	var reconfigured []rpc.ForeignRestore
	f.o.Reconfigure = func(_ context.Context, s state.Site, fr rpc.ForeignRestore) error {
		if f.restarts != 0 {
			t.Error("the credentials were written after PHP restarted")
		}
		reconfigured = append(reconfigured, fr)
		return nil
	}
	params := func(foreign *rpc.ForeignRestore) rpc.RestoreParams {
		return rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID, Foreign: foreign}
	}
	if _, err := f.o.RestoreSnapshot(ctx, params(nil)); agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "old-box") {
		t.Fatalf("another server's snapshot without the exception = %v", err)
	}
	if _, err := f.o.RestoreSnapshot(ctx, params(&rpc.ForeignRestore{Hostname: "third-box"})); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a snapshot of another server than the one named = %v", err)
	}
	fr := &rpc.ForeignRestore{Hostname: "old-box", Databases: map[string]string{"site_12": "site_3"}, DBPassword: "new-pw", ConnectorToken: "tok"}
	res, err := f.o.RestoreSnapshot(ctx, params(fr))
	if err != nil {
		t.Fatalf("foreign restore: %v", err)
	}
	if res["databases"] != "site_3" || !strings.Contains(f.m.Imported["site_3"], "CREATE TABLE good") {
		t.Fatalf("result %v, imported %v", res, f.m.Imported)
	}
	if len(reconfigured) != 1 || reconfigured[0].DBPassword != "new-pw" || f.restarts != 1 {
		t.Fatalf("reconfigured %+v, restarts %d", reconfigured, f.restarts)
	}
	for _, bad := range []rpc.RestoreParams{
		{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID, Mode: "files", Foreign: fr},
		{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID, Databases: []string{"site_3"}, Foreign: fr},
		{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID, Foreign: &rpc.ForeignRestore{Hostname: ""}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate took %+v", bad)
		}
	}
	// A restore test never takes another server's snapshot.
	if res := f.verify(); res.Passed {
		t.Fatal("a restore test passed another server's snapshot")
	}
}

// SPEC §5 step 7: a failed reconfiguration undoes the restore.
func TestForeignRestoreUndoneWhenCredentialsFail(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	f.write("snap-"+dbID+".json", `[{"id":"`+dbID+`","hostname":"old-box","tags":["site:docs.example.com","part:database","dbname:site_3"]}]`)
	f.write("snap-"+filesID+".json", `[{"id":"`+filesID+`","hostname":"old-box","tags":["site:docs.example.com","kind:full","db:`+dbID+`"]}]`)
	mk(t, f.site.RootPath, map[string]string{"releases/initial/index.html": "the new site"})
	f.o.Reconfigure = func(context.Context, state.Site, rpc.ForeignRestore) error { return os.ErrPermission }
	_, err := f.o.RestoreSnapshot(ctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID,
		Foreign: &rpc.ForeignRestore{Hostname: "old-box"}})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("restore = %v", err)
	}
	if got := f.live("current/index.html"); got != "the new site" {
		t.Fatalf("the restore was not undone: %q", got)
	}
}

// SPEC §9: a deletion takes the description with the backup; a description
// nothing names any more goes as an orphan.
func TestForgetTakesTheDescription(t *testing.T) {
	f := newFixture(t)
	f.siteSnapshots(`[{"id":"` + forgetFiles + `","tags":["site:docs.example.com","db:` + forgetDB + `","manifest:` + forgetOther + `"]},` +
		`{"id":"` + forgetDB + `","tags":["site:docs.example.com","part:database"]},` +
		`{"id":"` + forgetOther + `","tags":["site:docs.example.com","part:manifest"]}]`)
	res, err := f.forget(forgetFiles)
	if err != nil || strings.Join(res.Forgotten, " ") != forgetFiles+" "+forgetDB+" "+forgetOther {
		t.Fatalf("forget = %+v %v", res, err)
	}
	if _, err := f.forget(forgetOther); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("deleting a description alone = %v", err)
	}
	f = newFixture(t)
	f.siteSnapshots(`[{"id":"` + forgetOther + `","tags":["site:docs.example.com","part:manifest"]}]`)
	if res, err := f.forget(forgetFiles); err != nil || !res.Missing || strings.Join(res.Forgotten, " ") != forgetOther {
		t.Fatalf("orphan description = %+v %v", res, err)
	}
}

// SPEC §8: a kept backup keeps its description.
func TestRetentionKeepsTheDescriptionOfAKeptBackup(t *testing.T) {
	f := newFixture(t)
	f.write("plan", `[{"host":"box","keep":[{"id":"`+snapNew+`","tags":["manifest:`+snapVerified+`"]}],`+
		`"remove":[{"id":"`+snapVerified+`"},{"id":"`+snapOld+`"}]}]`)
	res, err := f.o.BackupRetention(ctx, rpc.RetentionParams{Site: f.site, Repository: repoURL, Password: "pw", KeepWithinDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Forgotten, []string{snapOld}) || !slices.Contains(res.Kept, snapVerified) {
		t.Fatalf("retention = %+v", res)
	}
}

// SPEC §14: the sites of a repository, by domain and server, the newest
// backup of each; ForeignSnapshot reads one with its description and its
// databases, by ID or original.
func TestRepositorySitesAndForeignSnapshot(t *testing.T) {
	f := newFixture(t)
	f.write("all.json", `[`+
		`{"id":"aaaaaaaa00000001","hostname":"old-box","time":"2026-10-01T10:00:00Z","tags":["site:a.example.com"]},`+
		`{"id":"aaaaaaaa00000002","hostname":"old-box","time":"2026-10-02T10:00:00Z","tags":["site:a.example.com","manifest:`+manifestSnap+`","db:`+dbID+`"]},`+
		`{"id":"`+dbID+`","hostname":"old-box","tags":["site:a.example.com","part:database","dbname:site_7"]},`+
		`{"id":"`+manifestSnap+`","hostname":"old-box","tags":["site:a.example.com","part:manifest"]},`+
		`{"id":"bbbbbbbb00000001","hostname":"box","time":"2026-10-03T10:00:00Z","tags":["site:b.example.com"]},`+
		`{"id":"cccccccc00000001","original":"dddddddd00000001","hostname":"old-box","time":"2026-10-04T10:00:00Z","tags":["site:a.example.com"]}]`)
	res, err := f.o.RepositorySites(ctx, rpc.TestBackupParams{Repository: repoURL, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sites) != 2 || res.Sites[0].Domain != "a.example.com" || res.Sites[0].Snapshots != 3 || res.Sites[0].Snapshot != "cccccccc00000001" ||
		res.Sites[0].Manifest || res.Sites[1].Hostname != "box" {
		t.Fatalf("sites = %+v", res.Sites)
	}
	f.write("snap-aaaaaaaa00000002.json", `[{"id":"aaaaaaaa00000002","hostname":"old-box","tags":["site:a.example.com","manifest:`+manifestSnap+`","db:`+dbID+`"]}]`)
	f.write("snap-"+manifestSnap+".json", `[{"id":"`+manifestSnap+`","hostname":"old-box","tags":["site:a.example.com","part:manifest"]}]`)
	f.write("snap-"+dbID+".json", `[{"id":"`+dbID+`","hostname":"old-box","tags":["site:a.example.com","part:database","dbname:site_7"]}]`)
	f.write("dump-"+manifestSnap, `{"format":1,"domain":"a.example.com","type":"wordpress","php_version":"8.4"}`)
	one, err := f.o.ForeignSnapshot(ctx, rpc.ForeignSnapshotParams{Repository: repoURL, Password: "pw", SnapshotID: "aaaaaaaa00000002"})
	if err != nil {
		t.Fatal(err)
	}
	if one.Domain != "a.example.com" || one.Hostname != "old-box" || one.Manifest == nil || one.Manifest.Type != state.SiteWordPress ||
		!slices.Equal(one.Databases, []string{"site_7"}) {
		t.Fatalf("foreign snapshot = %+v", one)
	}
	copied, err := f.o.ForeignSnapshot(ctx, rpc.ForeignSnapshotParams{Repository: repoURL, Password: "pw", SnapshotID: "dddddddd00000001"})
	if err != nil || copied.ID != "cccccccc00000001" || copied.Manifest != nil {
		t.Fatalf("by original = %+v %v", copied, err)
	}
	if _, err := f.o.ForeignSnapshot(ctx, rpc.ForeignSnapshotParams{Repository: repoURL, Password: "pw", SnapshotID: dbID}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a database part = %v", err)
	}
}
