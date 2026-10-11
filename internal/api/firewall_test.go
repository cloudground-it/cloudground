package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// POST /api/firewall/enable (#14) needs the plan the operator was shown,
// hands it to the agent, which decides, and audits what was allowed.
func TestFirewallEnable(t *testing.T) {
	s, fa, ts := testServer(t)
	admin := setupAdmin(t, s, ts)

	if resp, body := admin.do("POST", "/api/firewall/enable", map[string]any{"expect": []string{}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no plan: %d %s", resp.StatusCode, body)
	}
	if fa.called(agentrpc.FirewallEnable.Name()) {
		t.Fatal("the agent was asked without a plan")
	}

	plan := []string{"2222/tcp", "80/tcp", "443/tcp", "443/udp", "8443/tcp"}
	fa.mu.Lock()
	if fa.results == nil {
		fa.results = map[string]any{}
	}
	fa.results[agentrpc.FirewallEnable.Name()] = rpc.FirewallStatusResult{Enabled: true, Rules: []string{"2222/tcp ALLOW Anywhere"}}
	fa.mu.Unlock()
	resp, body := admin.do("POST", "/api/firewall/enable", map[string]any{"expect": plan})
	var got rpc.FirewallStatusResult
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || !got.Enabled {
		t.Fatalf("enable: %d %s", resp.StatusCode, body)
	}
	fa.mu.Lock()
	sent, _ := fa.lastParams[agentrpc.FirewallEnable.Name()].(rpc.FirewallEnableParams)
	fa.mu.Unlock()
	if !reflect.DeepEqual(sent.Expect, plan) {
		t.Fatalf("the agent got %+v", sent)
	}
	events, _ := s.Store.ListAuditEvents(10)
	found := false
	for _, e := range events {
		if e.Action == "firewall.enable" && e.Detail == "allowed 2222/tcp 80/tcp 443/tcp 443/udp 8443/tcp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no firewall.enable audit: %+v", events)
	}
}
