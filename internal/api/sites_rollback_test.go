package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// R-3.13: a provisioning call that failed without an answer from the agent
// dropped the site's record anyway, while the agent had built the site (or
// was still building it): a site running that no page shows, and a domain
// that could never be created again. The record must stay, in error, for a
// delete to clean up; an agent's own refusal still drops it.
func TestCreateSiteFailureKeepsTheRecordWhenTheOutcomeIsUnknown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kept bool
	}{
		{"agent unreachable", &agentrpc.Error{Code: agentrpc.CodeUnavailable, Message: "The agent is not reachable"}, true},
		{"past the deadline", &agentrpc.Error{Code: agentrpc.CodeTimeout, Message: "timed out"}, true},
		{"refused", &agentrpc.Error{Code: agentrpc.CodeConflict, Message: "the site's directory already exists"}, false},
		{"failed and undone", &agentrpc.Error{Code: agentrpc.CodeInternal, Message: "Could not create the site"}, false},
		{"untyped", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, agent, ts := testServer(t)
			c := setupAdmin(t, s, ts)
			agent.errs[agentrpc.CreateSite.Name()] = tc.err
			resp, body := c.do("POST", "/api/sites", map[string]any{"domain": "lost.example.com", "type": "php"})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("create site = %d %s", resp.StatusCode, body)
			}
			waitForTasks(t, s)
			site, err := s.Store.GetSiteByDomain("lost.example.com")
			if !tc.kept {
				if err == nil {
					t.Fatalf("the record outlived a known failure (status %s)", site.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("the record was dropped: %v", err)
			}
			if site.Status != state.SiteError {
				t.Fatalf("status = %s, want %s", site.Status, state.SiteError)
			}
		})
	}
}

// R-3.13: a step after provisioning failed and the teardown the API then asked
// for failed too; the record was dropped regardless, leaving the site built.
func TestTeardownOfACreatedSiteKeepsTheRecordWhenItFails(t *testing.T) {
	s, agent, _ := testServer(t)
	site, err := s.Store.CreateSite(state.Site{Domain: "half.example.com", Type: state.SitePHP,
		SystemUser: "cg-site-1", RootPath: "/srv/sites/half.example.com", Status: state.SiteProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	agent.errs[agentrpc.DeleteSite.Name()] = &agentrpc.Error{Code: agentrpc.CodeUnavailable, Message: "The agent is not reachable"}
	s.teardownCreatedSite(site)
	got, err := s.Store.GetSiteByDomain("half.example.com")
	if err != nil {
		t.Fatalf("the record was dropped although the site is still there: %v", err)
	}
	if got.Status != state.SiteError {
		t.Fatalf("status = %s, want %s", got.Status, state.SiteError)
	}

	delete(agent.errs, agentrpc.DeleteSite.Name())
	s.teardownCreatedSite(got)
	if _, err := s.Store.GetSiteByDomain("half.example.com"); err == nil {
		t.Fatal("the record outlived a teardown that succeeded")
	}
}
