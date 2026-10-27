package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const pushedCommit = "0123456789abcdef0123456789abcdef01234567"

type gitHookFixture struct {
	s      *Server
	agent  *fakeAgent
	ts     *httptest.Server
	admin  *client
	site   state.Site
	view   gitDeployView
	secret string
}

// gitHookSite is a static site with git deploy on (branch main), a fake
// agent that fetches commit pushedCommit into the site's export and builds a
// release, and its live release tracked.
func gitHookSite(t *testing.T) (*gitHookFixture, *liveTracker) {
	t.Helper()
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "hook.example.com", state.SiteStatic, "", 0)
	resp, body := admin.do("PUT", "/api/sites/"+itoa(site.ID)+"/git-deploy", map[string]any{"enabled": true, "repo": "https://example.com/acme/shop.git", "build_command": "make"})
	if resp.StatusCode != 200 {
		t.Fatalf("turn on: %d %s", resp.StatusCode, body)
	}
	v := gitDeployOf(t, body)
	agent.mu.Lock()
	agent.results[agentrpc.GitSync.Name()] = rpc.GitSyncResult{Commit: pushedCommit, SourceDir: site.RootPath + "/tmp/cg-git/export/x"}
	agent.results[agentrpc.DeployRelease.Name()] = rpc.DeployResult{Path: site.RootPath + "/releases/x", Checksum: "sum"}
	agent.mu.Unlock()
	return &gitHookFixture{s: s, agent: agent, ts: ts, admin: admin, site: site, view: v, secret: v.Secret}, trackLive(agent, "initial")
}

func hubSig(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// deliver posts a webhook; hdr are name, value pairs.
func (f *gitHookFixture) deliver(t *testing.T, path string, body []byte, hdr ...string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", f.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func (f *gitHookFixture) github(t *testing.T, event string, body []byte) (int, map[string]any) {
	t.Helper()
	code, out, _ := f.deliver(t, f.view.WebhookPath, body, "X-GitHub-Event", event, "X-Hub-Signature-256", "sha256="+hubSig(f.secret, body))
	return code, out
}

func pushBody(ref string) []byte {
	b, _ := json.Marshal(map[string]any{"ref": ref, "after": pushedCommit})
	return b
}

func (f *gitHookFixture) waitDeployment(t *testing.T, out map[string]any) state.Deployment {
	t.Helper()
	id, _ := out["deployment_id"].(float64)
	task, _ := out["task"].(float64)
	if id == 0 || task == 0 {
		t.Fatalf("no deployment or task in %v", out)
	}
	waitTaskEnd(t, f.s, int64(task))
	d, err := f.s.Store.GetDeployment(int64(id))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// §15.3: every forge's signature is accepted; anything else, whatever is
// wrong with it, is the same 401 as a hook that does not exist or is off.
func TestGitHookSignatures(t *testing.T) {
	f, _ := gitHookSite(t)
	body := pushBody("refs/heads/other") // ignored: only the signature is tested
	good := hubSig(f.secret, body)
	for name, hdr := range map[string][]string{
		"github":                             {"X-GitHub-Event", "push", "X-Hub-Signature-256", "sha256=" + good},
		"gitea":                              {"X-Gitea-Event", "push", "X-Gitea-Signature", good},
		"forgejo":                            {"X-Forgejo-Event", "push", "X-Forgejo-Signature", good},
		"gitlab":                             {"X-Gitlab-Event", "Push Hook", "X-Gitlab-Token", f.secret},
		"gitea sending github's headers too": {"X-Gitea-Event", "push", "X-GitHub-Event", "push", "X-Hub-Signature-256", "sha256=" + good},
	} {
		code, out, raw := f.deliver(t, f.view.WebhookPath, body, hdr...)
		if code != 200 || out["status"] != "ignored" || out["reason"] != "branch" {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	forges := map[string]bool{}
	ds, _ := f.s.Store.GitDeliveries(f.site.ID)
	for _, d := range ds {
		forges[d.Forge] = true
	}
	for _, want := range []string{"github", "gitea", "forgejo", "gitlab"} {
		if !forges[want] {
			t.Errorf("no delivery from %s: %v", want, forges)
		}
	}

	wrong := hubSig("another secret", body)
	flipped := []byte(good)
	flipped[0] ^= 1 // a hex digit changed
	cases := map[string][]string{
		"none":               {"X-GitHub-Event", "push"},
		"wrong hmac":         {"X-Hub-Signature-256", "sha256=" + wrong},
		"no prefix":          {"X-Hub-Signature-256", good},
		"sha1 only":          {"X-Hub-Signature", "sha1=" + good},
		"not hex":            {"X-Hub-Signature-256", "sha256=zz"},
		"truncated":          {"X-Hub-Signature-256", "sha256=" + good[:32]},
		"changed digit":      {"X-Gitea-Signature", string(flipped)},
		"gitlab wrong token": {"X-Gitlab-Token", "nope"},
		"gitlab with hmac":   {"X-Gitlab-Token", good},
		"empty token":        {"X-Gitlab-Token", ""},
	}
	unknown := map[string]string{}
	for name, hdr := range cases {
		code, _, raw := f.deliver(t, f.view.WebhookPath, body, hdr...)
		if code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", name, code, raw)
		}
		unknown[name] = raw
		// The same request to a hook that does not exist answers the same.
		code2, _, raw2 := f.deliver(t, "/hooks/git/"+strings.Repeat("a", 24), body, hdr...)
		if code2 != code || raw2 != raw {
			t.Errorf("%s: unknown hook answers %d %s, a known one %d %s", name, code2, raw2, code, raw)
		}
	}
	// The failures so far fill the minute's window (TestGitHookRateLimit
	// is about that).
	f.s.gitHookFailLimiter.Reset("127.0.0.1")
	// A body changed after signing.
	code, _, _ := f.deliver(t, f.view.WebhookPath, append(body[:len(body):len(body)], ' '), "X-Hub-Signature-256", "sha256="+good)
	if code != http.StatusUnauthorized {
		t.Errorf("a changed body: %d", code)
	}
	// A valid signature for a hook that does not exist, and for one turned off.
	ghost := "/hooks/git/" + strings.Repeat("b", 24)
	if code, _, raw := f.deliver(t, ghost, body, "X-Hub-Signature-256", "sha256="+good); code != 401 || raw != unknown["none"] {
		t.Errorf("a signed request to nobody: %d %s", code, raw)
	}
	if resp, _ := f.admin.do("PUT", "/api/sites/"+itoa(f.site.ID)+"/git-deploy", map[string]any{"enabled": false}); resp.StatusCode != 200 {
		t.Fatal("turn off")
	}
	if code, _, raw := f.deliver(t, f.view.WebhookPath, body, "X-Hub-Signature-256", "sha256="+good); code != 401 || raw != unknown["none"] {
		t.Errorf("a signed request to a hook that is off: %d %s", code, raw)
	}
	// A refusal leaves nothing in the deliveries or the audit log.
	if after, _ := f.s.Store.GitDeliveries(f.site.ID); len(after) != len(ds) {
		t.Errorf("refused requests were recorded: %d deliveries, had %d", len(after), len(ds))
	}
	// The old URL after the secret is regenerated.
	f.admin.do("PUT", "/api/sites/"+itoa(f.site.ID)+"/git-deploy", map[string]any{"enabled": true})
	resp, nb := f.admin.do("POST", "/api/sites/"+itoa(f.site.ID)+"/git-deploy/secret", nil)
	if resp.StatusCode != 200 {
		t.Fatal("rotate")
	}
	fresh := gitDeployOf(t, nb)
	if code, _, _ := f.deliver(t, f.view.WebhookPath, body, "X-Hub-Signature-256", "sha256="+good); code != 401 {
		t.Errorf("the old URL answers %d", code)
	}
	if code, _, _ := f.deliver(t, fresh.WebhookPath, body, "X-Hub-Signature-256", "sha256="+hubSig(f.secret, body)); code != 401 {
		t.Errorf("the old secret on the new URL answers %d", code)
	}
	if code, out, _ := f.deliver(t, fresh.WebhookPath, body, "X-GitHub-Event", "push", "X-Hub-Signature-256", "sha256="+hubSig(fresh.Secret, body)); code != 200 || out["reason"] != "branch" {
		t.Errorf("the new URL and secret: %d %v", code, out)
	}
}

// 20 failures a minute from one address are answered 429, even a valid
// delivery after them; another address is not affected.
func TestGitHookRateLimit(t *testing.T) {
	f, _ := gitHookSite(t)
	body := pushBody("refs/heads/main")
	for i := 0; i < gitHookFailures; i++ {
		if code, _, _ := f.deliver(t, f.view.WebhookPath, body, "X-Hub-Signature-256", "sha256=00"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	if code, _, _ := f.deliver(t, f.view.WebhookPath, body, "X-Hub-Signature-256", "sha256=00"); code != http.StatusTooManyRequests {
		t.Fatalf("the failure after the limit: %d, want 429", code)
	}
	if code, _ := f.github(t, "push", body); code != http.StatusTooManyRequests {
		t.Fatalf("a valid delivery from the blocked address: %d, want 429", code)
	}
	if f.agent.called(agentrpc.GitSync.Name()) {
		t.Fatal("a limited delivery reached the agent")
	}
	f.s.gitHookFailLimiter.Reset(clientNetwork("127.0.0.1"))
	if code, _ := f.github(t, "push", pushBody("refs/heads/other")); code != 200 {
		t.Fatalf("after the window: %d", code)
	}
}

// §15.3 steps 4 and 5: a ping is answered; what is not a push of the
// branch is ignored, and nothing is built.
func TestGitHookEvents(t *testing.T) {
	f, _ := gitHookSite(t)
	if code, out := f.github(t, "ping", []byte(`{"zen":"x"}`)); code != 200 || out["status"] != "pong" {
		t.Fatalf("ping: %d %v", code, out)
	}
	for name, c := range map[string]struct {
		event  string
		body   string
		reason string
	}{
		"another event": {"issues", `{"ref":"refs/heads/main"}`, "event"},
		"a tag":         {"push", `{"ref":"refs/tags/v1","after":"` + pushedCommit + `"}`, "branch"},
		"another":       {"push", `{"ref":"refs/heads/develop","after":"` + pushedCommit + `"}`, "branch"},
		"a prefix":      {"push", `{"ref":"refs/heads/main2","after":"` + pushedCommit + `"}`, "branch"},
		"deleted":       {"push", `{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`, "deleted"},
		"deleted flag":  {"push", `{"ref":"refs/heads/main","after":"` + pushedCommit + `","deleted":true}`, "deleted"},
		"not json":      {"push", `ref=refs/heads/main`, "payload"},
		"no ref":        {"push", `{"after":"` + pushedCommit + `"}`, "payload"},
	} {
		code, out := f.github(t, c.event, []byte(c.body))
		if code != 200 || out["status"] != "ignored" || out["reason"] != c.reason {
			t.Errorf("%s: %d %v, want ignored for %s", name, code, out, c.reason)
		}
	}
	code, out, _ := f.deliver(t, f.view.WebhookPath, []byte(`{}`), "X-Gitlab-Event", "Tag Push Hook", "X-Gitlab-Token", f.secret)
	if code != 200 || out["reason"] != "event" {
		t.Errorf("a GitLab tag push: %d %v", code, out)
	}
	if f.agent.called(agentrpc.GitSync.Name()) {
		t.Fatal("an ignored delivery was built")
	}
	if n := countDeployments(t, f.s, f.site.ID); n != 0 {
		t.Fatalf("%d deployments for deliveries that built nothing", n)
	}
}

func countDeployments(t *testing.T, s *Server, siteID int64) int {
	t.Helper()
	ds, err := s.Store.ListDeployments(siteID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(ds)
}

// §15.4: a push of the branch fetches as the site user, builds a release
// from the export and promotes it, in that order, with the rollback of §4.
func TestGitHookDeploys(t *testing.T) {
	f, live := gitHookSite(t)
	code, out := f.github(t, "push", pushBody("refs/heads/main"))
	if code != http.StatusAccepted || out["status"] != "queued" || out["coalesced"] != nil {
		t.Fatalf("push: %d %v", code, out)
	}
	d := f.waitDeployment(t, out)
	if d.Status != state.DeployPromoted || live.live() != d.ReleaseID {
		t.Fatalf("deployment %s, live %q, want %s promoted", d.Status, live.live(), d.ReleaseID)
	}
	var order []string
	f.agent.mu.Lock()
	for _, h := range f.agent.history {
		switch h.method {
		case agentrpc.GitSync.Name(), agentrpc.DeployRelease.Name(), agentrpc.PromoteRelease.Name():
			order = append(order, h.method)
		}
	}
	sync, _ := f.agent.lastParams[agentrpc.GitSync.Name()].(rpc.GitSyncParams)
	dep, _ := f.agent.lastParams[agentrpc.DeployRelease.Name()].(rpc.DeployParams)
	f.agent.mu.Unlock()
	if strings.Join(order, ",") != "GitSync,DeployRelease,PromoteRelease" {
		t.Fatalf("the agent was called %v", order)
	}
	if sync.Repo != "https://example.com/acme/shop.git" || sync.Branch != "main" || sync.BuildCommand != "make" || sync.ReleaseID != d.ReleaseID ||
		!strings.HasPrefix(sync.DeployKey, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("GitSync params %+v", sync)
	}
	if dep.SourceDir != f.site.RootPath+"/tmp/cg-git/export/x" || dep.ReleaseID != d.ReleaseID {
		t.Fatalf("DeployRelease params %+v", dep)
	}
	// The job's payload holds no secret.
	jobs, _ := f.s.Store.ListJobs(10, true)
	for _, j := range jobs {
		if strings.Contains(j.Payload, "BEGIN") || strings.Contains(j.Payload, f.secret) {
			t.Fatalf("payload of %s holds a secret: %s", j.Kind, j.Payload)
		}
	}
	ds, _ := f.s.Store.GitDeliveries(f.site.ID)
	if len(ds) != 1 || ds[0].Outcome != state.GitQueued || ds[0].Commit != pushedCommit || ds[0].DeploymentID != d.ID || ds[0].Forge != "github" {
		t.Fatalf("deliveries %+v", ds)
	}
	var audited bool
	events, _ := f.s.Store.ListAuditEvents(20)
	for _, e := range events {
		if e.Action == "git.deploy" && e.Actor == "git webhook (github)" && e.Subject == f.site.Domain && strings.Contains(e.Detail, "main 0123456") {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("no audit entry: %+v", events)
	}

	// A second push: a new release, the first becomes superseded, and the
	// existing rollback goes back to it.
	code, out = f.github(t, "push", pushBody("refs/heads/main"))
	d2 := f.waitDeployment(t, out)
	if code != http.StatusAccepted || d2.Status != state.DeployPromoted || status(t, f.s, d.ID) != state.DeploySuperseded {
		t.Fatalf("second push: %d %s, first %s", code, d2.Status, status(t, f.s, d.ID))
	}
	if rc, j := rollbackNow(t, f.s, f.admin, f.site, map[string]any{}); rc != http.StatusAccepted || j.Status != state.JobSucceeded || live.live() != d.ReleaseID {
		t.Fatalf("rollback %d %s, live %q, want %s", rc, j.Status, live.live(), d.ReleaseID)
	}
}

// A fetch or a build that fails leaves the live release live and the
// deployment failed, with the output in the task's log.
func TestGitHookFailureKeepsTheLiveRelease(t *testing.T) {
	for _, failed := range []string{"fetch", "build"} {
		t.Run(failed, func(t *testing.T) {
			f, live := gitHookSite(t)
			code, out := f.github(t, "push", pushBody("refs/heads/main"))
			d := f.waitDeployment(t, out)
			if code != http.StatusAccepted || d.Status != state.DeployPromoted {
				t.Fatalf("the first push: %d %s", code, d.Status)
			}
			was := live.live()
			f.agent.mu.Lock()
			f.agent.results[agentrpc.GitSync.Name()] = rpc.GitSyncResult{Failed: failed, Output: "ERR! the " + failed + " broke"}
			f.agent.mu.Unlock()
			before := countCalls(f.agent, agentrpc.DeployRelease.Name())
			_, out = f.github(t, "push", pushBody("refs/heads/main"))
			bad := f.waitDeployment(t, out)
			if bad.Status != state.DeployFailed || live.live() != was || status(t, f.s, d.ID) != state.DeployPromoted {
				t.Fatalf("after a failed %s: deployment %s, live %q (was %q), first %s", failed, bad.Status, live.live(), was, status(t, f.s, d.ID))
			}
			if countCalls(f.agent, agentrpc.DeployRelease.Name()) != before || countCalls(f.agent, agentrpc.PromoteRelease.Name()) != 1 {
				t.Fatal("a failed step went on to build or promote")
			}
			task, _ := out["task"].(float64)
			j := waitTaskEnd(t, f.s, int64(task))
			if j.Status != state.JobFailed || !strings.Contains(j.LogHead+j.LogTail, "ERR! the "+failed+" broke") || !strings.Contains(j.Error, "live release was not changed") {
				t.Fatalf("job %s %q\n%s%s", j.Status, j.Error, j.LogHead, j.LogTail)
			}
		})
	}
}

// §15.4: deploys of a site run one at a time and at most one waits: a push
// while one runs queues one more, a third joins it.
func TestGitHookQueuesAtMostOneMore(t *testing.T) {
	f, _ := gitHookSite(t)
	var mu sync.Mutex
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	running, peak, syncs := 0, 0, 0
	f.agent.setHook(func(m string) {
		if m != agentrpc.GitSync.Name() {
			return
		}
		mu.Lock()
		running++
		syncs++
		peak = max(peak, running)
		mu.Unlock()
		started <- struct{}{}
		<-release
		mu.Lock()
		running--
		mu.Unlock()
	})
	body := pushBody("refs/heads/main")
	_, first := f.github(t, "push", body)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first deploy never started")
	}
	code, second := f.github(t, "push", body)
	if code != http.StatusAccepted || second["coalesced"] != nil || second["deployment_id"] == first["deployment_id"] {
		t.Fatalf("a push while one runs: %d %v", code, second)
	}
	code, third := f.github(t, "push", body)
	if code != http.StatusAccepted || third["coalesced"] != true || third["deployment_id"] != second["deployment_id"] {
		t.Fatalf("a push while one waits: %d %v, want it to join %v", code, third, second["deployment_id"])
	}
	_, fourth := f.github(t, "push", body)
	if fourth["coalesced"] != true || fourth["deployment_id"] != second["deployment_id"] {
		t.Fatalf("a fourth push: %v", fourth)
	}
	close(release)
	f.waitDeployment(t, first)
	d2 := f.waitDeployment(t, second)
	mu.Lock()
	defer mu.Unlock()
	if syncs != 2 || peak != 1 || d2.Status != state.DeployPromoted {
		t.Fatalf("%d fetches, %d at once, second %s: want 2 one after the other", syncs, peak, d2.Status)
	}
	if n := countDeployments(t, f.s, f.site.ID); n != 2 {
		t.Fatalf("%d deployments for four pushes, want 2", n)
	}
	ds, _ := f.s.Store.GitDeliveries(f.site.ID)
	got := map[string]int{}
	for _, d := range ds {
		got[d.Outcome]++
	}
	if got[state.GitQueued] != 2 || got[state.GitCoalesced] != 2 {
		t.Fatalf("deliveries %v", got)
	}
	// With nothing waiting a later push queues again.
	f.agent.setHook(nil)
	_, later := f.github(t, "push", body)
	if later["coalesced"] != nil {
		t.Fatalf("a push after both ran: %v", later)
	}
	f.waitDeployment(t, later)
}

// "Deploy now" is a push without the forge, and a git deploy that is off
// cannot be run.
func TestGitDeployRun(t *testing.T) {
	f, live := gitHookSite(t)
	resp, body := f.admin.do("POST", "/api/sites/"+itoa(f.site.ID)+"/git-deploy/run", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run: %d %s", resp.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	task, _ := out["task"].(map[string]any)
	out["task"] = task["id"]
	d := f.waitDeployment(t, out)
	if d.Status != state.DeployPromoted || live.live() != d.ReleaseID {
		t.Fatalf("%s, live %q", d.Status, live.live())
	}
	ds, _ := f.s.Store.GitDeliveries(f.site.ID)
	if len(ds) != 1 || ds[0].Forge != "manual" || ds[0].Commit != pushedCommit {
		t.Fatalf("deliveries %+v", ds)
	}
	f.admin.do("PUT", "/api/sites/"+itoa(f.site.ID)+"/git-deploy", map[string]any{"enabled": false})
	if resp, _ := f.admin.do("POST", "/api/sites/"+itoa(f.site.ID)+"/git-deploy/run", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("run while off: %d, want 409", resp.StatusCode)
	}
}

// An agent that cannot be reached fails the deployment before anything is
// built, the live release stays.
func TestGitHookAgentError(t *testing.T) {
	f, live := gitHookSite(t)
	f.agent.mu.Lock()
	f.agent.errs[agentrpc.GitSync.Name()] = errors.New("agent down")
	f.agent.mu.Unlock()
	_, out := f.github(t, "push", pushBody("refs/heads/main"))
	d := f.waitDeployment(t, out)
	if d.Status != state.DeployFailed || live.live() != "initial" || f.agent.called(agentrpc.DeployRelease.Name()) {
		t.Fatalf("%s live %q", d.Status, live.live())
	}
}
