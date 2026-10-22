package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/guard"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// deployAndWait asks for a deploy and waits for its job; it returns the
// deployment as it ended and the job.
func deployAndWait(t *testing.T, s *Server, c *client, site state.Site) (state.Deployment, state.Job) {
	t.Helper()
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/deployments", site.ID), map[string]string{"source_dir": site.RootPath + "/current"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy = %d %s", resp.StatusCode, body)
	}
	var out struct {
		DeploymentID int64  `json:"deployment_id"`
		ReleaseID    string `json:"release_id"`
	}
	_ = json.Unmarshal(body, &out)
	j := waitTaskEnd(t, s, taskOf(t, body).ID)
	d, err := s.Store.GetDeployment(out.DeploymentID)
	if err != nil || d.ReleaseID != out.ReleaseID {
		t.Fatalf("deployment %d (%q): %+v %v", out.DeploymentID, out.ReleaseID, d, err)
	}
	return d, j
}

// liveTracker makes the fake agent keep a current link: a promote or a
// rollback that names a release makes it the one CurrentRelease answers
// (a refused one does not, unless swapOnError: the agent swapped, then
// failed).
type liveTracker struct {
	agent       *fakeAgent
	swapOnError bool
}

func trackLive(agent *fakeAgent, start string) *liveTracker {
	lt := &liveTracker{agent: agent}
	lt.set(start)
	agent.setHook(func(m string) {
		if m != agentrpc.PromoteRelease.Name() && m != agentrpc.RollbackRelease.Name() {
			return
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if agent.errs[m] != nil && !lt.swapOnError {
			return
		}
		for i := len(agent.history) - 1; i >= 0; i-- {
			if agent.history[i].method == m {
				if p := agent.history[i].params.(rpc.ReleaseParams); p.ReleaseID != "" {
					agent.results[agentrpc.CurrentRelease.Name()] = rpc.CurrentReleaseResult{ReleaseID: p.ReleaseID}
				}
				return
			}
		}
	})
	return lt
}

func (lt *liveTracker) set(release string) {
	lt.agent.mu.Lock()
	lt.agent.results[agentrpc.CurrentRelease.Name()] = rpc.CurrentReleaseResult{ReleaseID: release}
	lt.agent.mu.Unlock()
}

func (lt *liveTracker) live() string {
	lt.agent.mu.Lock()
	defer lt.agent.mu.Unlock()
	r, _ := lt.agent.results[agentrpc.CurrentRelease.Name()].(rpc.CurrentReleaseResult)
	return r.ReleaseID
}

// lastRelease is the release the last call of method named.
func lastRelease(agent *fakeAgent, method string) string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	p, _ := agent.lastParams[method].(rpc.ReleaseParams)
	return p.ReleaseID
}

func rollbackNow(t *testing.T, s *Server, c *client, site state.Site, body any) (int, state.Job) {
	t.Helper()
	resp, b := c.do("POST", fmt.Sprintf("/api/sites/%d/rollback", site.ID), body)
	if resp.StatusCode != http.StatusAccepted {
		return resp.StatusCode, state.Job{}
	}
	return resp.StatusCode, waitTaskEnd(t, s, taskOf(t, b).ID)
}

func status(t *testing.T, s *Server, id int64) state.DeploymentStatus {
	t.Helper()
	d, err := s.Store.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Status
}

// R-4.3: a deploy is recorded when it is asked for and moves pending →
// building → created; a promote makes it the only live release (the one
// before is superseded); a rollback records which release is live again;
// a failed build is failed, and only a built release can be promoted (409
// otherwise, before anything is queued).
func TestDeployStateMachine(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "machine.example.com", state.SiteStatic, "", 0)
	agent.mu.Lock()
	agent.results[agentrpc.DeployRelease.Name()] = rpc.DeployResult{Path: "/srv/sites/machine.example.com/releases/x", Checksum: "sum"}
	agent.mu.Unlock()
	live := trackLive(agent, "initial")

	a, j := deployAndWait(t, s, c, site)
	if j.Status != state.JobSucceeded || a.Status != state.DeployCreated || a.Path == "" || a.Checksum != "sum" {
		t.Fatalf("deploy = %s %q, deployment %+v", j.Status, j.Error, a)
	}
	if len(a.ReleaseID) != 26 || !siteval.ValidReleaseID(a.ReleaseID) {
		t.Fatalf("release id %q is not a ULID", a.ReleaseID)
	}
	b, _ := deployAndWait(t, s, c, site)
	if !(a.ReleaseID < b.ReleaseID) {
		t.Fatalf("release ids do not increase: %s then %s", a.ReleaseID, b.ReleaseID)
	}

	promote := func(d state.Deployment) (int, state.Job) {
		resp, body := c.do("POST", fmt.Sprintf("/api/deployments/%d/promote", d.ID), map[string]any{})
		if resp.StatusCode != http.StatusAccepted {
			return resp.StatusCode, state.Job{}
		}
		return resp.StatusCode, waitTaskEnd(t, s, taskOf(t, body).ID)
	}
	if code, j := promote(a); code != http.StatusAccepted || j.Status != state.JobSucceeded || status(t, s, a.ID) != state.DeployPromoted {
		t.Fatalf("promote a = %d %s %q, now %s", code, j.Status, j.Error, status(t, s, a.ID))
	}
	if code, _ := promote(b); code != http.StatusAccepted || status(t, s, b.ID) != state.DeployPromoted || status(t, s, a.ID) != state.DeploySuperseded {
		t.Fatalf("promote b = %d: a %s, b %s; want a superseded, b promoted", code, status(t, s, a.ID), status(t, s, b.ID))
	}

	// The rollback names its target: the release b replaced.
	if code, j := rollbackNow(t, s, c, site, map[string]any{}); code != http.StatusAccepted || j.Status != state.JobSucceeded {
		t.Fatalf("rollback = %d %s %q", code, j.Status, j.Error)
	}
	if got := lastRelease(agent, agentrpc.RollbackRelease.Name()); got != a.ReleaseID || live.live() != a.ReleaseID {
		t.Fatalf("the agent was asked to go back to %q (live %q), want %s", got, live.live(), a.ReleaseID)
	}
	if status(t, s, b.ID) != state.DeployRolledBack || status(t, s, a.ID) != state.DeployPromoted {
		t.Fatalf("after the rollback: a %s, b %s; want a promoted, b rolled_back", status(t, s, a.ID), status(t, s, b.ID))
	}
	// A rolled-back release can be promoted again.
	if code, j := promote(b); code != http.StatusAccepted || j.Status != state.JobSucceeded || status(t, s, b.ID) != state.DeployPromoted {
		t.Fatalf("promote the rolled-back release = %d %s", code, j.Status)
	}

	agent.mu.Lock()
	agent.errs[agentrpc.DeployRelease.Name()] = errors.New("simulated build failure")
	agent.results[agentrpc.PromoteRelease.Name()] = map[string]string{}
	agent.mu.Unlock()
	f, j := deployAndWait(t, s, c, site)
	if j.Status != state.JobFailed || f.Status != state.DeployFailed {
		t.Fatalf("failed build = %s, deployment %s", j.Status, f.Status)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.DeployRelease.Name())
	agent.mu.Unlock()
	before := countCalls(agent, agentrpc.PromoteRelease.Name())
	if code, _ := promote(f); code != http.StatusConflict {
		t.Fatalf("promote a failed deployment = %d, want 409", code)
	}
	if countCalls(agent, agentrpc.PromoteRelease.Name()) != before {
		t.Fatal("a refused promote reached the agent")
	}

	// A promote the agent refuses leaves the release where it was.
	agent.mu.Lock()
	agent.errs[agentrpc.PromoteRelease.Name()] = agentrpc.NotFound("release pruned")
	agent.mu.Unlock()
	if code, j := promote(a); code != http.StatusAccepted || j.Status != state.JobFailed || status(t, s, a.ID) != state.DeploySuperseded {
		t.Fatalf("refused promote = %d %s, a is %s; want superseded", code, j.Status, status(t, s, a.ID))
	}
	if status(t, s, b.ID) != state.DeployPromoted || live.live() != b.ReleaseID {
		t.Fatalf("the live release is %s (%s live) after a refused promote of another", status(t, s, b.ID), live.live())
	}

	// The agent swapped current and then failed (a PHP reload): the
	// records follow the server, with a drift note (an alert).
	agent.mu.Lock()
	agent.errs[agentrpc.PromoteRelease.Name()] = errors.New("php reload failed")
	live.swapOnError = true
	agent.mu.Unlock()
	if code, j := promote(a); code != http.StatusAccepted || j.Status != state.JobFailed {
		t.Fatalf("promote that failed after the swap = %d %s", code, j.Status)
	}
	if status(t, s, a.ID) != state.DeployPromoted || status(t, s, b.ID) != state.DeploySuperseded {
		t.Fatalf("after a promote that failed after the swap: a %s, b %s; want the records to follow the server", status(t, s, a.ID), status(t, s, b.ID))
	}
	if m, _ := s.Store.GetDeploymentMeta(a.ID); !strings.HasPrefix(m.Note, noteDrift) {
		t.Fatalf("no drift note on the release found live: %+v", m)
	}
	found := false
	for _, al := range evaluatedAlerts(t, s) {
		found = found || al.Kind == "deploy" && strings.Contains(al.Message, "deploy.drift")
	}
	if !found {
		t.Fatal("no deploy.drift alert")
	}
}

// R-4.3: a rollback goes to the release that was live before, never to the
// newest older one the agent would pick (here one built and never
// promoted); a target that was never live is refused with 409.
func TestRollbackIsTargeted(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "target.example.com", state.SiteStatic, "", 0)
	live := trackLive(agent, "initial")
	promote := func(d state.Deployment) {
		resp, body := c.do("POST", fmt.Sprintf("/api/deployments/%d/promote", d.ID), map[string]any{})
		if resp.StatusCode != http.StatusAccepted || waitTaskEnd(t, s, taskOf(t, body).ID).Status != state.JobSucceeded {
			t.Fatalf("promote %s failed", d.ReleaseID)
		}
	}
	a, _ := deployAndWait(t, s, c, site)
	promote(a)
	x, _ := deployAndWait(t, s, c, site) // built, never promoted
	b, _ := deployAndWait(t, s, c, site)
	promote(b)
	if code, _ := rollbackNow(t, s, c, site, map[string]any{"deployment_id": x.ID}); code != http.StatusConflict {
		t.Fatalf("rollback to a release never live = %d, want 409", code)
	}
	if code, j := rollbackNow(t, s, c, site, nil); code != http.StatusAccepted || j.Status != state.JobSucceeded {
		t.Fatalf("rollback = %d %s %q", code, j.Status, j.Error)
	}
	if got := lastRelease(agent, agentrpc.RollbackRelease.Name()); got != a.ReleaseID || live.live() != a.ReleaseID {
		t.Fatalf("rolled back to %q, want %s (not the unpromoted %s)", got, a.ReleaseID, x.ReleaseID)
	}
	// From a, whose predecessor is provisioning's release: back to initial.
	if code, j := rollbackNow(t, s, c, site, nil); code != http.StatusAccepted || j.Status != state.JobSucceeded || live.live() != "initial" {
		t.Fatalf("second rollback = %d %s, live %s", code, j.Status, live.live())
	}
	if status(t, s, a.ID) != state.DeployRolledBack {
		t.Fatalf("a is %s after the rollback from it", status(t, s, a.ID))
	}
	// With nothing earlier recorded: 409, nothing queued.
	before := countCalls(agent, agentrpc.RollbackRelease.Name())
	if code, _ := rollbackNow(t, s, c, site, nil); code != http.StatusConflict || countCalls(agent, agentrpc.RollbackRelease.Name()) != before {
		t.Fatalf("rollback with nothing earlier = %d", code)
	}
	// A chosen earlier release that was live: b.
	if code, j := rollbackNow(t, s, c, site, map[string]any{"deployment_id": b.ID}); code != http.StatusAccepted || j.Status != state.JobSucceeded || live.live() != b.ReleaseID {
		t.Fatalf("rollback to b = %d %s, live %s", code, j.Status, live.live())
	}
}

// R-4.3: a deploy still queued is pending and cannot be promoted; canceled
// before it ran, it is canceled and nothing was built.
func TestQueuedDeployIsPendingAndCancelable(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "queued.example.com", state.SiteStatic, "", 0)
	release := holdLocks(t, s, jobs.SiteLock(site.ID))
	defer release()
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/deployments", site.ID), map[string]string{"source_dir": site.RootPath + "/current"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy = %d %s", resp.StatusCode, body)
	}
	var out struct {
		DeploymentID int64 `json:"deployment_id"`
	}
	_ = json.Unmarshal(body, &out)
	if st := status(t, s, out.DeploymentID); st != state.DeployPending {
		t.Fatalf("a queued deploy is %s, want pending", st)
	}
	if resp, _ := c.do("POST", fmt.Sprintf("/api/deployments/%d/promote", out.DeploymentID), map[string]any{}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("promote a pending deployment = %d, want 409", resp.StatusCode)
	}
	if code, _, b := cancelTask(t, c, taskOf(t, body).ID); code != http.StatusOK {
		t.Fatalf("cancel = %d %s", code, b)
	}
	release()
	waitForTasks(t, s)
	if st := status(t, s, out.DeploymentID); st != state.DeployCanceled {
		t.Fatalf("a deploy canceled while queued is %s", st)
	}
	if agent.called(agentrpc.DeployRelease.Name()) {
		t.Fatal("a canceled deploy was built")
	}
}

// fakeProber stands in for Performance Guard.
type fakeProber struct {
	compare, verify guard.Report
	compares        int
	verifies        int
}

func (f *fakeProber) Compare(context.Context, guard.Endpoint, guard.Endpoint, []string, guard.Thresholds) (guard.Report, error) {
	f.compares++
	return f.compare, nil
}

func (f *fakeProber) Verify(_ context.Context, _ guard.Endpoint, baseline []guard.Sample, _ []string, _ guard.Thresholds) (guard.Report, error) {
	f.verifies++
	if len(baseline) == 0 {
		return guard.Report{}, errors.New("no baseline")
	}
	return f.verify, nil
}

// Measure answers the baseline of a change in place: one sample per path.
func (f *fakeProber) Measure(_ context.Context, _ guard.Endpoint, paths []string) ([]guard.Sample, error) {
	out := make([]guard.Sample, len(paths))
	for i, p := range paths {
		out[i] = guard.Sample{Path: p, Requests: 20, Status: 200}
	}
	return out, nil
}

func useProber(t *testing.T, p prober) {
	old := newProber
	newProber = func(string, int) prober { return p }
	t.Cleanup(func() { newProber = old })
}

func safePushSites(t *testing.T, s *Server) (prod, stg state.Site) {
	t.Helper()
	prod = seedActiveSite(t, s, "push.example.com", state.SiteWordPress, "8.4", 0)
	stg, err := s.Store.CreateSite(state.Site{Domain: "staging.push.example.com", Type: state.SiteWordPress, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-99", RootPath: "/srv/sites/staging.push.example.com", Status: state.SiteActive, StagingOf: prod.ID})
	if err != nil {
		t.Fatal(err)
	}
	return prod, stg
}

func safePushNow(t *testing.T, s *Server, c *client, prod state.Site) (state.Deployment, state.Job) {
	t.Helper()
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/safe-push", prod.ID), map[string]any{"paths": []string{"/"}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("safe push = %d %s", resp.StatusCode, body)
	}
	var out struct {
		DeploymentID int64 `json:"deployment_id"`
	}
	_ = json.Unmarshal(body, &out)
	j := waitTaskEnd(t, s, taskOf(t, body).ID)
	d, _ := s.Store.GetDeployment(out.DeploymentID)
	return d, j
}

var baseline = []guard.Sample{{Path: "/", Requests: 20, Status: 200}}

// R-4.3: a safe push moves pending → guarding → building → promoting →
// verifying → promoted; a block leaves it blocked with nothing built; a
// regression after the promotion rolls it back.
func TestSafePushStateMachine(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	prod, _ := safePushSites(t, s)
	agent.mu.Lock()
	agent.results[agentrpc.DeployRelease.Name()] = rpc.DeployResult{Path: "/srv/sites/push.example.com/releases/x", Checksum: "sum"}
	agent.mu.Unlock()
	p := &fakeProber{compare: guard.Report{Verdict: guard.VerdictPass, Baseline: baseline}, verify: guard.Report{Verdict: guard.VerdictPass}}
	useProber(t, p)
	live := trackLive(agent, "initial")

	d, j := safePushNow(t, s, c, prod)
	if j.Status != state.JobSucceeded || d.Status != state.DeployPromoted || p.verifies != 1 {
		t.Fatalf("passing push = %s %q, deployment %s, %d verifications", j.Status, j.Error, d.Status, p.verifies)
	}
	if !strings.Contains(d.GuardJSON, `"verdict":"pass"`) || d.Path == "" {
		t.Fatalf("the live deployment keeps no report or path: %+v", d)
	}

	// Blocked: nothing is built, production untouched.
	builds := countCalls(agent, agentrpc.DeployRelease.Name())
	p.compare = guard.Report{Verdict: guard.VerdictBlock, Reasons: []string{"slower"}, Baseline: baseline}
	blocked, j := safePushNow(t, s, c, prod)
	if j.Status != state.JobFailed || blocked.Status != state.DeployBlocked || !strings.Contains(blocked.GuardJSON, `"block"`) {
		t.Fatalf("blocked push = %s, deployment %+v", j.Status, blocked)
	}
	if countCalls(agent, agentrpc.DeployRelease.Name()) != builds {
		t.Fatal("a blocked push built a release")
	}
	if status(t, s, d.ID) != state.DeployPromoted {
		t.Fatal("a blocked push changed the live release's record")
	}

	// A release built and never promoted, between the live one and the
	// next push: the automatic rollback must not land on it.
	x, _ := deployAndWait(t, s, c, prod)

	// A regression once live: rolled back to the release that was live
	// before the push, which is live again.
	p.compare = guard.Report{Verdict: guard.VerdictPass, Baseline: baseline}
	p.verify = guard.Report{Verdict: guard.VerdictBlock, Reasons: []string{"new errors"}}
	bad, j := safePushNow(t, s, c, prod)
	if j.Status != state.JobFailed || bad.Status != state.DeployRolledBack {
		t.Fatalf("regressed push = %s %q, deployment %s", j.Status, j.Error, bad.Status)
	}
	if got := lastRelease(agent, agentrpc.RollbackRelease.Name()); got != d.ReleaseID || live.live() != d.ReleaseID {
		t.Fatalf("the automatic rollback went to %q (live %q), want %s, not the unpromoted %s", got, live.live(), d.ReleaseID, x.ReleaseID)
	}
	if status(t, s, d.ID) != state.DeployPromoted {
		t.Fatalf("after the automatic rollback the previous release is %s", status(t, s, d.ID))
	}
	if m, _ := s.Store.GetDeploymentMeta(bad.ID); !m.Regressed {
		t.Fatal("the regressed release is not marked so")
	}
	// It is never a rollback target.
	if code, _ := rollbackNow(t, s, c, prod, map[string]any{"deployment_id": bad.ID}); code != http.StatusConflict {
		t.Fatalf("rollback to a regressed release = %d, want 409", code)
	}
}

// R-4.3: a deployment a restart left behind is settled from what the site
// serves: a safe push whose release went live unverified is verified now;
// one left promoting that did not go live is built, not live; a failed
// build's directory is removed; a drift is recorded. A verification a
// person canceled is recorded live and unverified, with an alert.
func TestReconcileAfterARestart(t *testing.T) {
	s, agent, _ := testServer(t)
	prod, _ := safePushSites(t, s)
	p := &fakeProber{verify: guard.Report{Verdict: guard.VerdictPass}}
	useProber(t, p)
	report, _ := json.Marshal(guard.Report{Verdict: guard.VerdictPass, Baseline: baseline})
	walk := func(to ...state.DeploymentStatus) state.Deployment {
		d, err := s.Store.CreateDeployment(state.Deployment{SiteID: prod.ID, ReleaseID: siteval.NewReleaseID(time.Now()), Status: state.DeployPending})
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range to {
			if d, err = s.Store.MoveDeployment(d.ID, nil, st, state.DeploymentUpdate{GuardJSON: string(report), Path: "/p", Checksum: "c"}); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	failed := walk(state.DeployBuilding, state.DeployFailed)
	stale := walk(state.DeployGuarding, state.DeployBuilding, state.DeployPromoting)
	cand := walk(state.DeployGuarding, state.DeployBuilding, state.DeployPromoting, state.DeployVerifying)
	trackLive(agent, cand.ReleaseID)
	settle := func(d state.Deployment, js state.JobStatus) {
		s.settleDeployment(state.Job{Kind: "deploy.safe-push", SiteID: prod.ID, Status: js,
			Payload: fmt.Sprintf(`{"deployment_id":%d}`, d.ID)})
	}
	settle(cand, state.JobFailed)
	if status(t, s, cand.ID) != state.DeployVerifying {
		t.Fatal("an interrupted verification was settled without one")
	}
	// The next start queues the site's reconcile.
	s.queueDeployReconciles()
	if list, _ := s.Store.ListJobs(20, true); len(list) == 0 || list[0].Kind != "deploy.reconcile" {
		t.Fatalf("no reconcile queued after a restart: %+v", list)
	}
	waitForTasks(t, s)
	p.verifies = 0
	s.flagReconcile(prod.ID)
	for _, to := range []state.DeploymentStatus{state.DeployVerifying} {
		if _, err := s.Store.MoveDeployment(cand.ID, nil, state.DeployPromoting, state.DeploymentUpdate{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.MoveDeployment(cand.ID, nil, to, state.DeploymentUpdate{GuardJSON: string(report)}); err != nil {
			t.Fatal(err)
		}
	}
	j := runJobNow(t, s, jobs.Spec{Kind: "deploy.reconcile", SiteID: prod.ID, Locks: siteLocks(prod.ID), Payload: sitePayload{SiteID: prod.ID}})
	if j.Status != state.JobSucceeded || p.verifies != 1 || status(t, s, cand.ID) != state.DeployPromoted {
		t.Fatalf("reconcile = %s %q: %d verifications, candidate %s", j.Status, j.Error, p.verifies, status(t, s, cand.ID))
	}
	if st := status(t, s, stale.ID); st != state.DeployCreated {
		t.Fatalf("a promote that never went live is %s, want created", st)
	}
	agent.mu.Lock()
	rm, _ := agent.lastParams[agentrpc.RemoveReleases.Name()].(rpc.RemoveReleasesParams)
	agent.mu.Unlock()
	if len(rm.ReleaseIDs) != 1 || rm.ReleaseIDs[0] != failed.ReleaseID {
		t.Fatalf("RemoveReleases = %+v, want the failed build %s", rm, failed.ReleaseID)
	}

	// Verification canceled by a person: live, unverified, an alert.
	canceled := walk(state.DeployGuarding, state.DeployBuilding, state.DeployPromoting, state.DeployVerifying)
	settle(canceled, state.JobCanceled)
	d, _ := s.Store.GetDeployment(canceled.ID)
	m, _ := s.Store.GetDeploymentMeta(canceled.ID)
	if d.Status != state.DeployPromoted || !strings.Contains(d.GuardJSON, `"unverified"`) || m.Note != noteUnverified {
		t.Fatalf("canceled verification = %s %s %+v", d.Status, d.GuardJSON, m)
	}
	found := false
	for _, al := range evaluatedAlerts(t, s) {
		found = found || al.Kind == "deploy" && strings.Contains(al.Message, "without its post-promotion verification")
	}
	if !found {
		t.Fatal("no alert for the unverified release")
	}

	// The records said canceled was live; the server serves cand: drift.
	j = runJobNow(t, s, jobs.Spec{Kind: "deploy.reconcile", SiteID: prod.ID, Locks: siteLocks(prod.ID), Payload: sitePayload{SiteID: prod.ID}})
	if j.Status != state.JobSucceeded || status(t, s, cand.ID) != state.DeployPromoted || status(t, s, canceled.ID) != state.DeploySuperseded {
		t.Fatalf("drift reconcile = %s: cand %s, canceled %s", j.Status, status(t, s, cand.ID), status(t, s, canceled.ID))
	}
	if m, _ := s.Store.GetDeploymentMeta(cand.ID); !strings.HasPrefix(m.Note, noteDrift+":"+canceled.ReleaseID) {
		t.Fatalf("drift note = %q", m.Note)
	}
}

// R-4.3: a safe push the panel restarted during its verification resumes
// there: the release is verified (not built or promoted again) and
// recorded live.
func TestSafePushResumesItsVerification(t *testing.T) {
	s, agent, _ := testServer(t)
	prod, stg := safePushSites(t, s)
	p := &fakeProber{verify: guard.Report{Verdict: guard.VerdictPass}}
	useProber(t, p)
	report, _ := json.Marshal(guard.Report{Verdict: guard.VerdictPass, Baseline: baseline})
	d, err := s.Store.CreateDeployment(state.Deployment{SiteID: prod.ID, ReleaseID: siteval.NewReleaseID(time.Now()), Status: state.DeployPending})
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []state.DeploymentStatus{state.DeployGuarding, state.DeployBuilding, state.DeployPromoting, state.DeployVerifying} {
		if d, err = s.Store.MoveDeployment(d.ID, nil, to, state.DeploymentUpdate{GuardJSON: string(report), Path: "/p", Checksum: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	j := runJobNow(t, s, jobs.Spec{Kind: "deploy.safe-push", SiteID: prod.ID, Locks: siteLocks(prod.ID, stg.ID),
		Payload: deployPayload{DeploymentID: d.ID, StagingID: stg.ID, Paths: []string{"/"}}})
	if j.Status != state.JobSucceeded || status(t, s, d.ID) != state.DeployPromoted || p.verifies != 1 || p.compares != 0 {
		t.Fatalf("resumed push = %s %q, deployment %s, %d compares, %d verifications", j.Status, j.Error, status(t, s, d.ID), p.compares, p.verifies)
	}
	if agent.called(agentrpc.DeployRelease.Name()) || agent.called(agentrpc.PromoteRelease.Name()) {
		t.Fatalf("the resumed push built or promoted again: %v", agent.calls)
	}
}

// R-4.3: the store refuses every transition the machine does not have.
func TestDeploymentTransitionsAreChecked(t *testing.T) {
	s, _, _ := testServer(t)
	site := seedActiveSite(t, s, "moves.example.com", state.SiteStatic, "", 0)
	d, _ := s.Store.CreateDeployment(state.Deployment{SiteID: site.ID, ReleaseID: siteval.NewReleaseID(time.Now()), Status: state.DeployPending})
	_, err := s.Store.MoveDeployment(d.ID, nil, state.DeployPromoted, state.DeploymentUpdate{})
	var se *state.StateError
	if !errors.As(err, &se) || se.From != state.DeployPending || !errors.Is(err, state.ErrDeploymentState) {
		t.Fatalf("pending → promoted = %v", err)
	}
	if _, err := s.Store.MoveDeployment(d.ID, []state.DeploymentStatus{state.DeployCreated}, state.DeployBuilding, state.DeploymentUpdate{}); !errors.Is(err, state.ErrDeploymentState) {
		t.Fatalf("a move from a status the deployment is not in = %v", err)
	}
	if got := status(t, s, d.ID); got != state.DeployPending {
		t.Fatalf("a refused move changed the deployment to %s", got)
	}
}

// evaluatedAlerts runs the alert evaluator and reads the table back.
func evaluatedAlerts(t *testing.T, s *Server) []state.Alert {
	t.Helper()
	_ = s.evaluateAlerts(context.Background()) // the fake agent's services may fail; deploy alerts come from the store
	list, err := s.Store.ListAlerts()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// #11: the periodic reconcile and a rollback to provisioning's release
// both record it when the site has no record of it. Run together, both
// used to find none and insert, and the loser's insert failed on the
// table's unique constraint. Now one record is made, by one of them, and
// no write fails.
func TestInitialReleaseRecordedOnceUnderRace(t *testing.T) {
	s, _, _ := testServer(t)
	var logged syncBuffer
	s.Log = slog.New(slog.NewTextHandler(&logged, nil))
	site := seedActiveSite(t, s, "race.example.com", state.SiteStatic, "", 0)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); <-start; s.syncLive(site, siteval.InitialRelease) }()
		go func() { defer wg.Done(); <-start; s.recordInitialRelease(site.ID, state.DeploySuperseded) }()
	}
	close(start)
	wg.Wait()
	deps, err := s.Store.ListDeployments(site.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].ReleaseID != siteval.InitialRelease || deps[0].Status != state.DeployPromoted {
		t.Fatalf("records after the race: %+v, want one promoted initial", deps)
	}
	if strings.Contains(logged.String(), "state write failed") {
		t.Fatalf("a write failed during the race:\n%s", logged.String())
	}
}
