package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

var errAgentDown = agentrpc.Wrap(agentrpc.CodeUnavailable, "The agent is not running", fmt.Errorf("dial"))

// noCapabilities is a server whose agent never answered Capabilities.
func noCapabilities(t *testing.T) (*Server, *fakeAgent, *httptest.Server) {
	s, agent, ts := testServer(t)
	agent.setErr(agentrpc.Capabilities.Name(), errAgentDown)
	return s, agent, ts
}

func staticSiteRecord(t *testing.T, s *Server, domain string) state.Site {
	t.Helper()
	site, err := s.Store.CreateSite(state.Site{Domain: domain, Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/" + domain, Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// A site whose PHP version was removed from the server can still change its
// limits: its own version is not checked against the installed ones.
func TestPHPSettingsKeepARemovedVersion(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site, err := s.Store.CreateSite(state.Site{Domain: "legacy.example.com", Type: state.SitePHP, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, PHPVersion: "8.0", SystemUser: "cg-site-1", RootPath: "/srv/sites/legacy.example.com", Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"php_version": "8.0", "memory_limit_mb": 512})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("limits with the site's own (removed) version = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if got, _ := s.Store.GetSite(site.ID); got.PHPVersion != "8.0" || got.Config.PHP.MemoryLimitMB != 512 {
		t.Fatalf("after = %s %d", got.PHPVersion, got.Config.PHP.MemoryLimitMB)
	}
	if resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"php_version": "8.1"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("another removed version = %d %s", resp.StatusCode, body)
	}
}

// One ask of the agent for every concurrent caller; a caller whose context
// ends stops waiting at once; the others get the answer when it comes.
func TestCapabilitiesSingleFlight(t *testing.T) {
	s, agent, _ := testServer(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	agent.setHook(func(method string) {
		if method == agentrpc.Capabilities.Name() {
			entered <- struct{}{}
			<-release
		}
	})
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.capabilities(context.Background())
			if err == nil && c.Paths.SitesRoot != "/srv/sites" {
				err = fmt.Errorf("answer %+v", c)
			}
			errs <- err
		}()
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.capabilities(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a caller with a short context: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the short caller waited for the hung agent")
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := agent.count(agentrpc.Capabilities.Name()); got != 1 {
		t.Fatalf("%d asks for %d concurrent callers", got, n+1)
	}
}

// A failed ask stands for every caller for capsRetry: the agent being down
// costs one dial, not one per waiting request.
func TestCapabilitiesFailureIsShared(t *testing.T) {
	s, agent, _ := testServer(t)
	agent.setErr(agentrpc.Capabilities.Name(), errAgentDown)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.capabilities(context.Background()); err == nil {
				t.Error("no answer ever: an error")
			}
		}()
	}
	wg.Wait()
	if _, err := s.capabilities(context.Background()); err == nil {
		t.Fatal("still an error")
	}
	if got := agent.count(agentrpc.Capabilities.Name()); got != 1 {
		t.Fatalf("%d asks after one failure", got)
	}
	// Past capsRetry the agent is asked again.
	s.caps.mu.Lock()
	s.caps.failedAt = time.Now().Add(-capsRetry - time.Second)
	s.caps.mu.Unlock()
	agent.setErr(agentrpc.Capabilities.Name(), nil)
	if _, err := s.capabilities(context.Background()); err != nil || agent.count(agentrpc.Capabilities.Name()) != 2 {
		t.Fatalf("after capsRetry: %v, %d asks", err, agent.count(agentrpc.Capabilities.Name()))
	}
}

// After a self-update the answer is refreshed until the agent's start time
// changes, then the normal TTL applies again.
func TestCapabilitiesFollowTheRestartAfterAnUpdate(t *testing.T) {
	s, agent, _ := testServer(t)
	name := agentrpc.Capabilities.Name()
	before := testCapabilities
	before.StartedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	agent.setResult(name, before)
	if _, err := s.capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.expectAgentRestart()
	// The old agent still answers: the watch goes on, with a short TTL.
	if _, err := s.capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.caps.mu.Lock()
	watching := !s.caps.watchUntil.IsZero()
	s.caps.at = time.Now().Add(-capsRestartTTL - time.Millisecond)
	s.caps.mu.Unlock()
	if !watching {
		t.Fatal("the same start time ended the watch")
	}
	after := before
	after.StartedAt = before.StartedAt.Add(time.Hour)
	after.PHPVersions = []string{"8.5"}
	agent.setResult(name, after)
	c, err := s.capabilities(context.Background())
	if err != nil || len(c.PHPVersions) != 1 || agent.count(name) != 3 {
		t.Fatalf("the restarted agent is asked within capsRestartTTL: %v %v %d", c.PHPVersions, err, agent.count(name))
	}
	s.caps.mu.Lock()
	defer s.caps.mu.Unlock()
	if !s.caps.watchUntil.IsZero() {
		t.Fatal("the new start time ends the watch")
	}
}

// Without the sites' root ever known, an orphan record under it is not
// judged, and the upgrade pass is not marked done; once the agent answers,
// the next pass prunes it and finishes.
func TestPrunePassWaitsForTheSitesRoot(t *testing.T) {
	s, agent, _ := noCapabilities(t)
	orphan := "/srv/sites/gone.example.com/current/wp-content/mu-plugins/cloudground-connector.php"
	if err := s.Store.RecordManagedFile(orphan, "abc"); err != nil {
		t.Fatal(err)
	}
	s.reconcileAfterUpgrade(context.Background())
	if done, _ := s.Store.RenderedVersion(); done != "" {
		t.Fatalf("pass marked done (%q) without the sites' root", done)
	}
	if all, _ := s.Store.ManagedFiles(); all[orphan] == "" {
		t.Fatal("the record was dropped without knowing the root")
	}
	agent.setErr(agentrpc.Capabilities.Name(), nil)
	s.forgetCapabilities()
	s.reconcileAfterUpgrade(context.Background())
	if all, _ := s.Store.ManagedFiles(); all[orphan] != "" {
		t.Fatal("the orphan under the sites' root was not pruned")
	}
	if done, _ := s.Store.RenderedVersion(); done == "" {
		t.Fatal("the complete pass is not marked done")
	}
}

// Creating or recreating staging resolves the sites' root before anything
// changes: with the agent unable to say, the old copy stays, and the answer
// is the agent's status, not a 500.
func TestStagingNeedsTheSitesRootFirst(t *testing.T) {
	s, agent, ts := noCapabilities(t)
	c := setupAdmin(t, s, ts)
	prod := staticSiteRecord(t, s, "shop.example.com")
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/staging", prod.ID), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("create staging without the root = %d %s", resp.StatusCode, body)
	}
	if _, err := s.Store.StagingSiteFor(prod.ID); err == nil {
		t.Fatal("a staging record was made without a root")
	}

	old, err := s.Store.CreateSite(state.Site{Domain: "staging.shop.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-2", RootPath: "/srv/sites/staging.shop.example.com", Status: state.SiteActive, StagingOf: prod.ID})
	if err != nil {
		t.Fatal(err)
	}
	resp, body = c.do("POST", fmt.Sprintf("/api/sites/%d/staging/recreate", prod.ID), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("recreate staging without the root = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if agent.called(agentrpc.DeleteSite.Name()) {
		t.Fatal("the old staging copy was deleted")
	}
	if got, err := s.Store.StagingSiteFor(prod.ID); err != nil || got.ID != old.ID {
		t.Fatalf("the old staging copy is gone: %v", err)
	}

	// With the agent answering, the copy is made where it keeps sites.
	moved := testCapabilities
	moved.Paths.SitesRoot = "/data/sites"
	agent.setErr(agentrpc.Capabilities.Name(), nil)
	agent.setResult(agentrpc.Capabilities.Name(), moved)
	s.forgetCapabilities()
	resp, body = c.do("POST", fmt.Sprintf("/api/sites/%d/staging/recreate", prod.ID), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("recreate = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	stg, err := s.Store.StagingSiteFor(prod.ID)
	if err != nil || stg.ID == old.ID || stg.RootPath != "/data/sites/staging.shop.example.com" {
		t.Fatalf("recreated staging = %+v %v", stg, err)
	}
}
