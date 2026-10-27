package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #143 (SPEC §3.20): functions are created with the agent's release, their
// secrets sealed and handed over only in the environment, rendered into
// the vhost, rolled back, invoked and deleted.
func TestFunctions(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "fn.example.com", state.SiteStatic, "", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/sites/%d/functions", site.ID)

	// The server lacks the runtime: 412.
	if resp, b := admin.do("POST", base, map[string]any{"name": "hello", "runtime": "node"}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("no runtime = %d %s", resp.StatusCode, b)
	}
	agent.setResult(agentrpc.Capabilities.Name(), rpc.Capabilities{Runtimes: rpc.RuntimesResult{Node: []rpc.RuntimeVersion{{Version: "22"}}}})
	s.staleCapabilities()
	agent.setResult(agentrpc.SetFunction.Name(), rpc.FunctionResult{Release: "01JBXR3C2D0000000000000001", Releases: []string{"01JBXR3C2D0000000000000001"}})

	for _, bad := range []map[string]any{
		{"name": "Bad", "runtime": "node"},
		{"name": "hello", "runtime": "node", "memory_mb": 8},
		{"name": "hello", "runtime": "node", "schedule": "every minute"},
		{"name": "hello", "runtime": "node", "env": map[string]string{"PORT": "1"}},
	} {
		if resp, b := admin.do("POST", base, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, b)
		}
	}
	resp, body := admin.do("POST", base, map[string]any{"name": "hello", "runtime": "node", "schedule": "*/5 * * * *",
		"secrets": map[string]string{"API_KEY": "s3cret"}, "env": map[string]string{"MODE": "live"}})
	if resp.StatusCode != http.StatusCreated || strings.Contains(string(body), "s3cret") || !strings.Contains(string(body), `"url":"https://fn.example.com/.functions/hello"`) {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	sp, _ := agent.lastParams[agentrpc.SetFunction.Name()].(rpc.SetFunctionParams)
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if sp.Env["API_KEY"] != "s3cret" || sp.Env["MODE"] != "live" || sp.Function.MemoryMB != 128 || sp.Function.TimeoutSec != 10 {
		t.Fatalf("agent got %+v", sp)
	}
	if len(ref.Site.Config.Functions) != 1 || ref.Site.Config.Functions[0].Name != "hello" || strings.Contains(fmt.Sprint(ref.Site), "s3cret") {
		t.Fatalf("rendered %+v", ref.Site.Config.Functions)
	}
	if sec, _ := s.Store.Secret(fnSecret(site.ID, "hello", "API_KEY")); sec != "s3cret" {
		t.Fatalf("secret %q", sec)
	}
	st, _ := s.Store.GetSite(site.ID)
	if len(st.Config.Functions) != 1 || st.Config.Functions[0].Release != "01JBXR3C2D0000000000000001" {
		t.Fatalf("stored %+v", st.Config.Functions)
	}
	if resp, _ := admin.do("POST", base, map[string]any{"name": "hello", "runtime": "node"}); resp.StatusCode != http.StatusConflict {
		t.Fatal("a second hello taken")
	}

	// A change of code goes to the agent; a refused render stores nothing.
	if resp, b := admin.do("PUT", base+"/hello", map[string]any{"code": "exports.handler = () => 'v2';", "timeout_sec": 30}); resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d %s", resp.StatusCode, b)
	}
	agent.mu.Lock()
	sp, _ = agent.lastParams[agentrpc.SetFunction.Name()].(rpc.SetFunctionParams)
	agent.mu.Unlock()
	if sp.Code != "exports.handler = () => 'v2';" || sp.Function.TimeoutSec != 30 {
		t.Fatalf("update sent %+v", sp)
	}
	agent.setErr(agentrpc.ApplySiteConfig.Name(), agentrpc.Wrap(agentrpc.CodeUnavailable, "nginx -t failed", nil))
	if resp, _ := admin.do("PUT", base+"/hello", map[string]any{"memory_mb": 512}); resp.StatusCode < 400 {
		t.Fatal("a refused render answered ok")
	}
	agent.setErr(agentrpc.ApplySiteConfig.Name(), nil)
	if st, _ := s.Store.GetSite(site.ID); st.Config.Functions[0].MemoryMB != 128 {
		t.Fatal("a refused render was stored")
	}

	agent.setResult(agentrpc.FunctionRollback.Name(), rpc.FunctionResult{Release: "01JBXR3C2D0000000000000000"})
	if resp, b := admin.do("POST", base+"/hello/rollback", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "01JBXR3C2D0000000000000000") {
		t.Fatalf("rollback = %d %s", resp.StatusCode, b)
	}
	agent.setResult(agentrpc.InvokeFunction.Name(), rpc.InvokeResult{Status: 200, MS: 12, Body: "ok"})
	if resp, b := admin.do("POST", base+"/hello/invoke", map[string]string{"body": "{}"}); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"status":200`) {
		t.Fatalf("invoke = %d %s", resp.StatusCode, b)
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
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.Functions) != 0 {
		t.Fatal("still stored")
	}
	if sec, _ := s.Store.Secret(fnSecret(site.ID, "hello", "API_KEY")); sec != "" {
		t.Fatal("the secret outlived the function")
	}
	if !agent.called(agentrpc.DeleteFunction.Name()) {
		t.Fatal("the agent did not remove it")
	}
	if resp, _ := admin.do("GET", base+"/hello", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("a deleted function answers")
	}
}
