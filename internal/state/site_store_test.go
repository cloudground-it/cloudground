package state

import (
	"errors"
	"sync"
	"testing"
)

// A site's scalar fields, aliases and JSON config come back as written; a
// mutation persists; a second site on the domain is refused; deleting
// leaves nothing to read.
func TestSiteRecordRoundTrip(t *testing.T) {
	s := testStore(t)
	in := Site{
		Domain: "roundtrip.example.com", Aliases: []string{"www.roundtrip.example.com"},
		Type: SiteWordPress, Profile: ProfileCommerce, Engine: EngineNginx, PHPVersion: "8.4",
		SystemUser: "cg-site-1", RootPath: "/srv/sites/roundtrip.example.com", Status: SiteProvisioning,
		Config: SiteConfig{
			CacheEnabled: true, CacheTTLSec: 600,
			Database:  DatabaseConfig{Enabled: true, Host: "127.0.0.1", Port: 3306, Name: "site_1", User: "site_1"},
			Backups:   BackupPolicy{Enabled: true, Schedule: "hourly", RetentionDays: 14},
			PHP:       PHPSettings{MemoryLimitMB: 512},
			Resources: Resources{PHPWorkers: 6},
		},
	}
	created, err := s.CreateSite(in)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 {
		t.Fatal("no ID assigned")
	}
	got, err := s.GetSiteByDomain(in.Domain)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.Profile != ProfileCommerce || got.PHPVersion != "8.4" || got.Status != SiteProvisioning {
		t.Fatalf("scalar fields = %+v", got)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "www.roundtrip.example.com" {
		t.Fatalf("aliases = %v", got.Aliases)
	}
	if got.Config.Database != in.Config.Database || got.Config.Backups != in.Config.Backups ||
		got.Config.PHP != in.Config.PHP || got.Config.Resources != in.Config.Resources || got.Config.CacheTTLSec != 600 {
		t.Fatalf("config = %+v, want %+v", got.Config, in.Config)
	}

	if _, err := s.MutateSite(got.ID, func(st *Site) error {
		st.Status, st.Config.ObjectCache = SiteActive, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.GetSite(got.ID); after.Status != SiteActive || !after.Config.ObjectCache {
		t.Fatalf("after the mutation: %+v", after)
	}

	dup := in
	dup.Type, dup.SystemUser, dup.RootPath = SiteStatic, "other", "/srv/sites/other"
	if _, err := s.CreateSite(dup); err == nil {
		t.Fatal("a second site on the same domain was recorded")
	}

	if err := s.DeleteSite(got.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSite(got.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after delete: %v, want ErrNotFound", err)
	}
}

// Configs written while BackupPolicy had a repository field (never read,
// since removed) still decode.
func TestConfigWithTheRetiredRepositoryKeyDecodes(t *testing.T) {
	s := testStore(t)
	site := seedSite(t, s, "legacy.example.com", SiteStatic, SiteActive, 0)
	legacy := `{"cache_enabled":true,"backups":{"enabled":true,"schedule":"weekly","retention_days":60,"repository":"default"}}`
	if _, err := s.db.Exec(`UPDATE sites SET config = ? WHERE id = ?`, legacy, site.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSite(site.ID)
	if err != nil {
		t.Fatalf("decode a config with backups.repository: %v", err)
	}
	if want := (BackupPolicy{Enabled: true, Schedule: "weekly", RetentionDays: 60}); got.Config.Backups != want || !got.Config.CacheEnabled {
		t.Fatalf("decoded %+v", got.Config)
	}
}

// Two writers changing different fields of a site at once both land
// (R-0.5): a write of the whole row from an earlier read would put one of
// them back.
func TestConcurrentMutationsBothLand(t *testing.T) {
	s := testStore(t)
	site := seedSite(t, s, "concurrent.example.com", SiteWordPress, SiteActive, 0)
	set := func(cache, sftp bool) func(*Site) error {
		return func(st *Site) error {
			if cache {
				st.Config.ObjectCache = true
			}
			if sftp {
				st.Config.SFTPEnabled = true
			}
			return nil
		}
	}
	for round := 0; round < 50; round++ {
		var wg sync.WaitGroup
		gate := make(chan struct{})
		errs := make(chan error, 2)
		for _, fn := range []func(*Site) error{set(true, false), set(false, true)} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				_, err := s.MutateSite(site.ID, fn)
				errs <- err
			}()
		}
		close(gate)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.GetSite(site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Config.ObjectCache || !got.Config.SFTPEnabled {
			t.Fatalf("round %d lost a change: object_cache=%v sftp=%v", round, got.Config.ObjectCache, got.Config.SFTPEnabled)
		}
		if _, err := s.MutateSite(site.ID, func(st *Site) error {
			st.Config.ObjectCache, st.Config.SFTPEnabled = false, false
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// The callback's error aborts the mutation and comes back unwrapped; a
// missing site is ErrNotFound; neither leaves the store unable to write.
func TestFailedMutationLeavesTheRowAlone(t *testing.T) {
	s := testStore(t)
	site := seedSite(t, s, "abort.example.com", SiteStatic, SiteActive, 0)
	refused := errors.New("refused by the callback")
	_, err := s.MutateSite(site.ID, func(st *Site) error {
		st.Status = SiteError
		return refused
	})
	if err != refused {
		t.Fatalf("MutateSite = %v, want the callback's error itself", err)
	}
	if got, _ := s.GetSite(site.ID); got.Status != SiteActive {
		t.Fatalf("an aborted mutation was kept: status %s", got.Status)
	}
	if _, err := s.MutateSite(site.ID+1000, func(*Site) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing site: %v, want ErrNotFound", err)
	}
	if _, err := s.MutateSite(site.ID, func(st *Site) error { st.Status = SiteError; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSite(site.ID); got.Status != SiteError {
		t.Fatalf("status %s after a committed mutation", got.Status)
	}
}

// An operator reaches the sites assigned to them and their staging copies,
// nothing else; the list says the same.
func TestOperatorAccessFollowsAssignment(t *testing.T) {
	s := testStore(t)
	mine := seedSite(t, s, "mine.example.com", SiteStatic, SiteActive, 0)
	mineStaging := seedSite(t, s, "staging.mine.example.com", SiteStatic, SiteActive, mine.ID)
	theirs := seedSite(t, s, "theirs.example.com", SiteStatic, SiteActive, 0)
	theirsStaging := seedSite(t, s, "staging.theirs.example.com", SiteStatic, SiteActive, theirs.ID)
	op, err := s.CreateUser("operator@example.com", "hash", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserSites(op.ID, []int64{mine.ID}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		site Site
		want bool
	}{{mine, true}, {mineStaging, true}, {theirs, false}, {theirsStaging, false}} {
		ok, err := s.UserCanAccessSite(op.ID, c.site.ID)
		if err != nil || ok != c.want {
			t.Errorf("access to %s = %v (%v), want %v", c.site.Domain, ok, err, c.want)
		}
	}
	listed, err := s.ListSitesForUser(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != mine.ID || listed[1].ID != mineStaging.ID {
		t.Fatalf("operator's list = %+v, want the site and its staging copy, by domain", listed)
	}
}

// ListUsers reads every operator's assignments with one more query on the
// store's single connection; reading them inside the first query's rows
// would wait on itself forever.
func TestListUsersCarriesAssignments(t *testing.T) {
	s := testStore(t)
	site := seedSite(t, s, "assigned.example.com", SiteStatic, SiteActive, 0)
	op, err := s.CreateUser("op@example.com", "hash", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserSites(op.ID, []int64{site.ID}); err != nil {
		t.Fatal(err)
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || len(users[0].SiteIDs) != 1 || users[0].SiteIDs[0] != site.ID {
		t.Fatalf("ListUsers = %+v", users)
	}
}
