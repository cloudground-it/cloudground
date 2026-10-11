package api

// The adversarial review of R-4.3: each test fails on the branch as it was
// reviewed (63e9447).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// failFirstApply makes the first ApplySiteConfig wait for release and then
// fail; later ones succeed. entered receives when the first one starts.
func failFirstApply(agent *fakeAgent) (entered chan struct{}, release func()) {
	entered = make(chan struct{}, 1)
	gate := make(chan struct{})
	var n atomic.Int32
	agent.setHook(func(m string) {
		if m != agentrpc.ApplySiteConfig.Name() {
			return
		}
		if n.Add(1) == 1 {
			entered <- struct{}{}
			<-gate
			agent.setErr(m, errors.New("simulated render failure"))
			return
		}
		agent.setErr(m, nil)
	})
	return entered, func() { close(gate) }
}

func waitEntered(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never reached the agent")
	}
}

// 1: the agent's verdict on a public root through a link is the field's.
func TestPatchPublicRootRefusedByTheAgent(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "links.example.com")
	agent.setErr(agentrpc.CheckDocRoot.Name(), agentrpc.Invalid("the public root %q passes through a symbolic link", "x"))
	code, a := patchSite(t, c, site.ID, map[string]any{"public_root": "x"})
	if code != http.StatusBadRequest || !strings.Contains(a.Fields["public_root"], "symbolic link") {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	agent.mu.Lock()
	ref, _ := agent.lastParams[agentrpc.CheckDocRoot.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if ref.Site.Config.PublicRoot != "x" {
		t.Fatalf("the agent was asked about %q", ref.Site.Config.PublicRoot)
	}
}

// 3: a put-back compares generations, not values: A sets 256, B 128, C
// 256 again; A's failure must not take C's 256 away.
func TestPutBackIsNotFooledByTheSameValue(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "aba.example.com")
	if code, _ := patchSite(t, c, site.ID, map[string]any{"upload_max_mb": 128}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitForTasks(t, s)
	entered, release := failFirstApply(agent)
	for i, v := range []int{256, 128, 256} {
		if code, a := patchSite(t, c, site.ID, map[string]any{"upload_max_mb": v}); code != http.StatusAccepted {
			t.Fatalf("PATCH %d = %d %+v", v, code, a)
		}
		if i == 0 {
			waitEntered(t, entered)
		}
	}
	release()
	waitForTasks(t, s)
	got, _ := s.Store.GetSite(site.ID)
	if got.Config.PHP.UploadMaxMB != 256 {
		t.Fatalf("upload_max_mb = %d after A failed, want C's 256", got.Config.PHP.UploadMaxMB)
	}
}

// 2: an alias another site took while a change was pending is left off by
// the put-back, which says so; a create cannot take a name in use.
func TestAliasesStayUnique(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	a := createWPSite(t, s, c, "a.example.com")
	if code, _ := patchSite(t, c, a.ID, map[string]any{"aliases": []string{"x.example.com"}}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitForTasks(t, s)
	if _, _, err := s.siteSvc().Create(context.Background(), "t", newSite{Domain: "x.example.com", Type: "static"}); problemOf(t, err).kind != probConflict {
		t.Fatalf("a domain that is another site's alias: %v", err)
	}
	if _, _, err := s.siteSvc().Create(context.Background(), "t", newSite{Domain: "b.example.com", Type: "static", Aliases: []string{"x.example.com"}}); problemOf(t, err).kind != probConflict {
		t.Fatalf("an alias in use: %v", err)
	}

	entered, release := failFirstApply(agent)
	if code, _ := patchSite(t, c, a.ID, map[string]any{"aliases": []string{}}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitEntered(t, entered)
	b, _, err := s.siteSvc().Create(context.Background(), "t", newSite{Domain: "b.example.com", Type: "static", Aliases: []string{"x.example.com"}})
	if err != nil {
		t.Fatalf("x is free once a dropped it: %v", err)
	}
	release()
	waitForTasks(t, s)
	gotA, _ := s.Store.GetSite(a.ID)
	gotB, _ := s.Store.GetSite(b.ID)
	if len(gotA.Aliases) != 0 || len(gotB.Aliases) != 1 {
		t.Fatalf("a = %v, b = %v: the put-back duplicated an alias", gotA.Aliases, gotB.Aliases)
	}
	list, _ := s.Store.ListJobs(20, false)
	noted := false
	for _, j := range list {
		if j.Kind == kindSiteUpdate && j.Status == state.JobFailed {
			full, _ := s.Store.GetJob(j.ID)
			noted = noted || strings.Contains(full.LogHead+full.LogTail, "alias x.example.com is left off")
		}
	}
	if !noted {
		t.Fatal("the task log does not say the alias was left off")
	}
}

// 5: an object-cache job queued by the old binary carried no payload; it
// fails saying so, not silently with the stored value.
func TestLegacyObjectCacheJobWithoutIntentFails(t *testing.T) {
	s, agent, _ := testServer(t)
	site := seedActiveSite(t, s, "legacyoc.example.com", state.SiteWordPress, "8.4", 0)
	j := runJobNow(t, s, jobs.Spec{Kind: kindObjectCache, SiteID: site.ID, Locks: siteLocks(site.ID)})
	if j.Status != state.JobFailed || j.Error != errUnknownIntent.Error() {
		t.Fatalf("job = %s %q", j.Status, j.Error)
	}
	if agent.called(agentrpc.WPObjectCache.Name()) {
		t.Fatal("the agent was asked to switch the object cache")
	}
}

// A change or a delete the queue refuses is undone; staging is refused on
// a site being deleted and on a name another site has.
func TestRefusedWorkLeavesNothingBehind(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "drain.example.com")
	other := seedActiveSite(t, s, "elsewhere.example.com", state.SiteStatic, "", 0)
	if _, err := s.Store.MutateSite(other.ID, func(st *state.Site) error {
		st.Aliases = []string{"staging.drain.example.com"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.stagingSvc().Create(context.Background(), "t", site); problemOf(t, err).kind != probConflict {
		t.Fatalf("staging on a taken name: %v", err)
	}
	if _, err := s.Store.GetSiteByDomain("staging.drain.example.com"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("a staging record was left behind")
	}
	deleting := site
	deleting.Status = state.SiteDeleting
	if _, _, err := s.stagingSvc().Create(context.Background(), "t", deleting); problemOf(t, err).kind != probConflict {
		t.Fatalf("staging of a site being deleted: %v", err)
	}

	before, _ := s.Store.GetSite(site.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.DrainJobs(ctx)
	if code, _ := patchSite(t, c, site.ID, map[string]any{"memory_limit_mb": 512}); code != http.StatusServiceUnavailable {
		t.Fatalf("PATCH while draining = %d", code)
	}
	got, _ := s.Store.GetSite(site.ID)
	if got.Config.PHP.MemoryLimitMB != before.Config.PHP.MemoryLimitMB || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("the refused change stayed: %+v", got)
	}
	// An alias change refused by the queue is undone too (its put-back
	// takes the names lock the change had held).
	if code, _ := patchSite(t, c, site.ID, map[string]any{"aliases": []string{"www.drain.example.com"}}); code != http.StatusServiceUnavailable {
		t.Fatalf("alias PATCH while draining = %d", code)
	}
	if got, _ := s.Store.GetSite(site.ID); len(got.Aliases) != 0 {
		t.Fatalf("aliases = %v after a refused change", got.Aliases)
	}
	if code := c.mustStatus("DELETE", fmt.Sprintf("/api/sites/%d", site.ID)); code != http.StatusServiceUnavailable {
		t.Fatalf("DELETE while draining = %d", code)
	}
	if got, _ := s.Store.GetSite(site.ID); got.Status != state.SiteActive {
		t.Fatalf("status = %s after a refused delete", got.Status)
	}
}

func (c *client) mustStatus(method, path string) int {
	resp, _ := c.do(method, path, nil)
	return resp.StatusCode
}

// PHP workers × memory limit beyond the site's budget: refused, unless an
// administrator says so; never for an operator.
func TestPatchPHPBudget(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createWPSite(t, s, admin, "budget.example.com")
	agent.setResult(agentrpc.SystemStatus.Name(), rpc.SystemStatusResult{MemTotalMB: 2048})
	code, a := patchSite(t, admin, site.ID, map[string]any{"php_workers": 16, "memory_limit_mb": 512})
	if code != http.StatusBadRequest || !strings.Contains(a.Error, "2048 MB") || a.Fields["php_workers"] == "" {
		t.Fatalf("over budget = %d %+v", code, a)
	}
	if code, a = patchSite(t, admin, site.ID, map[string]any{"php_workers": 4, "memory_limit_mb": 512}); code != http.StatusAccepted {
		t.Fatalf("within budget = %d %+v", code, a)
	}
	if code, a = patchSite(t, admin, site.ID, map[string]any{"php_workers": 16, "allow_over_budget": true}); code != http.StatusAccepted {
		t.Fatalf("admin override = %d %+v", code, a)
	}
	waitForTasks(t, s)

	resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"})
	if code, a = patchSite(t, op, site.ID, map[string]any{"memory_limit_mb": 1024, "allow_over_budget": true}); code != http.StatusForbidden {
		t.Fatalf("operator override = %d %+v", code, a)
	}
	if code, a = patchSite(t, op, site.ID, map[string]any{"memory_limit_mb": 1024}); code != http.StatusBadRequest {
		t.Fatalf("operator over budget = %d %+v", code, a)
	}
}
