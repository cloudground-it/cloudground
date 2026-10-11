package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// seedActiveSite records an active site directly in the store.
func seedActiveSite(t *testing.T, s *Server, domain string, typ state.SiteType, php string, memHighMB int) state.Site {
	t.Helper()
	site, err := s.Store.CreateSite(state.Site{
		Domain: domain, Type: typ, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		PHPVersion: php, SystemUser: "pending", RootPath: "/srv/sites/" + domain, Status: state.SiteActive,
		Config: state.SiteConfig{Resources: state.Resources{MemoryHighMB: memHighMB}},
	})
	if err != nil {
		t.Fatal(err)
	}
	site, err = s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.SystemUser = fmt.Sprintf("cg-site-%d", st.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// reconcileFixture seeds one site of each kind that matters to the
// reconcile: a static site, a proxy, a WordPress site sized from the site
// count, and a PHP site with its own memory limit.
func reconcileFixture(t *testing.T) (*Server, *fakeAgent, map[string]state.Site) {
	s, agent, _ := testServer(t)
	sites := map[string]state.Site{
		"static": seedActiveSite(t, s, "static.example.com", state.SiteStatic, "", 0),
		"proxy":  seedActiveSite(t, s, "proxy.example.com", state.SiteProxy, "", 0),
		"wp":     seedActiveSite(t, s, "wp.example.com", state.SiteWordPress, "8.4", 0),
		"pinned": seedActiveSite(t, s, "pinned.example.com", state.SitePHP, "8.4", 512),
	}
	return s, agent, sites
}

func applyCalls(agent *fakeAgent) (applies []rpc.SiteRef, crontabs int) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	for _, c := range agent.history {
		switch c.method {
		case agentrpc.ApplySiteConfig.Name():
			applies = append(applies, c.params.(rpc.SiteRef))
		case agentrpc.ApplyCron.Name():
			crontabs++
		}
	}
	return applies, crontabs
}

// R-0.7: a site created or deleted changes only the pools sized from the
// number of sites. Re-applying every site reloaded every tenant's PHP master
// (and emptied its OPcache) and nginx, once per site, on every create or
// delete.
func TestSiteCountChangeReappliesOnlyCountSizedPools(t *testing.T) {
	s, agent, sites := reconcileFixture(t)
	if err := s.Store.SetRenderedVersion(version.Version + "/3"); err != nil {
		t.Fatal(err)
	}
	s.reconcileAfterUpgrade(context.Background())

	applies, crontabs := applyCalls(agent)
	if len(applies) != 1 || applies[0].Site.ID != sites["wp"].ID {
		var got []string
		for _, a := range applies {
			got = append(got, a.Site.Domain)
		}
		t.Fatalf("a site-count change re-applied %v, want only wp.example.com (the one pool sized from the count)", got)
	}
	if !applies[0].ReloadOnlyIfChanged {
		t.Error("a site-count re-apply did not ask the agent to leave unchanged masters and nginx running")
	}
	if crontabs != 0 {
		t.Errorf("a site-count change re-rendered %d crontabs; nothing in them depends on the count", crontabs)
	}
	if got, _ := s.Store.RenderedVersion(); got != version.Version+"/4" {
		t.Fatalf("rendered_version = %q, want %q", got, version.Version+"/4")
	}

	// Nothing changed since: the next tick does nothing.
	s.reconcileAfterUpgrade(context.Background())
	if again, _ := applyCalls(agent); len(again) != 1 {
		t.Fatalf("an unchanged count re-applied %d more sites", len(again)-1)
	}
}

// A new version can change any template: every active site is re-applied,
// with the full reload, and the crontab of every site that has one (WordPress,
// or a site with a job) is installed again: the agent renders it.
func TestVersionChangeReappliesEverySite(t *testing.T) {
	s, agent, sites := reconcileFixture(t)
	if err := s.Store.SetRenderedVersion("0.0.0-older/4"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.CreateCronJob(state.CronJob{SiteID: sites["static"].ID, Schedule: "* * * * *", Command: "true", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s.reconcileAfterUpgrade(context.Background())

	applies, crontabs := applyCalls(agent)
	seen := map[int64]bool{}
	for _, a := range applies {
		seen[a.Site.ID] = true
		if a.ReloadOnlyIfChanged {
			t.Errorf("an upgrade re-apply of %s skipped reloads; a new version reloads everything", a.Site.Domain)
		}
	}
	for name, site := range sites {
		if !seen[site.ID] {
			t.Errorf("an upgrade did not re-apply the %s site", name)
		}
	}
	if crontabs != 2 {
		t.Errorf("an upgrade re-rendered %d crontabs, want 2 (the WordPress site and the static one with a job)", crontabs)
	}
	if got, _ := s.Store.RenderedVersion(); got != version.Version+"/4" {
		t.Fatalf("rendered_version = %q, want %q", got, version.Version+"/4")
	}
}

// A failed re-apply is retried on the next tick, still as a count-only one.
func TestSiteCountReconcileRetriesAfterFailure(t *testing.T) {
	s, agent, _ := reconcileFixture(t)
	if err := s.Store.SetRenderedVersion(version.Version + "/5"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = fmt.Errorf("simulated render failure")
	agent.mu.Unlock()
	s.reconcileAfterUpgrade(context.Background())
	if got, _ := s.Store.RenderedVersion(); got != version.Version+"/5" {
		t.Fatalf("rendered_version = %q after a failed re-apply, want it left for a retry", got)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	s.reconcileAfterUpgrade(context.Background())
	if got, _ := s.Store.RenderedVersion(); got != version.Version+"/4" {
		t.Fatalf("rendered_version = %q after the retry, want %q", got, version.Version+"/4")
	}
	applies, _ := applyCalls(agent)
	if len(applies) != 2 {
		t.Fatalf("%d re-applies over the failure and the retry, want 2 (the one count-sized pool, twice)", len(applies))
	}
}

func TestBudgetFollowsSiteCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		site state.Site
		want bool
	}{
		{"static", state.Site{Type: state.SiteStatic}, false},
		{"proxy", state.Site{Type: state.SiteProxy}, false},
		{"wordpress, automatic budget", state.Site{Type: state.SiteWordPress, PHPVersion: "8.4"}, true},
		{"woocommerce, automatic budget", state.Site{Type: state.SiteWooCommerce, PHPVersion: "8.4"}, true},
		{"laravel, automatic budget", state.Site{Type: state.SiteLaravel, PHPVersion: "8.4"}, true},
		{"php, own memory limit", state.Site{Type: state.SitePHP, PHPVersion: "8.4",
			Config: state.SiteConfig{Resources: state.Resources{MemoryHighMB: 512}}}, false},
		{"php without a version", state.Site{Type: state.SitePHP}, false},
	} {
		if got := budgetFollowsSiteCount(tc.site); got != tc.want {
			t.Errorf("%s: budgetFollowsSiteCount = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// POST /api/system/maintenance runs the scheduler's tick at once and answers
// when it is done: the site-count reconcile has happened (and is audited)
// by the time the response arrives, and the run itself is in the audit log
// under the administrator who asked for it.
func TestRunMaintenanceNowRunsOneTick(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.UpdateCheck = false; return nil })
	wp := seedActiveSite(t, s, "wp.example.com", state.SiteWordPress, "8.4", 0)
	if err := s.Store.SetRenderedVersion(version.Version + "/3"); err != nil {
		t.Fatal(err)
	}

	resp, body := c.do("POST", "/api/system/maintenance", map[string]string{})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ran":true`) {
		t.Fatalf("maintenance = %d %s", resp.StatusCode, body)
	}
	applies, _ := applyCalls(agent)
	if len(applies) != 1 || applies[0].Site.ID != wp.ID {
		t.Fatalf("the tick re-applied %d sites, want the one count-sized pool", len(applies))
	}
	if got, _ := s.Store.RenderedVersion(); got != version.Version+"/1" {
		t.Fatalf("rendered_version = %q after the tick, want %q", got, version.Version+"/1")
	}
	events, err := s.Store.ListAuditEvents(50)
	if err != nil {
		t.Fatal(err)
	}
	var resized, ran bool
	for _, e := range events {
		resized = resized || e.Action == "sites.resized"
		ran = ran || (e.Action == "system.maintenance" && e.Actor == "admin@example.com")
	}
	if !resized || !ran {
		t.Fatalf("audit after the run: sites.resized %v, system.maintenance by the admin %v (%+v)", resized, ran, events)
	}

	// A reconcile already running (the scheduler's) is joined: the run-now
	// answers once it has ended, and it does not run a second one.
	if err := s.Store.SetRenderedVersion(version.Version + "/3"); err != nil {
		t.Fatal(err)
	}
	inApply, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	agent.setHook(func(method string) {
		if method == agentrpc.ApplySiteConfig.Name() {
			once.Do(func() { close(inApply) })
			<-release
		}
	})
	if _, err := s.Jobs.Enqueue(jobs.Spec{Kind: "maintenance.reconcile", DedupKey: "schedule:maintenance.reconcile", Locks: []string{jobs.RenderLock}}); err != nil {
		t.Fatal(err)
	}
	<-inApply
	done := make(chan int, 1)
	go func() {
		r, _ := c.do("POST", "/api/system/maintenance", map[string]string{})
		done <- r.StatusCode
	}()
	select {
	case code := <-done:
		close(release)
		t.Fatalf("the run-now answered %d while a reconcile was running", code)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the run-now after the reconcile = %d", code)
	}
	if again, _ := applyCalls(agent); len(again) != 2 {
		t.Fatalf("%d re-applies in all, want 2: the run-now joined the running reconcile instead of starting another", len(again))
	}
}
