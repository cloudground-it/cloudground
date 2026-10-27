package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func lastRender(agent *fakeAgent) rpc.RenderFunctionsParams {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	p, _ := agent.lastParams[agentrpc.RenderFunctions.Name()].(rpc.RenderFunctionsParams)
	return p
}

// #143 v0.9.0 (SPEC §17): standalone functions are created with the
// agent's release and rendered into nginx, their secrets sealed and handed
// over only in the environment, their token made once and rotated, rolled
// back, invoked and deleted.
func TestFunctions(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	base := "/api/functions"

	// The server lacks the runtime: 412.
	if resp, b := admin.do("POST", base, map[string]any{"name": "hello", "runtime": "node"}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("no runtime = %d %s", resp.StatusCode, b)
	}
	agent.setResult(agentrpc.Capabilities.Name(), rpc.Capabilities{Runtimes: rpc.RuntimesResult{Node: []rpc.RuntimeVersion{{Version: "22"}}}})
	s.staleCapabilities()
	agent.setResult(agentrpc.SetFunction.Name(), rpc.FunctionResult{Release: "01JBXR3C2D0000000000000001", Releases: []string{"01JBXR3C2D0000000000000001"}})

	for _, bad := range []map[string]any{
		{"name": "Bad", "runtime": "node"},
		{"name": "domain", "runtime": "node"},
		{"name": "hello", "runtime": "node", "memory_mb": 8},
		{"name": "hello", "runtime": "node", "schedule": "every minute"},
		{"name": "hello", "runtime": "node", "env": map[string]string{"PORT": "1"}},
		{"name": "hello", "runtime": "node", "auth": "basic"},
		{"name": "hello", "runtime": "node", "cors_origins": []string{"https://a.example.com/x"}},
		{"name": "hello", "runtime": "node", "files": map[string]string{"lib.js": "x"}},
		{"name": "hello", "runtime": "node", "template": "cron"},
	} {
		if resp, b := admin.do("POST", base, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, b)
		}
	}
	if list, _ := s.Store.ListFunctions(); len(list) != 0 {
		t.Fatalf("a refused create left %v", list)
	}
	resp, body := admin.do("POST", base, map[string]any{"name": "hello", "runtime": "node", "schedule": "*/5 * * * *", "auth": "token",
		"secrets": map[string]string{"API_KEY": "s3cret"}, "env": map[string]string{"MODE": "live"}})
	if resp.StatusCode != http.StatusCreated || strings.Contains(string(body), "s3cret") {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID       int64    `json:"id"`
		Token    string   `json:"token"`
		HasToken bool     `json:"has_token"`
		URLs     []string `json:"urls"`
	}
	_ = json.Unmarshal(body, &created)
	if !functions.ValidToken(created.Token) || !created.HasToken || len(created.URLs) != 0 {
		t.Fatalf("created %s", body)
	}
	agent.mu.Lock()
	sp, _ := agent.lastParams[agentrpc.SetFunction.Name()].(rpc.SetFunctionParams)
	agent.mu.Unlock()
	if sp.Env["API_KEY"] != "s3cret" || sp.Env["MODE"] != "live" || sp.Function.MemoryMB != 128 || sp.Function.ID != created.ID || sp.Files["index.js"] == "" {
		t.Fatalf("agent got %+v", sp)
	}
	rp := lastRender(agent)
	if len(rp.Functions) != 1 || rp.Functions[0].Token != created.Token || rp.Functions[0].Name != "hello" {
		t.Fatalf("rendered %+v", rp)
	}
	if sec, _ := s.Store.Secret(state.FunctionSecret(created.ID, "API_KEY")); sec != "s3cret" {
		t.Fatalf("secret %q", sec)
	}
	f, _ := s.Store.GetFunction("hello")
	if f.Release != "01JBXR3C2D0000000000000001" || f.Auth != "token" {
		t.Fatalf("stored %+v", f)
	}
	if resp, _ := admin.do("POST", base, map[string]any{"name": "hello", "runtime": "node"}); resp.StatusCode != http.StatusConflict {
		t.Fatal("a second hello taken")
	}
	// The token is shown once: never again.
	if _, b := admin.do("GET", base+"/hello", nil); strings.Contains(string(b), created.Token) {
		t.Fatalf("the token is shown again: %s", b)
	}

	// A change of code goes to the agent; a refused render stores nothing
	// and puts the secrets back.
	if resp, b := admin.do("PUT", base+"/hello", map[string]any{"files": map[string]string{"index.js": "exports.handler = () => 'v2';", "package.json": "{}"}, "timeout_sec": 30}); resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d %s", resp.StatusCode, b)
	}
	agent.mu.Lock()
	sp, _ = agent.lastParams[agentrpc.SetFunction.Name()].(rpc.SetFunctionParams)
	agent.mu.Unlock()
	if sp.Files["package.json"] != "{}" || sp.Function.TimeoutSec != 30 {
		t.Fatalf("update sent %+v", sp)
	}
	agent.setErr(agentrpc.RenderFunctions.Name(), agentrpc.Wrap(agentrpc.CodeUnavailable, "nginx -t failed", nil))
	if resp, _ := admin.do("PUT", base+"/hello", map[string]any{"memory_mb": 512, "secrets": map[string]string{"API_KEY": "new"}}); resp.StatusCode < 400 {
		t.Fatal("a refused render answered ok")
	}
	agent.setErr(agentrpc.RenderFunctions.Name(), nil)
	if f, _ := s.Store.GetFunction("hello"); f.MemoryMB != 128 {
		t.Fatal("a refused render was stored")
	}
	if sec, _ := s.Store.Secret(state.FunctionSecret(created.ID, "API_KEY")); sec != "s3cret" {
		t.Fatalf("the secret was not put back: %q", sec)
	}

	// A new token replaces the old one in nginx.
	resp, body = admin.do("POST", base+"/hello/token", nil)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode != http.StatusOK || !functions.ValidToken(tok.Token) || tok.Token == created.Token || lastRender(agent).Functions[0].Token != tok.Token {
		t.Fatalf("token = %d %s", resp.StatusCode, body)
	}

	agent.setResult(agentrpc.RollbackFunction.Name(), rpc.FunctionResult{Release: "01JBXR3C2D0000000000000000"})
	if resp, b := admin.do("POST", base+"/hello/rollback", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "01JBXR3C2D0000000000000000") {
		t.Fatalf("rollback = %d %s", resp.StatusCode, b)
	}
	agent.setResult(agentrpc.InvokeFunction.Name(), rpc.InvokeResult{Status: 200, MS: 12, Body: "ok"})
	if resp, b := admin.do("POST", base+"/hello/invoke", map[string]string{"method": "get", "path": "/x?y=1", "body": "{}"}); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"status":200`) {
		t.Fatalf("invoke = %d %s", resp.StatusCode, b)
	}
	agent.mu.Lock()
	ip, _ := agent.lastParams[agentrpc.InvokeFunction.Name()].(rpc.InvokeFunctionParams)
	agent.mu.Unlock()
	if ip.Method != "GET" || ip.Path != "/x?y=1" || ip.Source != "test" {
		t.Fatalf("invoke sent %+v", ip)
	}
	if resp, _ := admin.do("POST", base+"/hello/invoke", map[string]string{"path": "no-slash"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a path without / taken")
	}

	// The sweep invokes a due function once in its minute.
	before := agent.count(agentrpc.InvokeFunction.Name())
	runJobNow(t, s, jobs.Spec{Kind: "functions.sweep"})
	runJobNow(t, s, jobs.Spec{Kind: "functions.sweep"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && agent.count(agentrpc.InvokeFunction.Name()) == before && time.Now().Minute()%5 == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if time.Now().Minute()%5 == 0 {
		if n := agent.count(agentrpc.InvokeFunction.Name()) - before; n != 1 {
			t.Fatalf("the sweep invoked %d times in one minute", n)
		}
	}

	if resp, _ := admin.do("DELETE", base+"/hello", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if _, err := s.Store.GetFunction("hello"); err == nil {
		t.Fatal("still stored")
	}
	if sec, _ := s.Store.Secret(state.FunctionSecret(created.ID, "API_KEY")); sec != "" {
		t.Fatal("the secret outlived the function")
	}
	if !agent.called(agentrpc.DeleteFunction.Name()) || len(lastRender(agent).Functions) != 0 {
		t.Fatal("the agent did not remove it")
	}
	if resp, _ := admin.do("GET", base+"/hello", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("a deleted function answers")
	}

	// A refused create leaves nothing.
	agent.setErr(agentrpc.SetFunction.Name(), agentrpc.Invalid("installing the function's dependencies failed: npm ERR!"))
	if resp, b := admin.do("POST", base, map[string]any{"name": "broken", "runtime": "node"}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "npm ERR!") {
		t.Fatalf("refused create = %d %s", resp.StatusCode, b)
	}
	if _, err := s.Store.GetFunction("broken"); err == nil {
		t.Fatal("a refused create was stored")
	}
}

// Every host name is one owner's (SPEC §17.4, §17.5): a function's domain
// cannot be a site's, another function's, the functions domain or the
// panel's, and a site cannot take a function's.
func TestFunctionDomains(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	seedActiveSite(t, s, "site.example.com", state.SiteStatic, "", 0)
	for _, n := range []string{"a", "b"} {
		f, err := s.Store.CreateFunction(state.Function{Name: n, Runtime: "node", Version: "22", MemoryMB: 128, TimeoutSec: 10, Release: "01JBXR3C2D0000000000000001"})
		if err != nil {
			t.Fatal(err)
		}
		_ = f
	}
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.PanelDomain = "panel.example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp, b := admin.do("PUT", "/api/functions/a/domains", map[string]any{"domains": []string{"API.example.com."}}); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"https://api.example.com/"`) {
		t.Fatalf("domains = %d %s", resp.StatusCode, b)
	}
	if rp := lastRender(agent); len(rp.Functions) != 2 || rp.Functions[0].Domains[0] != "api.example.com" {
		t.Fatalf("rendered %+v", rp)
	}
	for _, taken := range []string{"site.example.com", "api.example.com", "panel.example.com"} {
		if resp, b := admin.do("PUT", "/api/functions/b/domains", map[string]any{"domains": []string{taken}}); resp.StatusCode != http.StatusConflict {
			t.Errorf("%s = %d %s", taken, resp.StatusCode, b)
		}
	}
	for _, bad := range []string{"203.0.113.7", "not a host", "a..b"} {
		if resp, _ := admin.do("PUT", "/api/functions/b/domains", map[string]any{"domains": []string{bad}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s taken", bad)
		}
	}
	// The functions domain.
	if resp, b := admin.do("PUT", "/api/functions/domain", map[string]any{"domain": "api.example.com"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a function's domain as the functions domain = %d %s", resp.StatusCode, b)
	}
	if resp, b := admin.do("PUT", "/api/functions/domain", map[string]any{"domain": "fn.example.com"}); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"domain":"fn.example.com"`) {
		t.Fatalf("functions domain = %d %s", resp.StatusCode, b)
	}
	if rp := lastRender(agent); rp.Domain != "fn.example.com" {
		t.Fatalf("rendered %+v", rp)
	}
	if _, b := admin.do("GET", "/api/functions", nil); !strings.Contains(string(b), `"https://fn.example.com/a"`) || !strings.Contains(string(b), `"domain":"fn.example.com"`) {
		t.Fatalf("list %s", b)
	}
	if resp, _ := admin.do("PUT", "/api/functions/b/domains", map[string]any{"domains": []string{"fn.example.com"}}); resp.StatusCode != http.StatusConflict {
		t.Fatal("the functions domain taken by a function")
	}
	// A site cannot take a function's domain or the functions domain.
	for _, d := range []string{"api.example.com", "fn.example.com"} {
		if resp, b := admin.do("POST", "/api/sites", map[string]any{"domain": d, "type": "static"}); resp.StatusCode != http.StatusConflict {
			t.Errorf("a site on %s = %d %s", d, resp.StatusCode, b)
		}
	}
	// Cleared: the lineage goes.
	if resp, _ := admin.do("PUT", "/api/functions/domain", map[string]any{"domain": ""}); resp.StatusCode != http.StatusOK {
		t.Fatal("clearing refused")
	}
	if !agent.called(agentrpc.DeleteFunctionsCertificate.Name()) {
		t.Fatal("the functions domain's lineage was kept")
	}
	if ps, _ := s.Store.PanelSettings(); ps.FunctionsDomain != "" {
		t.Fatal("not cleared")
	}
}

// A v0.8.x site function becomes a standalone one (SPEC §17.8): its
// settings and secrets carried over, the site's settings emptied, the
// marker set; a refused one stays in its site and raises the alert.
func TestFunctionsMigration(t *testing.T) {
	s, agent, _ := testServer(t)
	site := seedActiveSite(t, s, "old.example.com", state.SiteStatic, "", 0)
	other := seedActiveSite(t, s, "other.example.com", state.SiteStatic, "", 0)
	if _, err := s.Store.CreateFunction(state.Function{Name: "taken", Runtime: "node", Version: "22", MemoryMB: 128, TimeoutSec: 10}); err != nil {
		t.Fatal(err)
	}
	for _, st := range []state.Site{site, other} {
		fns := []state.SiteFunction{{Name: "hello", Runtime: "node", Version: "22", MemoryMB: 256, TimeoutSec: 20, Env: map[string]string{"A": "1"}, Secrets: []string{"KEY"}}}
		if st.ID == site.ID {
			fns = append(fns, state.SiteFunction{Name: "taken", Runtime: "node", Version: "22", MemoryMB: 128, TimeoutSec: 10})
		}
		if _, err := s.Store.MutateSite(st.ID, func(x *state.Site) error { x.Config.Functions = fns; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := s.Store.SetSecret(state.SiteSecret(st.ID, "fn.hello.KEY"), "v-"+st.Domain); err != nil {
			t.Fatal(err)
		}
	}
	agent.setResult(agentrpc.MigrateFunction.Name(), rpc.FunctionResult{Release: "01JBXR3C2D0000000000000001"})
	if err := s.migrateFunctions(t.Context(), func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Store.ListFunctions()
	names := map[string]state.Function{}
	for _, f := range list {
		names[f.Name] = f
	}
	h, ok := names["hello"]
	h2, ok2 := names["hello-"+itoa(other.ID)]
	tk, ok3 := names["taken-"+itoa(site.ID)]
	if !ok || !ok2 || !ok3 || h.MemoryMB != 256 || h.Release == "" || h.Env["A"] != "1" || tk.Release == "" {
		t.Fatalf("functions %+v", names)
	}
	for id, want := range map[int64]string{h.ID: "v-old.example.com", h2.ID: "v-other.example.com"} {
		if v, _ := s.Store.Secret(state.FunctionSecret(id, "KEY")); v != want {
			t.Errorf("function %d's secret %q", id, v)
		}
	}
	if v, _ := s.Store.Secret(state.SiteSecret(site.ID, "fn.hello.KEY")); v != "" {
		t.Error("the old secret was kept")
	}
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.Functions) != 0 {
		t.Fatalf("the site still holds %+v", st.Config.Functions)
	}
	if ps, _ := s.Store.PanelSettings(); ps.FunctionsMigration != "done" {
		t.Fatalf("marker %q", ps.FunctionsMigration)
	}

	// A refusal leaves the old function and the alert.
	if _, err := s.Store.MutateSite(site.ID, func(x *state.Site) error {
		x.Config.Functions = []state.SiteFunction{{Name: "late", Runtime: "node", Version: "22", MemoryMB: 128, TimeoutSec: 10}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	agent.setErr(agentrpc.MigrateFunction.Name(), agentrpc.Invalid("no code"))
	if err := s.migrateFunctions(t.Context(), func(int, string) {}); err == nil {
		t.Fatal("a refused migration succeeded")
	}
	if _, err := s.Store.GetFunction("late"); err == nil {
		t.Fatal("a refused migration left its function")
	}
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.Functions) != 1 {
		t.Fatal("the refused function left its site")
	}
	ps, _ := s.Store.PanelSettings()
	if !strings.HasPrefix(ps.FunctionsMigration, "failed: ") || len(functionsConditions(ps)) != 1 {
		t.Fatalf("marker %q", ps.FunctionsMigration)
	}
}
