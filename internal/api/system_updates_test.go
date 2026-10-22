package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// §7.5: the list is the agent's, kept for a while; a refresh and an
// upgrade are tasks under the apt lock, and an upgrade drops the list.
func TestSystemUpdates(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	pending := rpc.SystemUpdatesResult{Packages: []rpc.PendingUpdate{{Name: "libc6", From: "1", To: "2", Security: true}, {Name: "nginx", From: "1", To: "2"}},
		Security: 1, RebootRequired: true, RebootPackages: []string{"linux-image-generic"}}
	agent.setResult(agentrpc.SystemUpdates.Name(), pending)

	for range 2 {
		resp, body := c.do("GET", "/api/system/updates", nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"libc6"`) || !strings.Contains(string(body), `"reboot_required":true`) ||
			!strings.Contains(string(body), `"checked_at"`) {
			t.Fatalf("list = %d %s", resp.StatusCode, body)
		}
	}
	if n := countCalls(agent, agentrpc.SystemUpdates.Name()); n != 1 {
		t.Fatalf("the list asked the agent %d times, want once (kept)", n)
	}

	resp, body := c.do("POST", "/api/system/updates/refresh", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("refresh = %d %s", resp.StatusCode, body)
	}
	j := waitTaskEnd(t, s, taskOf(t, body).ID)
	p, _ := agent.last(agentrpc.SystemUpdates.Name()).(rpc.SystemUpdatesParams)
	if j.Status != state.JobSucceeded || !p.Refresh || len(j.Locks) != 1 || j.Locks[0] != aptLock {
		t.Fatalf("refresh job %s %q, locks %v, params %+v", j.Status, j.Error, j.Locks, p)
	}

	agent.setResult(agentrpc.ApplyUpdates.Name(), rpc.ApplyUpdatesResult{Upgraded: 1, Pending: rpc.SystemUpdatesResult{Packages: []rpc.PendingUpdate{}, RebootRequired: true}})
	resp, body = c.do("POST", "/api/system/updates/apply", map[string]bool{"security_only": true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	j = waitTaskEnd(t, s, taskOf(t, body).ID)
	ap, _ := agent.last(agentrpc.ApplyUpdates.Name()).(rpc.ApplyUpdatesParams)
	if j.Status != state.JobSucceeded || !ap.SecurityOnly {
		t.Fatalf("apply job %s %q, params %+v", j.Status, j.Error, ap)
	}
	resp, body = c.do("GET", "/api/system/updates", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "libc6") {
		t.Fatalf("after the upgrade the list is still the old one: %s", body)
	}
	events, _ := s.Store.ListAuditEvents(20)
	found := false
	for _, e := range events {
		found = found || (e.Action == "system.updates" && e.Detail == "security")
	}
	if !found {
		t.Fatal("no system.updates audit row")
	}
}
