package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// releasesOf is the Releases list as the API answers it: release -> status.
func releasesOf(t *testing.T, c *client, siteID int64) map[string]string {
	t.Helper()
	resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d/deployments", siteID), nil)
	var rows []struct {
		ReleaseID string `json:"release_id"`
		Status    string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &rows) != nil {
		t.Fatalf("deployments = %d %s", resp.StatusCode, body)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.ReleaseID] = r.Status
	}
	return out
}

// Provisioning's release is listed: live on a new site, and live again
// after a rollback to it. It had no record, so the list never showed it and
// after a rollback to it no row was live.
func TestInitialReleaseIsListed(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "initial.example.com")
	if got := releasesOf(t, c, site.ID); got[siteval.InitialRelease] != string(state.DeployPromoted) {
		t.Fatalf("a new site's releases: %v, want initial promoted", got)
	}

	// A site from before this fix: no record of its initial release. A
	// release is promoted over it, then rolled back to it.
	old := staticSiteRecord(t, s, "old.example.com")
	a, err := s.Store.CreateDeployment(state.Deployment{SiteID: old.ID, ReleaseID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Status: state.DeployPromoted})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetRollbackTo(a.ID, siteval.InitialRelease); err != nil {
		t.Fatal(err)
	}
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/rollback", old.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rollback = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	got := releasesOf(t, c, old.ID)
	if got[siteval.InitialRelease] != string(state.DeployPromoted) || got[a.ReleaseID] != string(state.DeployRolledBack) {
		t.Fatalf("after the rollback to initial: %v, want initial promoted and the other rolled back", got)
	}

	// The reconcile finding a site on its initial release records it too.
	other := staticSiteRecord(t, s, "other.example.com")
	s.syncLive(other, siteval.InitialRelease)
	if got := releasesOf(t, c, other.ID); got[siteval.InitialRelease] != string(state.DeployPromoted) {
		t.Fatalf("after the reconcile: %v", got)
	}
}
