package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Deploys started together each get a release of their own. Release IDs were
// the time to the second: the second deploy of a second built over the
// first's name (refused by the agent) and could not be recorded (the
// deployments table is unique on site and release).
func TestConcurrentDeploysGetDistinctReleases(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "deploys.example.com", "type": "static"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create site = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain("deploys.example.com")
	if err != nil {
		t.Fatal(err)
	}

	const n = 4
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/deployments", site.ID),
				map[string]string{"source_dir": site.RootPath + "/current"})
			if resp.StatusCode != http.StatusAccepted {
				t.Errorf("deploy %d = %d %s", i, resp.StatusCode, body)
				return
			}
			var out struct {
				ReleaseID string `json:"release_id"`
			}
			json.Unmarshal(body, &out)
			ids[i] = out.ReleaseID
		}()
	}
	wg.Wait()
	waitForTasks(t, s)

	seen := map[string]bool{}
	for _, id := range ids {
		if !siteval.ValidReleaseID(id) || seen[id] {
			t.Fatalf("release ids %q: each must be valid and distinct", ids)
		}
		seen[id] = true
	}
	tasks, _ := s.Store.ListJobs(50, false)
	for _, task := range tasks {
		if task.Kind == "deploy.create" && task.Status != state.JobSucceeded {
			t.Errorf("deploy task %d: %s %s", task.ID, task.Status, task.Error)
		}
	}
	deps, _ := s.Store.ListDeployments(site.ID, 50)
	// Provisioning's release has its record too (W3).
	deps = slices.DeleteFunc(deps, func(d state.Deployment) bool { return d.ReleaseID == siteval.InitialRelease })
	if len(deps) != n {
		t.Fatalf("%d deployments recorded, want %d", len(deps), n)
	}
	// The agent was asked for exactly the IDs the API recorded.
	agent.mu.Lock()
	defer agent.mu.Unlock()
	asked := map[string]bool{}
	for _, call := range agent.history {
		if call.method == agentrpc.DeployRelease.Name() {
			asked[call.params.(rpc.DeployParams).ReleaseID] = true
		}
	}
	for _, d := range deps {
		if !seen[d.ReleaseID] || !asked[d.ReleaseID] {
			t.Errorf("deployment %q was not the release the API named and built", d.ReleaseID)
		}
	}
}
