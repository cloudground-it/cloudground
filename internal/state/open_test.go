package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testStore is a fresh store in a temporary directory, with its key beside
// it, closed when the test ends.
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedSite records a site with the fields every row needs, under domain.
func seedSite(t *testing.T, s *Store, domain string, typ SiteType, status SiteStatus, stagingOf int64) Site {
	t.Helper()
	site, err := s.CreateSite(Site{Domain: domain, Type: typ, Profile: ProfileBalanced, Engine: EngineNginx,
		SystemUser: "cg-" + domain, RootPath: "/srv/sites/" + domain, Status: status, StagingOf: stagingOf})
	if err != nil {
		t.Fatalf("create %s: %v", domain, err)
	}
	return site
}

// A second Open of an up-to-date database runs no step again and changes
// nothing it recorded.
func TestReopenRunsNoStepTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ledger := func() (rows, top int) {
		t.Helper()
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer s.Close()
		if err := s.db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_migrations`).Scan(&rows, &top); err != nil {
			t.Fatal(err)
		}
		return rows, top
	}
	firstRows, firstTop := ledger()
	againRows, againTop := ledger()
	if firstRows != latestSchema || firstTop != latestSchema {
		t.Fatalf("first open recorded %d steps up to %d, want %d", firstRows, firstTop, latestSchema)
	}
	if againRows != firstRows || againTop != firstTop {
		t.Fatalf("a second open recorded %d steps up to %d, want them unchanged", againRows, againTop)
	}
}

// The database and its WAL sidecars hold credentials: 0640 whatever the
// umask, set again on every Open.
func TestOpenSetsTheFileModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for round := 0; round < 2; round++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO audit_events (actor, action, subject, created_at) VALUES ('t', 'a', 's', ?)`, now()); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{path, path + "-wal", path + "-shm"} {
			info, err := os.Stat(f)
			if os.IsNotExist(err) && f != path {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != 0o640 {
				t.Errorf("round %d: %s has mode %04o, want 0640", round, filepath.Base(f), mode)
			}
		}
		s.Close()
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
	}
}

// Step 10 drops a table and a column nothing read. A database from before
// it upgrades with its drift history intact.
func TestUpgradePastStep10KeepsDriftHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openAtVersion(path, 9)
	if err != nil {
		t.Fatalf("open at 9: %v", err)
	}
	for _, q := range []string{
		// The console session needs no site behind it for this.
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO adminer_sessions (token_hash, site_id, expires_at, created_at) VALUES ('x', 42, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`,
		`INSERT INTO drift_events (path, expected_hash, actual_hash, diff, status, detected_at) VALUES ('/etc/nginx/old.conf', 'e', 'a', 'never filled', 'open', '2026-03-01T00:00:00Z')`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if ok, err := hasTable(s.db, "adminer_sessions"); err != nil || ok {
		t.Errorf("adminer_sessions after the upgrade: present=%v err=%v", ok, err)
	}
	var diffCols int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('drift_events') WHERE name = 'diff'`).Scan(&diffCols); err != nil || diffCols != 0 {
		t.Errorf("drift_events.diff after the upgrade: %d columns, err %v", diffCols, err)
	}
	open, err := s.ListDriftEvents(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Path != "/etc/nginx/old.conf" || open[0].ExpectedHash != "e" || open[0].ActualHash != "a" {
		t.Fatalf("drift history after the upgrade = %+v", open)
	}
}

// The runner refuses a history whose versions are not its positions.
func TestSchemaStepsAreInOrder(t *testing.T) {
	for i, step := range schemaSteps {
		if step.version != i+1 {
			t.Fatalf("schemaSteps[%d] is version %d", i, step.version)
		}
	}
	saved := schemaSteps
	t.Cleanup(func() { schemaSteps = saved })
	schemaSteps = append([]schemaStep{}, saved...)
	schemaSteps[1], schemaSteps[2] = schemaSteps[2], schemaSteps[1]
	if _, err := OpenWith(filepath.Join(t.TempDir(), "state.db"), Options{}); err == nil {
		t.Fatal("a history out of order was applied")
	}
}

// A database a newer build has migrated is refused, and left as it is: the
// way back is the pre-update snapshot, not this build's guesswork (R-6.5).
func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, '2030-01-01T00:00:00Z')`, latestSchema+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(path)
	var newer *SchemaNewerError
	if !errors.As(err, &newer) || newer.Have != latestSchema+1 || newer.Known != latestSchema {
		t.Fatalf("Open of a newer schema = %v, want a SchemaNewerError", err)
	}
	if !strings.Contains(err.Error(), "update-snapshots") {
		t.Fatalf("the refusal does not say where the way back is: %v", err)
	}
}
