package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Simulate (ops/dbstudio SPEC §12) as a signed-in user: as that user,
// audited database.simulate, redacted, before the call; the agent's answer
// as is.
func TestStudioSimulate(t *testing.T) {
	s, agent, anon, site := loginSetup(t)
	db := site.Config.Database.Name
	base := "/api/quarry/db/" + db
	agent.setResult(agentrpc.DBStudioSimulate.Name(), rpc.DBStudioSimulateResult{Kind: "update", Affected: 4, RolledBack: true})
	resp, _ := signInStudio(anon, "reader", "reader-password-1")
	sc := &client{t: t, base: anon.base, cookies: []*http.Cookie{cookieOf(resp, insecureStudioCookie)}}
	const stmt = "UPDATE wp_posts SET post_title = 'secret-value' WHERE ID < 20"
	resp, body := sdo(sc, "POST", base+"/simulate", map[string]any{"sql": stmt})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"affected":4`) || !strings.Contains(string(body), `"rolled_back":true`) {
		t.Fatalf("simulate: %d %s", resp.StatusCode, body)
	}
	p := agent.last(agentrpc.DBStudioSimulate.Name()).(rpc.DBStudioSimulateParams)
	if p.Login != strings.Repeat("ab", 32) || p.SQL != stmt || p.Database != db || p.ReadOnly {
		t.Fatalf("simulate sent %+v", p)
	}
	if resp, _ := sdo(sc, "POST", base+"/simulate", map[string]any{"sql": "  "}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty statement: %d", resp.StatusCode)
	}
	events, _ := s.Store.ListAuditEvents(10)
	found := false
	for _, e := range events {
		if strings.Contains(e.Detail, "secret-value") {
			t.Fatalf("the statement is in the audit: %+v", e)
		}
		found = found || (e.Action == "database.simulate" && e.Actor == "reader (database user)" && strings.HasPrefix(e.Detail, "db="+db+" UPDATE"))
	}
	if !found {
		t.Fatalf("a simulation is not audited: %+v", events)
	}
}

// From the panel: a manager simulates without allowing writes, on a write
// account the agent rolls back; a read-only account may not.
func TestStudioSimulateFromThePanel(t *testing.T) {
	_, agent, admin, ro, site := dbStudioSetup(t)
	db := site.Config.Database.Name
	base := "/api/quarry/db/" + db
	agent.setResult(agentrpc.DBStudioSimulate.Name(), rpc.DBStudioSimulateResult{Refused: "wp_log uses the MyISAM engine, which does not roll back"})
	as := openStudio(t, admin, site.ID)
	resp, body := sdo(as, "POST", base+"/simulate", map[string]any{"sql": "DELETE FROM wp_log"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "MyISAM") {
		t.Fatalf("simulate: %d %s", resp.StatusCode, body)
	}
	if p := agent.last(agentrpc.DBStudioSimulate.Name()).(rpc.DBStudioSimulateParams); p.ReadOnly || p.Login != "" || p.Database != db {
		t.Fatalf("simulate sent %+v", p)
	}
	agent.forget()
	rs := openStudio(t, ro, site.ID)
	if resp, _ := sdo(rs, "POST", base+"/simulate", map[string]any{"sql": "DELETE FROM wp_log"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a read-only account simulated: %d", resp.StatusCode)
	}
	if agent.called(agentrpc.DBStudioSimulate.Name()) {
		t.Fatal("a refused simulation reached the agent")
	}
}
