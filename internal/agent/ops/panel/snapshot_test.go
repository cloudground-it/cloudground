package panel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withState gives the fixture a state database, its WAL, and the agent's
// records, as the previous release left them.
func (f *guardFixture) withState(t *testing.T) (db, sites string) {
	t.Helper()
	lib := filepath.Join(f.dir, "lib")
	os.MkdirAll(lib, 0o750)
	db = filepath.Join(lib, "state.db")
	sites = filepath.Join(f.g.Plan.StateDir, "sites.json")
	os.WriteFile(db, []byte("schema 22"), 0o640)
	os.WriteFile(db+"-wal", []byte("wal of 22"), 0o640)
	os.WriteFile(sites, []byte(`{"sites":{"1":"a.test"}}`), 0o600)
	f.g.Plan.StateDB = db
	f.g.Plan.Records = []string{sites, filepath.Join(f.g.Plan.StateDir, "databases.json")} // the second does not exist
	f.g.Plan.SnapshotRoot = filepath.Join(f.g.Plan.StateDir, SnapshotDirName)
	f.g.Plan.KeepSnapshots = 2
	return db, sites
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// Before the new release first starts, both services are stopped and the
// state copied; a healthy release keeps what it migrated, and the snapshot.
func TestGuardSnapshotsBeforeTheNewReleaseStarts(t *testing.T) {
	f := newGuardFixture(t)
	db, _ := f.withState(t)
	// The new API migrates when it starts: by then the snapshot must exist.
	var snapAtStart []string
	f.g.Healthz = func(context.Context) (int, error) {
		if snapAtStart == nil {
			snapAtStart = Snapshots(f.g.Plan.SnapshotRoot)
			os.WriteFile(db, []byte("schema 23"), 0o640)
		}
		return 200, nil
	}
	if err := f.g.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(snapAtStart) != 1 {
		t.Fatalf("snapshots when the new release answered: %v", snapAtStart)
	}
	r := f.result(t)
	if !r.OK || r.Snapshot != snapAtStart[0] || r.Started == "" {
		t.Fatalf("result %+v", r)
	}
	snap, err := ReadSnapshot(r.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if snap.From != "v0.3.0" || snap.Version != "v0.4.0" || len(snap.Files) != 4 {
		t.Fatalf("snapshot %+v", snap)
	}
	if got := read(t, filepath.Join(r.Snapshot, "state.db")); got != "schema 22" {
		t.Fatalf("snapshot holds %q", got)
	}
	if read(t, db) != "schema 23" {
		t.Fatal("a healthy release lost what it migrated")
	}
	if fi, _ := os.Stat(r.Snapshot); fi.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot dir mode %v", fi.Mode().Perm())
	}
}

// An unhealthy release is rolled back with the state it migrated: the
// database, its WAL and the records come back byte for byte, a file the
// release created goes away, and the shared-memory index is dropped.
func TestGuardRollbackRestoresTheSnapshot(t *testing.T) {
	f := newGuardFixture(t)
	db, sites := f.withState(t)
	reg := f.g.Plan.Records[1]
	f.g.Healthz = func(context.Context) (int, error) {
		if f.live(t, "panel-api") == "new" {
			os.WriteFile(db, []byte("schema 23"), 0o640)
			os.WriteFile(db+"-wal", []byte("wal of 23"), 0o640)
			os.WriteFile(db+"-shm", []byte("shm"), 0o640)
			os.WriteFile(sites, []byte(`{"sites":{}}`), 0o600)
			os.WriteFile(reg, []byte(`{}`), 0o600)
			return 503, nil
		}
		return 200, nil
	}
	if err := f.g.Run(context.Background()); err == nil {
		t.Fatal("an unhealthy release was kept")
	}
	r := f.result(t)
	if !r.RolledBack || !r.RestoredHealthy || !r.StateRestored {
		t.Fatalf("result %+v", r)
	}
	for p, want := range map[string]string{db: "schema 22", db + "-wal": "wal of 22", sites: `{"sites":{"1":"a.test"}}`} {
		if got := read(t, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{reg, db + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the rollback", p)
		}
	}
	if fi, _ := os.Stat(db); fi.Mode().Perm() != 0o640 {
		t.Errorf("restored db mode %v", fi.Mode().Perm())
	}
}

// A snapshot that cannot be taken stops the update before the new release
// runs: the previous binaries go back and the panel starts on them.
func TestGuardAbortsWithoutASnapshot(t *testing.T) {
	f := newGuardFixture(t)
	db, _ := f.withState(t)
	os.Remove(db)
	os.Symlink("/etc/passwd", db) // the API's user could plant this
	if err := f.g.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("err = %v", err)
	}
	r := f.result(t)
	if r.OK || !r.RolledBack || !r.RestoredHealthy || r.Snapshot != "" {
		t.Fatalf("result %+v", r)
	}
	for _, b := range Binaries {
		if f.live(t, b) != "old" {
			t.Fatalf("%s is the new release after an abort", b)
		}
	}
	if HighestInstalled(f.g.Plan.StateDir) != "" {
		t.Fatal("an aborted update raised the floor")
	}
	if len(Snapshots(f.g.Plan.SnapshotRoot)) != 0 {
		t.Fatal("a partial snapshot was left")
	}
}

// Only the newest KeepSnapshots are kept, and nothing else in the
// directory is touched.
func TestPruneSnapshots(t *testing.T) {
	root := t.TempDir()
	names := []string{"20261001T000000Z-v0.1.0-to-v0.2.0", "20261002T000000Z-v0.2.0-to-v0.3.0", "20261003T000000Z-v0.3.0-to-v0.4.0", "notes"}
	for _, n := range names {
		os.Mkdir(filepath.Join(root, n), 0o700)
	}
	if err := pruneSnapshots(root, 2); err != nil {
		t.Fatal(err)
	}
	got := Snapshots(root)
	if len(got) != 2 || filepath.Base(got[0]) != names[1] || filepath.Base(got[1]) != names[2] {
		t.Fatalf("kept %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "notes")); err != nil {
		t.Fatal("pruning removed a directory that is not a snapshot")
	}
}

// A restore refuses a copy that does not match its recorded hash, and
// leaves the live file alone.
func TestRestoreChecksTheHash(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "state.db")
	os.WriteFile(live, []byte("before"), 0o640)
	p := Plan{Version: "v2", Previous: "v1", StateDB: live, SnapshotRoot: filepath.Join(dir, "snaps")}
	snap, err := p.takeSnapshot(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(snap.Dir, "state.db"), []byte("tampered"), 0o600)
	os.WriteFile(live, []byte("after"), 0o640)
	if err := p.restoreSnapshot(snap); err == nil {
		t.Fatal("a tampered snapshot was restored")
	}
	if read(t, live) != "after" {
		t.Fatal("the live file was replaced by a tampered copy")
	}
}
