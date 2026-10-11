package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// activeSite is a static site, active, as the store holds it.
func activeSite(t *testing.T, s *Server, domain string) state.Site {
	t.Helper()
	site, err := s.Store.CreateSite(state.Site{Domain: domain, Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		SystemUser: "cg-site-1", RootPath: "/srv/sites/" + domain, Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// The re-warm after a change is a quiet job, not a goroutine: it is
// recorded, joined while one is queued, and hidden from the task list.
func TestWarmAfterChangeIsAQuietJob(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := activeSite(t, s, "warm.example.com")
	agent.setHook(func(method string) {
		if method == agentrpc.WarmCache.Name() {
			time.Sleep(50 * time.Millisecond)
		}
	})
	s.warmInBackground(site)
	s.warmInBackground(site)
	waitForTasks(t, s)
	jobs, err := s.Store.ListJobs(20, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, j := range jobs {
		if j.Kind == "cache.rewarm" {
			n++
			if !j.Quiet || j.Status != state.JobSucceeded || j.SiteID != site.ID {
				t.Fatalf("re-warm job %+v", j)
			}
		}
	}
	if n < 1 || n > 2 {
		t.Fatalf("%d re-warm jobs", n)
	}
	if !agent.called(agentrpc.WarmCache.Name()) {
		t.Fatal("the site was not warmed")
	}
	_, body := c.do("GET", "/api/tasks", nil)
	if strings.Contains(string(body), "cache.rewarm") {
		t.Fatalf("the re-warm is listed as a task: %s", body)
	}
}

// The self-update's audit row names what was asked for, "latest" for the
// interface's empty request (it was empty).
func TestSelfUpdateAuditNamesTheVersion(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	agent.results[agentrpc.SelfUpdate.Name()] = rpc.SelfUpdateResult{UpdatedTo: "v9.9.9"}
	for body, want := range map[string]string{`{}`: "latest", `{"version":"v9.9.9"}`: "v9.9.9"} {
		var req map[string]string
		_ = json.Unmarshal([]byte(body), &req)
		if resp, out := c.do("POST", "/api/panel/update", req); resp.StatusCode != http.StatusOK {
			t.Fatalf("update %s = %d %s", body, resp.StatusCode, out)
		}
		rows, _ := s.Store.ListAuditEvents(1)
		if len(rows) != 1 || rows[0].Action != "panel.update" || rows[0].Subject != want {
			t.Fatalf("audit after %s = %+v, want subject %q", body, rows, want)
		}
	}
}
