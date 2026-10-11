package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// stagingIssues is the certificates asked for the staging name.
func stagingIssues(agent *fakeAgent, domain string) int {
	n := 0
	for _, c := range issueCalls(agent) {
		if c.Site.Domain == domain {
			n++
		}
	}
	return n
}

// lastTask is the newest task of kind.
func lastTask(t *testing.T, c *client, kind string) (status, log string) {
	t.Helper()
	resp, body := c.do("GET", "/api/tasks", nil)
	var tasks []struct{ Kind, Status, Log string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tasks) != nil {
		t.Fatalf("tasks = %d %s", resp.StatusCode, body)
	}
	for _, tk := range tasks {
		if tk.Kind == kind {
			return tk.Status, tk.Log
		}
	}
	t.Fatalf("no %s task: %s", kind, body)
	return "", ""
}

// No diff while the copy is being made: its tree is partial, and the diff
// listed most of production as removed (1001 files on the test box).
func TestStagingDiffWaitsForTheClone(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	prod := staticSiteRecord(t, s, "shop.example.com")
	stg, err := s.Store.CreateSite(state.Site{Domain: "staging.shop.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-2", RootPath: "/srv/sites/staging.shop.example.com", Status: state.SiteProvisioning, StagingOf: prod.ID})
	if err != nil {
		t.Fatal(err)
	}
	diff := func() (int, string) {
		resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d/staging/diff", prod.ID), nil)
		return resp.StatusCode, string(body)
	}
	if code, body := diff(); code != http.StatusConflict || !strings.Contains(body, msgStagingCloning) {
		t.Fatalf("diff of a provisioning copy = %d %s", code, body)
	}
	if agent.called(agentrpc.StagingDiff.Name()) {
		t.Fatal("the agent was asked for a diff of a partial copy")
	}
	if err := s.setSiteStatus(stg.ID, state.SiteActive); err != nil {
		t.Fatal(err)
	}
	if code, body := diff(); code != http.StatusOK {
		t.Fatalf("diff of a complete copy = %d %s", code, body)
	}

	// A reclone removes the old tree while the record still reads active.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	agent.setHook(func(method string) {
		if method == agentrpc.DeleteSite.Name() {
			entered <- struct{}{}
			<-release
		}
	})
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/staging/recreate", prod.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("recreate = %d %s", resp.StatusCode, body)
	}
	<-entered
	code, body := diff()
	close(release)
	agent.setHook(nil)
	if code != http.StatusConflict {
		t.Fatalf("diff during a reclone = %d %s", code, body)
	}
	waitForTasks(t, s)
	if code, body := diff(); code != http.StatusOK {
		t.Fatalf("diff after the reclone = %d %s", code, body)
	}
}

// Creating and recreating a staging copy request the certificate of
// staging.<domain>, like a new site, when an ACME e-mail is set; a failed
// request (DNS not pointing here yet) is a line in the job's log, not its
// failure. The copy used to stay on the default certificate for ever.
func TestStagingRequestsItsCertificate(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	prod := staticSiteRecord(t, s, "shop.example.com")
	stg := "staging.shop.example.com"

	// Without an ACME e-mail nothing is asked.
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/staging", prod.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create staging = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if n := stagingIssues(agent, stg); n != 0 {
		t.Fatalf("%d certificate requests without an ACME e-mail", n)
	}

	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.ACMEEmail = "ops@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/staging/recreate", prod.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("recreate staging = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if n := stagingIssues(agent, stg); n != 1 {
		t.Fatalf("recreate: %d certificate requests for %s, want 1", n, stg)
	}
	if status, _ := lastTask(t, c, "staging.recreate"); status != "succeeded" {
		t.Fatalf("recreate %s", status)
	}

	// DNS not pointing here: the clone still succeeds and says why.
	agent.setErr(agentrpc.IssueCert.Name(), errors.New("simulated: DNS problem: NXDOMAIN"))
	gone, err := s.Store.StagingSiteFor(prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := c.do("DELETE", fmt.Sprintf("/api/sites/%d", gone.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete staging = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/staging", prod.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create staging again = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if n := stagingIssues(agent, stg); n != 2 {
		t.Fatalf("create: %d certificate requests for %s, want 2", n, stg)
	}
	status, log := lastTask(t, c, "staging.create")
	if status != "succeeded" || !strings.Contains(log, "Certificate pending (DNS may not point here yet)") {
		t.Fatalf("create with ACME failing: %s, log:\n%s", status, log)
	}
}
