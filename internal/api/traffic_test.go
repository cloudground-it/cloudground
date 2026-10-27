package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/traffic"
)

// #33 (SPEC §11.8): the collect job sends every site; the route asks the
// agent for the site's range and checks the range.
func TestTraffic(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	a := seedActiveSite(t, s, "one.example.com", state.SiteStatic, "", 0)
	b := seedActiveSite(t, s, "two.example.com", state.SiteWordPress, "8.4", 0)

	runJobNow(t, s, jobs.Spec{Kind: "traffic.collect"})
	agent.mu.Lock()
	p, _ := agent.lastParams[agentrpc.TrafficCollect.Name()].(rpc.TrafficCollectParams)
	agent.mu.Unlock()
	if fmt.Sprint(p.Sites) != fmt.Sprintf("[{%d one.example.com} {%d two.example.com}]", a.ID, b.ID) {
		t.Fatalf("collect sent %v", p.Sites)
	}

	agent.setResult(agentrpc.TrafficStats.Name(), rpc.TrafficStatsResult{Stats: traffic.Stats{Totals: traffic.Counts{Pageviews: 42}}})
	path := fmt.Sprintf("/api/sites/%d/traffic", a.ID)
	resp, body := admin.do("GET", path+"?days=30", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"pageviews":42`) {
		t.Fatalf("traffic = %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	sp, _ := agent.lastParams[agentrpc.TrafficStats.Name()].(rpc.TrafficStatsParams)
	agent.mu.Unlock()
	if sp.ID != a.ID || sp.Domain != "one.example.com" || sp.Days != 30 {
		t.Fatalf("stats asked %+v", sp)
	}
	if resp, _ := admin.do("GET", path, nil); resp.StatusCode != http.StatusOK {
		t.Fatal("no range")
	}
	agent.mu.Lock()
	sp, _ = agent.lastParams[agentrpc.TrafficStats.Name()].(rpc.TrafficStatsParams)
	agent.mu.Unlock()
	if sp.Days != 7 {
		t.Fatalf("default range %d", sp.Days)
	}
	if resp, _ := admin.do("GET", path+"?days=5", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("days=5 = %d", resp.StatusCode)
	}
}
