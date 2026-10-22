package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The services are called directly here, without HTTP (SPEC.md §3).

func problemOf(t *testing.T, err error) *problem {
	t.Helper()
	var p *problem
	if !errors.As(err, &p) {
		t.Fatalf("err = %v, want a problem", err)
	}
	return p
}

// R-4.3: a create the queue refuses (the panel is stopping) leaves no
// record behind; it used to stay "provisioning" with nothing to finish it.
func TestCreateRefusedByTheQueueLeavesNoRecord(t *testing.T) {
	s, _, _ := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.DrainJobs(ctx)
	_, _, err := s.siteSvc().Create(context.Background(), "test", newSite{Domain: "late.example.com", Type: "static"})
	var qf queueFailure
	if !errors.As(err, &qf) || !errors.Is(err, jobs.ErrDraining) {
		t.Fatalf("err = %v, want the queue's refusal", err)
	}
	if _, err := s.Store.GetSiteByDomain("late.example.com"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("the record stayed: %v", err)
	}
}

func TestCreateDefaultsAndRefusals(t *testing.T) {
	s, _, _ := testServer(t)
	svc := s.siteSvc()
	site, _, err := svc.Create(context.Background(), "test", newSite{Domain: " App.Example.com ", Type: "laravel"})
	if err != nil {
		t.Fatal(err)
	}
	if site.Domain != "app.example.com" || site.Profile != state.ProfileBalanced || site.PHPVersion != "8.4" ||
		site.Config.PublicRoot != "public" || !site.Config.CacheEnabled || site.SystemUser != fmt.Sprintf("cg-site-%d", site.ID) ||
		site.Config.Database.Name != fmt.Sprintf("site_%d", site.ID) || site.RootPath != "/srv/sites/app.example.com" ||
		!site.Config.Backups.Enabled || site.Config.Backups.RetentionDays != 30 {
		t.Fatalf("defaults = %+v", site)
	}
	waitForTasks(t, s)
	_, _, err = svc.Create(context.Background(), "test", newSite{Domain: "app.example.com", Type: "static"})
	if p := problemOf(t, err); p.kind != probConflict {
		t.Fatalf("duplicate = %+v", p)
	}
	no := false
	st, _, err := svc.Create(context.Background(), "test", newSite{Domain: "flat.example.com", Type: "static", PHPVersion: "8.3", CacheEnabled: &no})
	if err != nil || st.PHPVersion != "" || st.Config.CacheEnabled || st.Config.Database.Enabled {
		t.Fatalf("static = %+v %v", st, err)
	}
	for _, bad := range []newSite{
		{Domain: "bad_domain", Type: "static"},
		{Domain: "x.example.com", Type: "cobol"},
		{Domain: "x.example.com", Type: "static", Profile: "turbo"},
		{Domain: "x.example.com", Type: "wordpress", AdminEmail: "nope"},
		{Domain: "x.example.com", Type: "wordpress", AdminEmail: "a@b.example", AdminUser: "a", AdminPassword: "short"},
		{Domain: "x.example.com", Type: "proxy", ProxyUpstream: "ftp://x"},
		{Domain: "x.example.com", Type: "php", PHPVersion: "8.9"},
	} {
		if _, _, err := svc.Create(context.Background(), "test", bad); problemOf(t, err).kind != probInvalid {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	waitForTasks(t, s)
}

// A payload written before R-4.3 (the PHP route's, without the newer
// fields) still runs, and a failed render puts back only what it changed.
func TestApplyRunsAPayloadFromBeforeTheUnifiedPatch(t *testing.T) {
	s, agent, _ := testServer(t)
	site := seedActiveSite(t, s, "legacy.example.com", state.SitePHP, "8.4", 0)
	site, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Config.PublicRoot = "web"
		st.Config.PHP.UploadMaxMB = 200
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.setErr(agentrpc.ApplySiteConfig.Name(), fmt.Errorf("simulated render failure"))
	old := fmt.Sprintf(`{"ours":{"profile":"balanced","php_version":"8.4","upload_max_mb":200},`+
		`"previous":{"profile":"balanced","php_version":"8.4","upload_max_mb":64},"prev_generation":%d,"our_generation":%d}`,
		site.DesiredGeneration-1, site.DesiredGeneration)
	j := runJobNow(t, s, jobs.Spec{Kind: kindApplyPHP, SiteID: site.ID, Locks: siteLocks(site.ID), Payload: json.RawMessage(old)})
	if j.Status != state.JobFailed {
		t.Fatalf("job = %s", j.Status)
	}
	got, _ := s.Store.GetSite(site.ID)
	if got.Config.PHP.UploadMaxMB != 64 || got.Config.PublicRoot != "web" {
		t.Fatalf("after the revert: upload %d, public root %q", got.Config.PHP.UploadMaxMB, got.Config.PublicRoot)
	}
}

func TestStagingServiceWithoutACopy(t *testing.T) {
	s, _, _ := testServer(t)
	prod := seedActiveSite(t, s, "solo.example.com", state.SiteStatic, "", 0)
	st := s.stagingSvc()
	if _, err := st.Of(prod); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Of = %v", err)
	}
	if _, _, err := st.Tables(context.Background(), prod); problemOf(t, err).kind != probPrecondition {
		t.Fatal("tables without a copy")
	}
	if _, err := st.Diff(context.Background(), prod); problemOf(t, err).kind != probNotFound {
		t.Fatal("diff without a copy")
	}
	if _, err := st.Credentials("test", prod); problemOf(t, err).kind != probNotFound {
		t.Fatal("credentials without a copy")
	}
	if _, err := st.Recreate(context.Background(), "test", prod); problemOf(t, err).kind != probNotFound {
		t.Fatal("recreate without a copy")
	}

	// The original is in maintenance: its copy is not, and has no key.
	prod, err := s.Store.MutateSite(prod.ID, func(x *state.Site) error {
		x.Config.Maintenance = state.SiteMaintenance{Enabled: true, Key: "0123456789abcdef0123456789abcdef", Message: "back soon"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stg, _, err := st.Create(context.Background(), "test", prod)
	if err != nil {
		t.Fatal(err)
	}
	if m := stg.Config.Maintenance; m.Enabled || m.Key != "" || m.Message != "back soon" {
		t.Fatalf("the copy inherited the maintenance mode: %+v", m)
	}
	if stg.Domain != "staging.solo.example.com" || stg.StagingOf != prod.ID || stg.Config.StagingAuthHash == "" {
		t.Fatalf("staging = %+v", stg)
	}
	waitForTasks(t, s)
	if _, _, err := st.Create(context.Background(), "test", prod); problemOf(t, err).kind != probConflict {
		t.Fatal("a second copy")
	}
	if _, _, err := st.Create(context.Background(), "test", stg); problemOf(t, err).kind != probInvalid {
		t.Fatal("staging of staging")
	}
	cred, err := st.Credentials("test", prod)
	if err != nil || cred.URL != "https://staging.solo.example.com/" || cred.User != "staging" || len(cred.Password) < 15 {
		t.Fatalf("credentials = %+v %v", cred, err)
	}
}
