package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #28 (SPEC §3.19): a Node.js site is created with its runtime (installed
// first when missing), its app settings change with the secrets sealed and
// handed to the agent only in the environment, and staging is refused.
func TestManagedAppSite(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)

	for _, bad := range []map[string]any{
		{"domain": "a.example.com", "type": "node", "runtime_version": "16"},
		{"domain": "a.example.com", "type": "python", "runtime_version": "2.7"},
		{"domain": "a.example.com", "type": "node", "start_command": "node a.js\nrm -rf /"},
	} {
		if resp, body := admin.do("POST", "/api/sites", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, body)
		}
	}

	site := createViaAPI(t, s, admin, map[string]any{"domain": "node.example.com", "type": "node"})
	if site.Config.App.Version != "22" || site.Config.App.Start != "node server.js" || site.PHPVersion != "" || site.Config.CacheEnabled {
		t.Fatalf("created %+v (php %q)", site.Config.App, site.PHPVersion)
	}
	agent.mu.Lock()
	inst, _ := agent.lastParams[agentrpc.RuntimeInstall.Name()].(rpc.RuntimeInstallParams)
	agent.mu.Unlock()
	if inst != (rpc.RuntimeInstallParams{Runtime: "node", Version: "22"}) {
		t.Fatalf("runtime install %+v", inst)
	}

	path := fmt.Sprintf("/api/sites/%d/app", site.ID)
	resp, body := admin.do("PUT", path, map[string]any{
		"start": "node dist/main.js", "env": map[string]string{"NODE_LOG": "info"},
		"secrets": map[string]string{"DATABASE_URL": "mysql://u:p@db/x"},
	})
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "mysql://") {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	var v appView
	_ = json.Unmarshal(body, &v)
	if v.Start != "node dist/main.js" || v.Env["NODE_LOG"] != "info" || len(v.Secrets) != 1 || v.Secrets[0] != "DATABASE_URL" {
		t.Fatalf("answer %+v", v)
	}
	if sec, _ := s.Store.Secret(appSecret(site.ID, "DATABASE_URL")); sec != "mysql://u:p@db/x" {
		t.Fatalf("secret %q", sec)
	}
	agent.mu.Lock()
	envp, _ := agent.lastParams[agentrpc.SetAppEnv.Name()].(rpc.AppEnvParams)
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if envp.Env["DATABASE_URL"] != "mysql://u:p@db/x" || envp.Env["NODE_LOG"] != "info" || ref.Site.Config.App.Start != "node dist/main.js" {
		t.Fatalf("agent got env %v, start %q", envp.Env, ref.Site.Config.App.Start)
	}
	if strings.Contains(fmt.Sprint(ref.Site), "mysql://") {
		t.Fatal("a secret value travelled in the site")
	}
	// The site's own view never carries a secret value.
	if _, b := admin.do("GET", fmt.Sprintf("/api/sites/%d", site.ID), nil); strings.Contains(string(b), "mysql://") {
		t.Fatal("GET /api/sites/{id} shows a secret")
	}

	// The mask keeps, "" removes; a reserved name and a missing version refuse.
	if resp, b := admin.do("PUT", path, map[string]any{"secrets": map[string]string{"DATABASE_URL": secretMask}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("mask = %d %s", resp.StatusCode, b)
	}
	if sec, _ := s.Store.Secret(appSecret(site.ID, "DATABASE_URL")); sec != "mysql://u:p@db/x" {
		t.Fatal("the mask changed the secret")
	}
	if resp, _ := admin.do("PUT", path, map[string]any{"env": map[string]string{"PORT": "1"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("PORT taken")
	}
	if resp, _ := admin.do("PUT", path, map[string]any{"version": "24"}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatal("a version the server lacks taken")
	}
	if resp, _ := admin.do("PUT", path, map[string]any{"secrets": map[string]string{"DATABASE_URL": ""}}); resp.StatusCode != http.StatusOK {
		t.Fatal("remove")
	}
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.App.Secrets) != 0 {
		t.Fatalf("still listed %v", st.Config.App.Secrets)
	}

	// A refused render stores nothing and puts the secret back.
	agent.setErr(agentrpc.ApplySiteConfig.Name(), agentrpc.Wrap(agentrpc.CodeUnavailable, "systemd said no", nil))
	if resp, _ := admin.do("PUT", path, map[string]any{"start": "node other.js", "secrets": map[string]string{"TOKEN": "t1"}}); resp.StatusCode < 400 {
		t.Fatal("a refused render answered ok")
	}
	agent.setErr(agentrpc.ApplySiteConfig.Name(), nil)
	if st, _ := s.Store.GetSite(site.ID); st.Config.App.Start != "node dist/main.js" {
		t.Fatal("a refused render was stored")
	}
	if sec, _ := s.Store.Secret(appSecret(site.ID, "TOKEN")); sec != "" {
		t.Fatal("the secret of a refused change stayed")
	}

	if resp, b := admin.do("POST", fmt.Sprintf("/api/sites/%d/staging", site.ID), nil); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "staging") {
		t.Fatalf("staging = %d %s", resp.StatusCode, b)
	}
	if resp, _ := admin.do("POST", fmt.Sprintf("/api/sites/%d/app/restart", site.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatal("restart")
	}
	if !agent.called(agentrpc.AppControl.Name()) {
		t.Fatal("restart did not reach the agent")
	}

	php := seedActiveSite(t, s, "php.example.com", state.SitePHP, "8.4", 0)
	if resp, _ := admin.do("GET", fmt.Sprintf("/api/sites/%d/app", php.ID), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a PHP site has an app")
	}
}

func TestRuntimesRoutes(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	agent.setResult(agentrpc.Runtimes.Name(), rpc.RuntimesResult{Node: []rpc.RuntimeVersion{{Version: "22", Full: "v22.11.0"}}})
	resp, body := admin.do("GET", "/api/runtimes", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"v22.11.0"`) || !strings.Contains(string(body), `"24"`) {
		t.Fatalf("runtimes = %d %s", resp.StatusCode, body)
	}
	if resp, _ := admin.do("POST", "/api/runtimes/install", map[string]string{"runtime": "ruby", "version": "3"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("ruby taken")
	}
	if resp, b := admin.do("POST", "/api/runtimes/install", map[string]string{"runtime": "node", "version": "24"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install = %d %s", resp.StatusCode, b)
	}
	waitForTasks(t, s)
	if !agent.called(agentrpc.RuntimeInstall.Name()) {
		t.Fatal("the install did not reach the agent")
	}
}
