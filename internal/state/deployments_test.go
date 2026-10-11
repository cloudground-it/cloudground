package state

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The deployments migration (R-4.3): of a site's promoted rows only the
// newest stays promoted, the others become superseded; a safe push the old
// panel left guarding is created when it built a release and failed when
// it built none; afterwards the database itself refuses a second promoted
// row for a site.
func TestDeploymentsMigration(t *testing.T) {
	before := -1
	for i, step := range schemaSteps {
		for _, q := range step.stmts {
			if strings.Contains(q, "idx_deployments_live") {
				before = i
			}
		}
	}
	if before < 0 {
		t.Fatal("no deployments migration")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openAtVersion(path, before)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sites (id, domain, type, profile, engine, system_user, root_path, status, config, created_at, updated_at)
			VALUES (1, 'a.example.com', 'static', 'balanced', 'nginx', 'cg-site-1', '/srv/sites/a.example.com', 'active', '{}', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
		`INSERT INTO sites (id, domain, type, profile, engine, system_user, root_path, status, config, created_at, updated_at)
			VALUES (2, 'b.example.com', 'static', 'balanced', 'nginx', 'cg-site-2', '/srv/sites/b.example.com', 'active', '{}', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
		`INSERT INTO deployments (id, site_id, release_id, path, status, created_at, promoted_at) VALUES
			(1, 1, '20261001-000000', '/p1', 'promoted', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
			(2, 1, '20261002-000000', '/p2', 'promoted', '2026-10-02T00:00:00Z', '2026-10-02T00:00:00Z'),
			(3, 1, '20261003-000000', '/p3', 'guarding', '2026-10-03T00:00:00Z', NULL),
			(4, 1, '20261004-000000', '', 'guarding', '2026-10-04T00:00:00Z', NULL),
			(5, 2, '20261001-000000', '/q1', 'promoted', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
			(6, 2, '20261002-000000', '/q2', 'rolled_back', '2026-10-02T00:00:00Z', '2026-10-02T00:00:00Z')`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[int64]DeploymentStatus{1: DeploySuperseded, 2: DeployPromoted, 3: DeployCreated, 4: DeployFailed, 5: DeployPromoted, 6: DeployRolledBack}
	for id, st := range want {
		if d, err := s.GetDeployment(id); err != nil || d.Status != st {
			t.Errorf("deployment %d = %s %v, want %s", id, d.Status, err, st)
		}
	}
	if _, err := s.db.Exec(`UPDATE deployments SET status='promoted' WHERE id=1`); err == nil {
		t.Fatal("a second promoted deployment of a site was stored")
	}
}

// A site has one live release: a promote (or a record created live, as a
// migration's) supersedes the other; a rollback records the release the
// agent went back to as live and the one it left as rolled back.
func TestOneLiveDeploymentPerSite(t *testing.T) {
	s := testStore(t)
	site, err := s.CreateSite(Site{Domain: "live.example.com", Type: SiteStatic, Profile: ProfileBalanced, Engine: EngineNginx,
		SystemUser: "u", RootPath: "/srv/sites/live.example.com", Status: SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: "A", Path: "/a", Status: DeployPromoted})
	b, _ := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: "B", Path: "/b", Status: DeployCreated})
	for _, to := range []DeploymentStatus{DeployPromoting, DeployPromoted} {
		if _, err := s.MoveDeployment(b.ID, nil, to, DeploymentUpdate{}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id int64) DeploymentStatus { d, _ := s.GetDeployment(id); return d.Status }
	if get(a.ID) != DeploySuperseded || get(b.ID) != DeployPromoted {
		t.Fatalf("after promoting B: A %s, B %s", get(a.ID), get(b.ID))
	}
	m, _ := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: "M", Path: "/m", Status: DeployPromoted})
	if get(b.ID) != DeploySuperseded || get(m.ID) != DeployPromoted {
		t.Fatalf("a record created live did not take over: B %s, M %s", get(b.ID), get(m.ID))
	}
	x, _ := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: "X", Path: "/x", Status: DeployCreated})
	if _, err := s.RecordRollback(site.ID, "X"); !errors.Is(err, ErrNeverLive) || get(x.ID) != DeployCreated || get(m.ID) != DeployPromoted {
		t.Fatalf("rollback to a release never live = %v", err)
	}
	if target, err := s.RollbackTarget(site.ID); err != nil || target != "B" {
		t.Fatalf("rollback target = %q %v, want the newest superseded B", target, err)
	}
	from, err := s.RecordRollback(site.ID, "B")
	if err != nil || from.ID != m.ID || get(m.ID) != DeployRolledBack || get(b.ID) != DeployPromoted {
		t.Fatalf("rollback to B: from %+v %v, M %s, B %s", from, err, get(m.ID), get(b.ID))
	}
	// Back to a release the panel has no record of (provisioning's): none
	// is live in the records.
	if _, err := s.RecordRollback(site.ID, "initial"); err != nil || get(b.ID) != DeployRolledBack {
		t.Fatalf("rollback to initial: %v, B %s", err, get(b.ID))
	}
	// A rollback to the live one changes nothing.
	for _, to := range []DeploymentStatus{DeployPromoting, DeployPromoted} {
		s.MoveDeployment(a.ID, nil, to, DeploymentUpdate{})
	}
	if _, err := s.RecordRollback(site.ID, "A"); err != nil || get(a.ID) != DeployPromoted {
		t.Fatalf("rollback to the live release: %v, A %s", err, get(a.ID))
	}
}

// The transition table's shape: every status is known, the terminal ones
// lead nowhere, and only a built release can be promoted.
func TestDeploymentMachine(t *testing.T) {
	for _, st := range []DeploymentStatus{DeployBlocked, DeployFailed, DeployCanceled} {
		if !DeploymentTerminal(st) {
			t.Errorf("%s is not terminal", st)
		}
	}
	for from, tos := range deployMoves {
		for _, to := range tos {
			if _, known := deployMoves[to]; !known {
				t.Errorf("%s → %s: unknown status", from, to)
			}
		}
	}
	promotable := map[DeploymentStatus]bool{DeployCreated: true, DeploySuperseded: true, DeployRolledBack: true, DeployPromoted: true}
	for st := range deployMoves {
		if DeploymentPromotable(st) != promotable[st] {
			t.Errorf("promotable(%s) = %v", st, DeploymentPromotable(st))
		}
	}
	if DeploymentTerminal("nonsense") {
		t.Error("an unknown status is terminal")
	}
}

// A site names each release once (UNIQUE (site_id, release_id) since the
// first schema): a second record of its initial release is refused by the
// database itself, so no duplicate can exist to be cleaned up.
func TestOneInitialReleaseRecordPerSite(t *testing.T) {
	s := testStore(t)
	site, err := s.CreateSite(Site{Domain: "one.example.com", Type: SiteStatic, Profile: ProfileBalanced, Engine: EngineNginx,
		SystemUser: "u", RootPath: "/srv/sites/one.example.com", Status: SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: InitialRelease, Status: DeployPromoted}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO deployments (site_id, release_id, path, status, created_at) VALUES (?, ?, '', 'superseded', ?)`,
		site.ID, InitialRelease, now()); err == nil {
		t.Fatal("the database stored a second record of a site's initial release")
	}
}

// The race of #11: a reconcile and a rollback that both find no record of
// the initial release each used to insert one. Recorded together, many at
// once, there is one record, and exactly one caller made it.
func TestRecordInitialReleaseOnce(t *testing.T) {
	s := testStore(t)
	site, err := s.CreateSite(Site{Domain: "init.example.com", Type: SiteStatic, Profile: ProfileBalanced, Engine: EngineNginx,
		SystemUser: "u", RootPath: "/srv/sites/init.example.com", Status: SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	var made atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status := DeploySuperseded
			if i%2 == 0 {
				status = DeployPromoted
			}
			ok, err := s.RecordInitialRelease(site.ID, status)
			if err != nil {
				t.Error(err)
			}
			if ok {
				made.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	deps, err := s.ListDeployments(site.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].ReleaseID != InitialRelease || made.Load() != 1 {
		t.Fatalf("%d callers made %d records: %+v", callers, made.Load(), deps)
	}
	if ok, err := s.RecordInitialRelease(site.ID, DeployPromoted); ok || err != nil {
		t.Fatalf("a later call = %v %v, want no new record", ok, err)
	}
}
