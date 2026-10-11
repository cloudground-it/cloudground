package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// #12: the bounds of signed-in Quarry sessions are the agent's own
// (rpc.DBStudioMaxSiteLogins, rpc.DBStudioMaxLogins), and the API refuses
// a sign-in past either with its reason before the agent keeps a session,
// which would otherwise end one the API still holds. The last one that
// fits signs in; past it the password is still checked (a wrong one reads
// as any refusal) and nothing is kept.
func TestStudioSignInBoundsAreTheAgents(t *testing.T) {
	s, agent, anon, site := loginSetup(t)
	at := s.studio.clock()
	fill := func(siteID int64, n int) {
		for i := range n {
			st := &studioSession{key: fmt.Sprintf("fill-%d-%d", siteID, i), mode: studioLogin, siteID: siteID, started: at, used: at}
			s.studio.add(st)
		}
	}
	logins := func() (n int) {
		s.studio.mu.Lock()
		defer s.studio.mu.Unlock()
		for _, x := range s.studio.byKey {
			if x.mode == studioLogin {
				n++
			}
		}
		return n
	}
	checkOnly := func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		p, _ := agent.lastParams[agentrpc.DBStudioOpen.Name()].(rpc.DBStudioOpenParams)
		return p.CheckOnly
	}

	// The last session that fits on the site.
	fill(site.ID, rpc.DBStudioMaxSiteLogins-1)
	if resp, body := signInStudio(anon, "reader", "reader-password-1"); resp.StatusCode != http.StatusOK || checkOnly() {
		t.Fatalf("sign-in %d of the site = %d %s (check only %v)", rpc.DBStudioMaxSiteLogins, resp.StatusCode, body, checkOnly())
	}
	if n := logins(); n != rpc.DBStudioMaxSiteLogins {
		t.Fatalf("%d signed-in sessions, want %d", n, rpc.DBStudioMaxSiteLogins)
	}

	// One past it: refused with the reason, the password checked only.
	resp, body := signInStudio(anon, "reader", "reader-password-1")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), fmt.Sprintf("%d signed-in sessions on this site", rpc.DBStudioMaxSiteLogins)) {
		t.Fatalf("sign-in past the site's bound = %d %s", resp.StatusCode, body)
	}
	if !checkOnly() || logins() != rpc.DBStudioMaxSiteLogins {
		t.Fatalf("past the bound: check only %v, %d sessions", checkOnly(), logins())
	}
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Refused: true})
	if resp, body := signInStudio(anon, "reader", "wrong-password"); resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), "signed-in sessions") {
		t.Fatalf("a wrong password on a full site = %d %s, want the usual 401", resp.StatusCode, body)
	}

	// The bound in all, from sessions of other sites.
	s.studio.mu.Lock()
	clear(s.studio.byKey)
	s.studio.mu.Unlock()
	for i := range rpc.DBStudioMaxLogins / rpc.DBStudioMaxSiteLogins {
		fill(int64(1000+i), rpc.DBStudioMaxSiteLogins)
	}
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Login: strings.Repeat("cd", 32)})
	resp, body = signInStudio(anon, "reader", "reader-password-1")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), fmt.Sprintf("%d signed-in sessions:", rpc.DBStudioMaxLogins)) || !checkOnly() {
		t.Fatalf("sign-in past the bound in all = %d %s (check only %v)", resp.StatusCode, body, checkOnly())
	}
}
