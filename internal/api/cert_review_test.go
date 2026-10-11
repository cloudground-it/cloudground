package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Back to Let's Encrypt from a custom certificate requests the certificate
// for the site's names as they are now: an alias added while the custom
// certificate was installed got no reissue (by design: it would replace the
// operator's certificate), and nothing asked for one afterwards, so the
// site went back to a certificate that did not cover it.
func TestBackToLetsEncryptIssuesForTheCurrentNames(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "certs.example.com")
	custom := rpc.CertStatusResult{Certs: map[string]rpc.CertInfo{site.Domain: {Exists: true, Custom: true, NotAfter: time.Now().Add(60 * 24 * time.Hour)}}}
	agent.setResult(agentrpc.CertStatus.Name(), custom)
	remove := func() map[string]any {
		resp, body := c.do("DELETE", fmt.Sprintf("/api/sites/%d/certificate/custom", site.ID), nil)
		var out map[string]any
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
			t.Fatalf("DELETE custom = %d %s", resp.StatusCode, body)
		}
		return out
	}

	// No ACME e-mail: nothing can be requested, and the answer says so.
	before := len(issueCalls(agent))
	if out := remove(); out["warning"] != msgNoACMEForLetsEncrypt || out["task"] != nil {
		t.Fatalf("without acme_email: %v", out)
	}
	waitForTasks(t, s)
	if n := len(issueCalls(agent)); n != before {
		t.Fatalf("%d certificate requests without an ACME e-mail", n-before)
	}

	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.ACMEEmail = "ops@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	// An alias added over the custom certificate: no reissue then.
	if code, _ := patchSite(t, c, site.ID, map[string]any{"aliases": []string{"www.certs.example.com"}}); code != http.StatusAccepted {
		t.Fatal(code)
	}
	waitForTasks(t, s)
	before = len(issueCalls(agent))

	out := remove()
	if out["task"] == nil || out["warning"] != nil {
		t.Fatalf("back to Let's Encrypt queued nothing: %v", out)
	}
	waitForTasks(t, s)
	calls := issueCalls(agent)
	if len(calls) != before+1 {
		t.Fatalf("%d certificate requests, want 1", len(calls)-before)
	}
	got := calls[len(calls)-1]
	if got.Site.Domain != site.Domain || !slices.Equal(got.Site.Aliases, []string{"www.certs.example.com"}) || got.Email != "ops@example.com" {
		t.Fatalf("issued for %s %v (%s), want the domain and its new alias", got.Site.Domain, got.Site.Aliases, got.Email)
	}
}
