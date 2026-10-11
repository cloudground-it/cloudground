package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// patchAnswer is the body of a PATCH, or of a refusal.
type patchAnswer struct {
	Site     state.Site            `json:"site"`
	Task     struct{ Kind string } `json:"task"`
	Error    string                `json:"error"`
	Fields   map[string]string     `json:"fields"`
	Warnings []string              `json:"warnings"`
}

func patchSite(t *testing.T, c *client, id int64, body any) (int, patchAnswer) {
	t.Helper()
	resp, raw := c.do("PATCH", fmt.Sprintf("/api/sites/%d", id), body)
	var a patchAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("PATCH answer %d %s: %v", resp.StatusCode, raw, err)
	}
	return resp.StatusCode, a
}

// countCalls is how many times the fake agent was called with method.
func countCalls(agent *fakeAgent, method string) int {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	n := 0
	for _, c := range agent.calls {
		if c == method {
			n++
		}
	}
	return n
}

// R-4.3: one PATCH changes settings of several former routes at once, in
// one job, and the record says so.
func TestPatchChangesSettingsInOneJob(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "patch.example.com")
	renders := countCalls(agent, agentrpc.ApplySiteConfig.Name())

	code, a := patchSite(t, c, site.ID, map[string]any{
		"aliases": []string{" WWW.patch.example.com "}, "profile": "maximum", "cache_ttl_sec": 900,
		"php_version": "8.3", "memory_limit_mb": 512, "max_execution_seconds": 300, "public_root": "/web/",
	})
	if code != http.StatusAccepted || a.Task.Kind != kindSiteUpdate {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	waitForTasks(t, s)
	got, _ := s.Store.GetSite(site.ID)
	if !slices.Equal(got.Aliases, []string{"www.patch.example.com"}) || got.Profile != state.ProfileMaximum ||
		got.Config.CacheTTLSec != 900 || got.PHPVersion != "8.3" || got.Config.PHP.MemoryLimitMB != 512 ||
		got.Config.PHP.MaxExecutionSeconds != 300 || got.Config.PublicRoot != "web" {
		t.Fatalf("stored site = %+v", got)
	}
	if n := countCalls(agent, agentrpc.ApplySiteConfig.Name()) - renders; n != 1 {
		t.Fatalf("%d renders for one patch, want 1", n)
	}
	if got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("applied %d, desired %d", got.AppliedGeneration, got.DesiredGeneration)
	}
	events, _ := s.Store.ListAuditEvents(5)
	if len(events) == 0 || events[0].Action != "site.update" ||
		events[0].Detail != "aliases,profile,cache_ttl_sec,php_version,memory_limit_mb,max_execution_seconds,public_root" {
		t.Fatalf("audit = %+v", events)
	}
}

// R-4.3: every field is checked, and the answer names each one wrong.
func TestPatchRefusesFieldByField(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "fields.example.com")
	other := seedActiveSite(t, s, "other.example.com", state.SiteStatic, "", 0)

	code, a := patchSite(t, c, site.ID, map[string]any{
		"profile": "turbo", "memory_limit_mb": 1, "php_version": "7.4", "cache_ttl_sec": -1,
		"proxy_upstream": "http://127.0.0.1:9000", "aliases": []string{"other.example.com"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	want := map[string]string{
		"aliases":         "alias other.example.com is already used by other.example.com",
		"profile":         "invalid profile",
		"cache_ttl_sec":   a.Fields["cache_ttl_sec"],
		"php_version":     "unsupported PHP version",
		"memory_limit_mb": "memory limit must be 64–4096 MB",
		"proxy_upstream":  "only a proxy site has an upstream",
	}
	for k, v := range want {
		if a.Fields[k] != v || v == "" {
			t.Errorf("fields[%s] = %q, want %q", k, a.Fields[k], v)
		}
	}
	if len(a.Fields) != len(want) || a.Error != want["aliases"] {
		t.Errorf("answer = %+v", a)
	}
	got, _ := s.Store.GetSite(site.ID)
	if got.DesiredGeneration != site.DesiredGeneration {
		t.Fatal("a refused patch changed the site")
	}

	// PHP settings on a site without PHP; the object cache off WordPress;
	// a public root on a proxy.
	code, a = patchSite(t, c, other.ID, map[string]any{"upload_max_mb": 10, "object_cache": true})
	if code != http.StatusBadRequest || a.Fields["upload_max_mb"] != msgNoPHP || a.Fields["object_cache"] != msgNotWordPress {
		t.Fatalf("static site = %d %+v", code, a)
	}
	proxy := seedActiveSite(t, s, "proxy.example.com", state.SiteProxy, "", 0)
	if code, a = patchSite(t, c, proxy.ID, map[string]any{"public_root": "public"}); code != http.StatusBadRequest || a.Fields["public_root"] == "" {
		t.Fatalf("proxy public root = %d %+v", code, a)
	}
	if code, a = patchSite(t, c, proxy.ID, map[string]any{"proxy_upstream": "http://127.0.0.1:8080/app"}); code != http.StatusAccepted {
		t.Fatalf("proxy upstream = %d %+v", code, a)
	}
	waitForTasks(t, s)
	if got, _ := s.Store.GetSite(proxy.ID); got.Config.ProxyUpstream != "http://127.0.0.1:8080/app" {
		t.Fatalf("upstream = %q", got.Config.ProxyUpstream)
	}

	// Nothing named, an unknown field, a duplicate alias, the own domain.
	if code, a = patchSite(t, c, site.ID, map[string]any{}); code != http.StatusBadRequest || a.Error != "name at least one setting to change" {
		t.Fatalf("empty patch = %d %+v", code, a)
	}
	if code, _ = patchSite(t, c, site.ID, map[string]any{"domain": "x.example.com"}); code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", code)
	}
	for _, aliases := range [][]string{{"a.example.com", "A.example.com"}, {"fields.example.com"}, {"not a host"}} {
		if code, a = patchSite(t, c, site.ID, map[string]any{"aliases": aliases}); code != http.StatusBadRequest || a.Fields["aliases"] == "" {
			t.Fatalf("aliases %v = %d %+v", aliases, code, a)
		}
	}
}

// R-4.3: hosts a site answers for and where a proxy sends its traffic are
// an administrator's, as creating the site is; an operator gets 403.
func TestPatchAdminOnlyFields(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "mine.example.com", state.SiteProxy, "", 0)
	resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	if resp, body := op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %s", resp.StatusCode, body)
	}
	code, a := patchSite(t, op, site.ID, map[string]any{"aliases": []string{"www.mine.example.com"}, "proxy_upstream": "http://127.0.0.1:1", "profile": "maximum"})
	if code != http.StatusForbidden || a.Error != msgAdminFields || len(a.Fields) != 2 {
		t.Fatalf("operator = %d %+v", code, a)
	}
	if got, _ := s.Store.GetSite(site.ID); got.Profile != state.ProfileBalanced {
		t.Fatal("a refused patch changed the profile")
	}
	if code, a = patchSite(t, op, site.ID, map[string]any{"profile": "maximum"}); code != http.StatusAccepted {
		t.Fatalf("operator profile = %d %+v", code, a)
	}
}

// R-4.3: the object cache alone is the agent's switch, not a render, and
// the site is applied afterwards (the old toggle left a pending change).
func TestPatchObjectCacheAlone(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "oc.example.com")
	renders := countCalls(agent, agentrpc.ApplySiteConfig.Name())
	if code, a := patchSite(t, c, site.ID, map[string]any{"object_cache": true}); code != http.StatusAccepted {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	waitForTasks(t, s)
	if countCalls(agent, agentrpc.ApplySiteConfig.Name()) != renders || !agent.called(agentrpc.WPObjectCache.Name()) {
		t.Fatal("want the object-cache call and no render")
	}
	got, _ := s.Store.GetSite(site.ID)
	if !got.Config.ObjectCache || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("site = object_cache %v, applied %d desired %d", got.Config.ObjectCache, got.AppliedGeneration, got.DesiredGeneration)
	}

	// With another field, both steps run; an agent that refuses the
	// object cache puts that field back and keeps the rendered one.
	agent.setErr(agentrpc.WPObjectCache.Name(), fmt.Errorf("simulated drop-in failure"))
	if code, a := patchSite(t, c, site.ID, map[string]any{"object_cache": false, "cache_ttl_sec": 120}); code != http.StatusAccepted {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	waitForTasks(t, s)
	got, _ = s.Store.GetSite(site.ID)
	// R-4.3 review: the put-back restores what was rendered, so the site is
	// not left pending; the failure is the task's.
	if !got.Config.ObjectCache || got.Config.CacheTTLSec != 120 || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("after a failed switch: object_cache %v, ttl %d, applied %d desired %d",
			got.Config.ObjectCache, got.Config.CacheTTLSec, got.AppliedGeneration, got.DesiredGeneration)
	}

	// The toggle alone, failing: the site ends where it was, applied.
	if code, _ := patchSite(t, c, site.ID, map[string]any{"object_cache": false}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitForTasks(t, s)
	got, _ = s.Store.GetSite(site.ID)
	if !got.Config.ObjectCache || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("after a failed toggle: object_cache %v, applied %d desired %d", got.Config.ObjectCache, got.AppliedGeneration, got.DesiredGeneration)
	}
}

// R-4.3: a render that fails puts back every field of the patch, the new
// ones included.
func TestPatchFailedRenderPutsBack(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "back.example.com")
	agent.setErr(agentrpc.ApplySiteConfig.Name(), fmt.Errorf("simulated render failure"))
	code, _ := patchSite(t, c, site.ID, map[string]any{"aliases": []string{"www.back.example.com"}, "public_root": "web", "upload_max_mb": 99})
	if code != http.StatusAccepted {
		t.Fatalf("PATCH = %d", code)
	}
	waitForTasks(t, s)
	got, _ := s.Store.GetSite(site.ID)
	if len(got.Aliases) != 0 || got.Config.PublicRoot != "" || got.Config.PHP.UploadMaxMB != site.Config.PHP.UploadMaxMB {
		t.Fatalf("not put back: %+v", got)
	}
}

// R-8.2: the routes PATCH replaced are gone. Each answers 404 as an
// unknown API path, changes nothing and queues nothing, also with the UI
// embedded (which used to answer any unknown path with its page).
func TestReplacedSettingsRoutesAreGone(t *testing.T) {
	s, agent, ts := testServer(t)
	s.UI = fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>ui</title>")}}
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "old.example.com")
	waitForTasks(t, s)
	before, _ := s.Store.GetSite(site.ID)
	renders := countCalls(agent, agentrpc.ApplySiteConfig.Name())
	for _, rq := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/config", map[string]any{"cache_ttl_sec": 77}},
		{"PUT", "/php", map[string]any{"upload_max_mb": 128}},
		{"POST", "/wp/object-cache", map[string]bool{"enable": false}},
	} {
		path := fmt.Sprintf("/api/sites/%d%s", site.ID, rq.path)
		resp, body := c.do(rq.method, path, rq.body)
		if resp.StatusCode != http.StatusNotFound || !jsonHas(body, "error", "no such API route: "+rq.method+" "+path) {
			t.Fatalf("%s %s = %d %s", rq.method, path, resp.StatusCode, body)
		}
	}
	waitForTasks(t, s)
	after, _ := s.Store.GetSite(site.ID)
	if after.DesiredGeneration != before.DesiredGeneration || after.Config.CacheTTLSec != before.Config.CacheTTLSec ||
		after.Config.PHP.UploadMaxMB != before.Config.PHP.UploadMaxMB || after.Config.ObjectCache != before.Config.ObjectCache {
		t.Fatalf("a removed route changed the site: %+v", after.Config)
	}
	if countCalls(agent, agentrpc.ApplySiteConfig.Name()) != renders {
		t.Fatal("a removed route queued a render")
	}
	// The app is still served for its own paths.
	resp, body := c.do("GET", "/sites/1/php", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>ui</title>") {
		t.Fatalf("GET /sites/1/php = %d %s", resp.StatusCode, body)
	}
}

// R-8.2: a job the removed routes queued before an upgrade still runs its
// payload, under each of their kinds.
func TestReplacedRoutesQueuedJobsStillRun(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "queued.example.com")
	waitForTasks(t, s)
	for _, kind := range []string{kindApplyConfig, kindApplyPHP, kindObjectCache} {
		cur, _ := s.Store.GetSite(site.ID)
		next, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
			st.Config.PHP.UploadMaxMB++
			st.Config.ObjectCache = !st.Config.ObjectCache
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		renders, switches := countCalls(agent, agentrpc.ApplySiteConfig.Name()), countCalls(agent, agentrpc.WPObjectCache.Name())
		j := runJobNow(t, s, jobs.Spec{Kind: kind, SiteID: site.ID, Locks: siteLocks(site.ID), Payload: applyPayloadOf(next, cur, false)})
		if j.Status != state.JobSucceeded {
			t.Fatalf("%s: %s %s", kind, j.Status, j.Error)
		}
		rendered := countCalls(agent, agentrpc.ApplySiteConfig.Name()) - renders
		switched := countCalls(agent, agentrpc.WPObjectCache.Name()) - switches
		if wantRender := kind != kindObjectCache; (rendered == 1) != wantRender || switched != 1 {
			t.Fatalf("%s: %d renders, %d object-cache switches", kind, rendered, switched)
		}
		if got, _ := s.Store.GetSite(site.ID); got.AppliedGeneration != got.DesiredGeneration {
			t.Fatalf("%s: left pending: %d/%d", kind, got.AppliedGeneration, got.DesiredGeneration)
		}
	}
}

// jsonHas reports whether the JSON object raw has key with the string val.
func jsonHas(raw []byte, key, val string) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	got, _ := m[key].(string)
	return got == val
}

// issueCalls are the sites the fake agent was asked to certify, in order.
func issueCalls(agent *fakeAgent) []rpc.CertificateParams {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	var out []rpc.CertificateParams
	for _, c := range agent.history {
		if c.method == agentrpc.IssueCert.Name() {
			out = append(out, c.params.(rpc.CertificateParams))
		}
	}
	return out
}

// R-4.3: new aliases on a site with a Let's Encrypt certificate get the
// certificate reissued for them once rendered; a failed reissue says the
// old certificate is still served; a custom certificate gets a warning
// and no reissue.
func TestPatchAliasesReissuesTheCertificate(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "certs.example.com")
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.ACMEEmail = "ops@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	live := rpc.CertStatusResult{Certs: map[string]rpc.CertInfo{site.Domain: {Exists: true, NotAfter: time.Now().Add(60 * 24 * time.Hour)}}}
	agent.setResult(agentrpc.CertStatus.Name(), live)
	before := len(issueCalls(agent))

	code, a := patchSite(t, c, site.ID, map[string]any{"aliases": []string{"www.certs.example.com"}})
	if code != http.StatusAccepted || a.Warnings != nil {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	waitForTasks(t, s)
	calls := issueCalls(agent)
	if len(calls) != before+1 || !slices.Equal(calls[len(calls)-1].Site.Aliases, []string{"www.certs.example.com"}) {
		t.Fatalf("issue calls = %+v, want one for the new aliases", calls[before:])
	}

	// Aliases that do not change ask for nothing.
	if code, _ := patchSite(t, c, site.ID, map[string]any{"aliases": []string{"www.certs.example.com"}}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitForTasks(t, s)
	if n := len(issueCalls(agent)); n != before+1 {
		t.Fatalf("%d issue calls after an unchanged alias list", n-before)
	}

	agent.setErr(agentrpc.IssueCert.Name(), fmt.Errorf("simulated ACME failure"))
	if code, _ := patchSite(t, c, site.ID, map[string]any{"aliases": []string{"shop.certs.example.com"}}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitForTasks(t, s)
	resp, body := c.do("GET", "/api/tasks", nil)
	var tasks []struct{ Kind, Status, Error string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tasks) != nil {
		t.Fatalf("tasks = %d %s", resp.StatusCode, body)
	}
	failed := false
	for _, tk := range tasks {
		if tk.Kind == "certificate.issue" && tk.Status == "failed" && strings.HasPrefix(tk.Error, msgCertNotReissued) {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("no failed reissue saying the old certificate is kept: %s", body)
	}
	agent.setErr(agentrpc.IssueCert.Name(), nil)

	live.Certs[site.Domain] = rpc.CertInfo{Exists: true, Custom: true, NotAfter: time.Now().Add(60 * 24 * time.Hour)}
	agent.setResult(agentrpc.CertStatus.Name(), live)
	n := len(issueCalls(agent))
	code, a = patchSite(t, c, site.ID, map[string]any{"aliases": []string{}})
	if code != http.StatusAccepted || len(a.Warnings) != 1 || a.Warnings[0] != msgCustomCertAliases {
		t.Fatalf("custom certificate = %d %+v", code, a)
	}
	waitForTasks(t, s)
	if len(issueCalls(agent)) != n {
		t.Fatal("a custom certificate was replaced by a reissue")
	}
}
