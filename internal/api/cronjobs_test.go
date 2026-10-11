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

func cronSite(t *testing.T) (*Server, *fakeAgent, *client, state.Site) {
	t.Helper()
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "crontest.example.com", "type": "static"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create site = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain("crontest.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return s, agent, c, site
}

func lastCron(t *testing.T, agent *fakeAgent) rpc.CronParams {
	t.Helper()
	agent.mu.Lock()
	defer agent.mu.Unlock()
	p, ok := agent.lastParams[agentrpc.ApplyCron.Name()].(rpc.CronParams)
	if !ok {
		t.Fatal("ApplyCron was not called")
	}
	return p
}

// The API sends typed jobs, never crontab text; the agent renders.
func TestCronSendsTypedJobs(t *testing.T) {
	s, agent, c, site := cronSite(t)
	resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/cron", site.ID), map[string]string{
		"schedule": "*/5 * * * *", "command": "echo 'date fmt %Y-%m-%d also 100% done'", "description": "fmt",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cron = %d %s", resp.StatusCode, body)
	}
	var job state.CronJob
	json.Unmarshal(body, &job)
	p := lastCron(t, agent)
	if p.Site.ID != site.ID || len(p.Jobs) != 1 || p.Jobs[0].ID != job.ID || !p.Jobs[0].Enabled ||
		p.Jobs[0].Command != "echo 'date fmt %Y-%m-%d also 100% done'" || p.Jobs[0].Schedule != "*/5 * * * *" {
		t.Fatalf("params %+v", p)
	}

	// Run now carries the site and the job.
	agent.results[agentrpc.RunCron.Name()] = rpc.RunCronResult{Status: "succeeded", Output: "ok"}
	resp, body = c.do("POST", fmt.Sprintf("/api/cron/%d/run", job.ID), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"succeeded"`) {
		t.Fatalf("run = %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	rp, _ := agent.lastParams[agentrpc.RunCron.Name()].(rpc.RunCronParams)
	agent.mu.Unlock()
	if rp.Site.ID != site.ID || rp.Job.ID != job.ID || rp.Job.Command != job.Command {
		t.Fatalf("run params %+v", rp)
	}
	if got, _ := s.Store.GetCronJob(job.ID); got.LastStatus != "succeeded" {
		t.Fatalf("last status %q", got.LastStatus)
	}
}

// A schedule crontab would refuse never reaches the store or the agent.
func TestCronRefusesBadJobs(t *testing.T) {
	s, agent, c, site := cronSite(t)
	for _, body := range []map[string]string{
		{"schedule": "*/15 * * * *\n* * * * * id", "command": "true"},
		{"schedule": "60 * * * *", "command": "true"},
		{"schedule": "5/10 * * * *", "command": "true"},
		{"schedule": "* * * *", "command": "true"},
		{"schedule": "* * * * *", "command": "true\nid"},
		{"schedule": "* * * * *", "command": strings.Repeat("x", 990)},
		{"schedule": "* * * * *"},
	} {
		resp, b := c.do("POST", fmt.Sprintf("/api/sites/%d/cron", site.ID), body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q = %d %s", body, resp.StatusCode, b)
		}
	}
	if jobs, _ := s.Store.ListCronJobs(site.ID); len(jobs) != 0 {
		t.Fatalf("stored %d refused jobs", len(jobs))
	}
	agent.mu.Lock()
	_, called := agent.lastParams[agentrpc.ApplyCron.Name()]
	agent.mu.Unlock()
	if called {
		t.Fatal("a refused job reached the agent")
	}
}

// PATCH edits a job and turns it off and on; the agent always gets the
// whole list with each job's flag.
func TestCronUpdate(t *testing.T) {
	s, agent, c, site := cronSite(t)
	_, body := c.do("POST", fmt.Sprintf("/api/sites/%d/cron", site.ID), map[string]string{"schedule": "* * * * *", "command": "true"})
	var job state.CronJob
	json.Unmarshal(body, &job)
	path := fmt.Sprintf("/api/cron/%d", job.ID)

	resp, b := c.do("PATCH", path, map[string]any{"enabled": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable = %d %s", resp.StatusCode, b)
	}
	if p := lastCron(t, agent); len(p.Jobs) != 1 || p.Jobs[0].Enabled {
		t.Fatalf("disable sent %+v", p.Jobs)
	}
	resp, b = c.do("PATCH", path, map[string]any{"enabled": true, "schedule": "0 4 * * 1-5", "command": "php artisan schedule:run", "description": "nightly"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit = %d %s", resp.StatusCode, b)
	}
	got, _ := s.Store.GetCronJob(job.ID)
	if !got.Enabled || got.Schedule != "0 4 * * 1-5" || got.Command != "php artisan schedule:run" || got.Description != "nightly" {
		t.Fatalf("stored %+v", got)
	}
	if p := lastCron(t, agent); !p.Jobs[0].Enabled || p.Jobs[0].Schedule != "0 4 * * 1-5" {
		t.Fatalf("edit sent %+v", p.Jobs)
	}

	// A bad edit is refused and leaves the job as it was.
	resp, _ = c.do("PATCH", path, map[string]any{"schedule": "* 24 * * *"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad edit = %d", resp.StatusCode)
	}
	// An edit the agent fails is undone in the store.
	agent.errs[agentrpc.ApplyCron.Name()] = fmt.Errorf("crontab: exit status 1")
	resp, _ = c.do("PATCH", path, map[string]any{"command": "echo new"})
	delete(agent.errs, agentrpc.ApplyCron.Name())
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a failed install reported success")
	}
	if got, _ := s.Store.GetCronJob(job.ID); got.Command != "php artisan schedule:run" {
		t.Fatalf("a failed edit was kept: %q", got.Command)
	}
	if resp, _ := c.do("PATCH", "/api/cron/9999", map[string]any{"enabled": false}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing job = %d", resp.StatusCode)
	}
}
