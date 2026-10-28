package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// blockedOf decodes a block list answer.
func blockedOf(t *testing.T, b []byte) blocklistView {
	t.Helper()
	var v blocklistView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

// sentBlocklist is the last list the agent was asked to enforce.
func sentBlocklist(a *fakeAgent) rpc.BlocklistApplyParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, _ := a.lastParams[agentrpc.BlocklistApply.Name()].(rpc.BlocklistApplyParams)
	return p
}

// #21 (SPEC §7.4): failures from the agent and from the panel's sign-in
// block the address once they reach the threshold; the block is stored with
// its end and sent to the agent; an allowed address and this server are
// never blocked; with automatic blocking off nothing is.
func TestBlocklistAutomatic(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	s.localAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.10")} }
	ctx := context.Background()

	// The agent saw six SSH failures from one address, five from another.
	agent.mu.Lock()
	agent.results[agentrpc.BlocklistFailures.Name()] = rpc.BlocklistFailuresResult{SSHWatch: true, NginxWatch: true, Failures: []rpc.BlocklistFailure{
		{Address: "203.0.113.9", Source: "ssh", Count: 6},
		{Address: "203.0.113.10", Source: "wordpress", Count: 5},
		{Address: "192.0.2.10", Source: "ssh", Count: 50},
	}}
	agent.mu.Unlock()
	before := time.Now()
	s.blocklistRound(ctx)
	agent.mu.Lock()
	delete(agent.results, agentrpc.BlocklistFailures.Name())
	agent.mu.Unlock()

	_, b := admin.do("GET", "/api/blocklist", nil)
	v := blockedOf(t, b)
	if len(v.Blocked) != 1 || v.Blocked[0].Prefix != "203.0.113.9/32" || v.Blocked[0].Source != "ssh" || v.Blocked[0].ExpiresAt == nil {
		t.Fatalf("blocked = %+v", v.Blocked)
	}
	if end := v.Blocked[0].ExpiresAt.Sub(before); end < 59*time.Minute || end > 61*time.Minute {
		t.Fatalf("the block lasts %v, want the default hour", end)
	}
	if !v.Enforced || !v.Watching.SSH || !v.Watching.WordPress || v.Settings != blocklist.DefaultSettings() {
		t.Fatalf("view = %+v", v)
	}
	if sent := sentBlocklist(agent); !slices.Equal(sent.Block, []string{"203.0.113.9/32"}) {
		t.Fatalf("the agent was sent %+v", sent)
	}

	// One more WordPress failure blocks the second address: the five
	// before still count.
	s.recordFailure("203.0.113.10", blocklist.SourceWordPress, 1, time.Now())
	s.signInFailed("unix") // the API socket without nginx: nobody to block
	s.blocklistRound(ctx)
	// A block WordPress caused keeps the address from the sites only
	// (internal/blocklist SPEC §5): a site's code may mark any answer.
	if sent := sentBlocklist(agent); !slices.Equal(sent.Block, []string{"203.0.113.9/32"}) || !slices.Equal(sent.BlockWeb, []string{"203.0.113.10/32"}) {
		t.Fatalf("the agent was sent %+v", sent)
	}

	// The panel's own sign-in failures count too.
	for range 6 {
		s.signInFailed("2001:db8:5:6::99")
	}
	s.blocklistRound(ctx)
	if sent := sentBlocklist(agent); !slices.Contains(sent.Block, "2001:db8:5:6::/64") {
		t.Fatalf("the panel's failures did not block the /64: %+v", sent)
	}

	// An allowed address is never blocked, and allowing a blocked one ends
	// its automatic block.
	if resp, b := admin.do("POST", "/api/blocklist/allowed", map[string]any{"address": "203.0.113.0/28", "note": "office"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("allow = %d %s", resp.StatusCode, b)
	}
	sent := sentBlocklist(agent)
	if slices.Contains(sent.Block, "203.0.113.9/32") || !slices.Equal(sent.Allow, []string{"203.0.113.0/28"}) {
		t.Fatalf("after allowing, the agent was sent %+v", sent)
	}
	for range 10 {
		s.signInFailed("203.0.113.5")
	}
	if list, _ := s.Store.BlockEntries(); slices.ContainsFunc(list, func(e state.BlockEntry) bool { return e.Prefix == "203.0.113.5/32" }) {
		t.Fatal("an allowed address was blocked")
	}

	// Off: failures block nothing.
	if resp, b := admin.do("PUT", "/api/blocklist/settings", map[string]any{"enabled": false, "max_failures": 3, "window_minutes": 5, "ban_minutes": 30}); resp.StatusCode != http.StatusOK || blockedOf(t, b).Settings.Enabled {
		t.Fatalf("settings = %d %s", resp.StatusCode, b)
	}
	for range 10 {
		s.signInFailed("198.51.100.200")
	}
	if list, _ := s.Store.BlockEntries(); slices.ContainsFunc(list, func(e state.BlockEntry) bool { return e.Prefix == "198.51.100.200/32" }) {
		t.Fatal("blocked with automatic blocking off")
	}
	for _, bad := range []map[string]any{
		{"enabled": true, "max_failures": 2, "window_minutes": 5, "ban_minutes": 30},
		{"enabled": true, "max_failures": 5, "window_minutes": 0, "ban_minutes": 30},
		{"enabled": true, "max_failures": 5, "window_minutes": 5, "ban_minutes": 50000},
	} {
		if resp, _ := admin.do("PUT", "/api/blocklist/settings", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d", bad, resp.StatusCode)
		}
	}
}

// #21: blocks by hand, their refusals, their removal, an expiry, and a list
// the agent could not take.
func TestBlocklistManual(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	s.localAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.10")} }

	resp, b := admin.do("POST", "/api/blocklist/blocks", map[string]any{"address": "198.51.100.0/24", "note": "scanner"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("block = %d %s", resp.StatusCode, b)
	}
	v := blockedOf(t, b)
	if len(v.Blocked) != 1 || v.Blocked[0].Source != "manual" || v.Blocked[0].ExpiresAt != nil || v.Blocked[0].Note != "scanner" {
		t.Fatalf("blocked = %+v", v.Blocked)
	}
	if !slices.Equal(sentBlocklist(agent).Block, []string{"198.51.100.0/24"}) {
		t.Fatalf("sent %+v", sentBlocklist(agent))
	}
	resp, b = admin.do("POST", "/api/blocklist/blocks", map[string]any{"address": "203.0.113.7", "minutes": 30})
	if v := blockedOf(t, b); resp.StatusCode != http.StatusOK || v.Blocked[0].ExpiresAt == nil {
		t.Fatalf("timed block = %d %s", resp.StatusCode, b)
	}
	for _, bad := range []map[string]any{
		{"address": "not an address"},
		{"address": "10.0.0.0/7"},                      // too broad
		{"address": "127.0.0.0/16"},                    // loopback
		{"address": "192.0.2.0/24"},                    // this server
		{"address": "198.51.100.9", "minutes": -1},     // negative
		{"address": "198.51.100.9", "minutes": 600000}, // over a year
		{"address": "198.51.100.9", "note": "two\nlines"},
	} {
		if resp, b := admin.do("POST", "/api/blocklist/blocks", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, b)
		}
	}
	if resp, _ := admin.do("DELETE", "/api/blocklist/blocks", map[string]any{"address": "203.0.113.99"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unblocking an address not blocked = %d", resp.StatusCode)
	}
	if resp, b := admin.do("DELETE", "/api/blocklist/blocks", map[string]any{"address": "198.51.100.0/24"}); resp.StatusCode != http.StatusOK || len(blockedOf(t, b).Blocked) != 1 {
		t.Fatalf("unblock = %d %s", resp.StatusCode, b)
	}
	if slices.Contains(sentBlocklist(agent).Block, "198.51.100.0/24") {
		t.Fatal("the agent still blocks the removed network")
	}

	// An ended block goes at the next round.
	past := time.Now().Add(-time.Minute)
	if _, err := s.Store.PutBlockEntry(state.BlockEntry{Kind: state.BlockKindBlock, Prefix: "203.0.113.50/32", Source: "ssh", CreatedAt: past.Add(-time.Hour), ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	s.blocklistRound(context.Background())
	if list, _ := s.Store.BlockEntries(); slices.ContainsFunc(list, func(e state.BlockEntry) bool { return e.Prefix == "203.0.113.50/32" }) {
		t.Fatal("an ended block was not pruned")
	}

	// The agent cannot apply: the list is saved, and said not to be enforced.
	agent.mu.Lock()
	agent.errs[agentrpc.BlocklistApply.Name()] = agentrpc.Wrap(agentrpc.CodeUnavailable, "nftables is not installed on this server: apt install nftables", nil)
	agent.mu.Unlock()
	resp, b = admin.do("POST", "/api/blocklist/blocks", map[string]any{"address": "198.51.100.77"})
	v = blockedOf(t, b)
	if resp.StatusCode != http.StatusOK || v.Enforced || !strings.Contains(v.Error, "nftables") || len(v.Blocked) != 2 {
		t.Fatalf("with the agent failing = %d %s", resp.StatusCode, b)
	}

	// Reading is server-wide data; changing is a session's (an admin's).
	if resp, _ := admin.do("DELETE", "/api/blocklist/allowed", map[string]any{"address": "1.2.3.4"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removing an address not allowed = %d", resp.StatusCode)
	}
}
