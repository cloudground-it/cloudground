package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

func (f *fakeAgent) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeAgent) setResult(method string, v any) {
	f.mu.Lock()
	f.results[method] = v
	f.mu.Unlock()
}

func (f *fakeAgent) setErr(method string, err error) {
	f.mu.Lock()
	f.errs[method] = err
	f.mu.Unlock()
}

// The capabilities page shows the agent's facts: the PHP versions it found,
// the default a new site gets, the services the page lists.
func TestCapabilitiesEndpoint(t *testing.T) {
	s, agent, ts := testServer(t)
	s.DefaultPHP = "8.4"
	c := setupAdmin(t, s, ts)
	resp, body := c.do("GET", "/api/system/capabilities", nil)
	var v capabilitiesView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil {
		t.Fatalf("capabilities = %d %s", resp.StatusCode, body)
	}
	if strings.Join(v.PHPVersions, ",") != "8.5,8.4,8.3,8.2" || v.DefaultPHP != "8.4" || v.SitesRoot != "/srv/sites" {
		t.Fatalf("view = %+v", v)
	}
	var names []string
	for _, sv := range v.Services {
		names = append(names, sv.Name)
	}
	if strings.Join(names, ",") != "nginx,mariadb,redis" {
		t.Fatalf("services = %v, want the listed ones", names)
	}
	// Cached: a second page load does not ask the agent again.
	c.do("GET", "/api/system/capabilities", nil)
	if n := agent.count(agentrpc.Capabilities.Name()); n != 1 {
		t.Fatalf("agent asked %d times for two loads", n)
	}
	// An agent with no PHP found reports an empty list, not null.
	agent.setResult(agentrpc.Capabilities.Name(), rpc.Capabilities{Paths: rpc.HostPaths{SitesRoot: "/srv/sites"}})
	s.forgetCapabilities()
	_, body = c.do("GET", "/api/system/capabilities", nil)
	if !strings.Contains(string(body), `"php_versions":[]`) || !strings.Contains(string(body), `"services":[]`) {
		t.Fatalf("empty lists: %s", body)
	}
}

// The cache: fresh answers are reused, old ones refreshed, a self-update
// forgets, and an agent that cannot answer leaves the last answer in use.
func TestCapabilitiesCache(t *testing.T) {
	s, agent, _ := testServer(t)
	ctx := t.Context()
	name := agentrpc.Capabilities.Name()
	if _, err := s.capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.capabilities(ctx); err != nil || agent.count(name) != 1 {
		t.Fatalf("a fresh answer is reused: %d calls, %v", agent.count(name), err)
	}
	s.caps.mu.Lock()
	s.caps.at = time.Now().Add(-capsTTL - time.Second)
	s.caps.mu.Unlock()
	restarted := testCapabilities
	restarted.StartedAt = time.Now()
	restarted.PHPVersions = []string{"8.5"}
	agent.setResult(name, restarted)
	c, err := s.capabilities(ctx)
	if err != nil || agent.count(name) != 2 || strings.Join(c.PHPVersions, ",") != "8.5" {
		t.Fatalf("an old answer is refreshed: %d calls, %v %v", agent.count(name), c.PHPVersions, err)
	}
	s.forgetCapabilities()
	agent.setErr(name, agentrpc.Wrap(agentrpc.CodeUnavailable, "The agent is not running", fmt.Errorf("dial")))
	c, err = s.capabilities(ctx)
	if err != nil || strings.Join(c.PHPVersions, ",") != "8.5" || agent.count(name) != 3 {
		t.Fatalf("the agent down: the last answer serves: %v %v", c.PHPVersions, err)
	}

	fresh, _, _ := testServer(t)
	fresh.Agent.(*fakeAgent).setErr(name, agentrpc.Wrap(agentrpc.CodeUnavailable, "The agent is not running", fmt.Errorf("dial")))
	if _, err := fresh.capabilities(ctx); err == nil {
		t.Fatal("no answer ever: the error")
	}
}

// PHP versions are checked against what the agent found, for a new site and
// for a site's PHP settings; the default falls back to an installed one.
func TestPHPVersionsComeFromTheAgent(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "old.example.com", "type": "php", "php_version": "8.1"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "PHP 8.1 is not installed on this server") {
		t.Fatalf("create with an absent version = %d %s", resp.StatusCode, body)
	}
	resp, body = c.do("POST", "/api/sites", map[string]any{"domain": "bad.example.com", "type": "php", "php_version": "8.4; rm"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "unsupported PHP version") {
		t.Fatalf("create with a malformed version = %d %s", resp.StatusCode, body)
	}
	if agent.called(agentrpc.CreateSite.Name()) {
		t.Fatal("the agent was asked to create a refused site")
	}

	// The configured default (8.4) is not installed here: the newest is used.
	only := testCapabilities
	only.PHPVersions = []string{"8.5", "8.3"}
	only.Paths.SitesRoot = "/data/sites"
	agent.setResult(agentrpc.Capabilities.Name(), only)
	s.forgetCapabilities()
	s.DefaultPHP = "8.4"
	resp, body = c.do("POST", "/api/sites", map[string]any{"domain": "new.example.com", "type": "php"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain("new.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if site.PHPVersion != "8.5" {
		t.Errorf("php = %s, want the newest installed", site.PHPVersion)
	}
	if site.RootPath != "/data/sites/new.example.com" {
		t.Errorf("root = %s, want under the agent's sites root", site.RootPath)
	}

	resp, body = c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"php_version": "8.4"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "PHP 8.4 is not installed on this server") {
		t.Fatalf("set an absent version = %d %s", resp.StatusCode, body)
	}
	resp, body = c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"php_version": "8.3"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("set an installed version = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if got, _ := s.Store.GetSite(site.ID); got.PHPVersion != "8.3" {
		t.Errorf("php after the change = %s", got.PHPVersion)
	}
}

// The panel restarts only the services its page lists; which those are is
// the agent's answer.
func TestRestartOnlyListedServices(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	for _, name := range []string{"cloudground-api", "sshd", "php-fpm"} {
		if resp, body := c.do("POST", "/api/services/"+name+"/restart", nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("restart %s = %d %s", name, resp.StatusCode, body)
		}
	}
	if agent.called(agentrpc.RestartService.Name()) {
		t.Fatal("the agent was asked to restart an unlisted service")
	}
	if resp, body := c.do("POST", "/api/services/redis/restart", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("restart redis = %d %s", resp.StatusCode, body)
	}
	if p, _ := agent.lastParams[agentrpc.RestartService.Name()].(rpc.ServiceParams); p.Name != "redis" {
		t.Fatalf("restart params = %+v", agent.lastParams[agentrpc.RestartService.Name()])
	}
}
