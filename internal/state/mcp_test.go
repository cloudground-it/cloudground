package state

import (
	"errors"
	"testing"
	"time"
)

func mcpTokens(tag string, at time.Time) MCPTokens {
	return MCPTokens{AccessHash: "a-" + tag, RefreshHash: "r-" + tag, AccessExpires: at.Add(time.Hour), RefreshExpires: at.Add(30 * 24 * time.Hour)}
}

func TestMCPGrantLifecycle(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	c, err := s.AddMCPClient(MCPClient{ID: "cgc_1", Name: "Claude", RedirectURIs: []string{"https://claude.ai/cb"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetMCPClient(c.ID); err != nil || got.RedirectURIs[0] != "https://claude.ai/cb" {
		t.Fatalf("GetMCPClient = %+v, %v", got, err)
	}
	now := time.Now()
	g, err := s.CreateMCPGrant(u.ID, c.ID, `{"write":{}}`, mcpTokens("1", now))
	if err != nil || g.ClientName != "Claude" || g.Email != u.Email {
		t.Fatalf("CreateMCPGrant = %+v, %v", g, err)
	}
	if _, err := s.MCPGrantByAccess("a-1", now); err != nil {
		t.Fatalf("by access: %v", err)
	}
	if _, err := s.MCPGrantByAccess("a-1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired access token: %v", err)
	}
	// Rotation: the new pair works, the old access token does not, the old
	// refresh token presented again revokes the grant.
	if _, err := s.RotateMCPGrant("r-1", c.ID, now, mcpTokens("2", now)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.MCPGrantByAccess("a-1", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("the rotated access token still works")
	}
	if _, err := s.RotateMCPGrant("r-2", "cgc_other", now, mcpTokens("x", now)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another client's refresh: %v", err)
	}
	if _, err := s.RotateMCPGrant("r-1", c.ID, now, mcpTokens("3", now)); !errors.Is(err, ErrMCPRefreshReused) {
		t.Fatalf("a reused refresh token: %v", err)
	}
	if _, err := s.MCPGrantByAccess("a-2", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("the grant survived a reused refresh token")
	}
}

func TestMCPGrantsEndWithAPITokensAndTheSwitch(t *testing.T) {
	s, _ := newStore(t)
	u := newPasskeyUser(t, s, "a@example.com")
	if _, err := s.AddMCPClient(MCPClient{ID: "cgc_1", Name: "C", RedirectURIs: []string{"https://x/cb"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	g, _ := s.CreateMCPGrant(u.ID, "cgc_1", `{}`, mcpTokens("1", now))
	a, err := s.AddMCPAction(MCPAction{GrantID: g.ID, UserID: u.ID, Tool: "restore_backup", Args: `{}`, Summary: "s", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)})
	if err != nil || a.Status != "pending" || a.ClientName != "C" {
		t.Fatalf("AddMCPAction = %+v, %v", a, err)
	}
	if late, _ := s.GetMCPAction(a.ID, now.Add(time.Hour)); late.Status != "expired" {
		t.Fatalf("a late action reads %q", late.Status)
	}
	if ok, _ := s.DecideMCPAction(a.ID, now.Add(time.Hour), "done", ""); ok {
		t.Fatal("an expired action was decided")
	}
	if ok, _ := s.DecideMCPAction(a.ID, now, "rejected", ""); !ok {
		t.Fatal("a pending action could not be decided")
	}
	if ok, _ := s.DecideMCPAction(a.ID, now, "done", ""); ok {
		t.Fatal("an action decided twice")
	}
	// A password reset ends the grants with the API tokens.
	if err := s.SetUserPassword(u.ID, "argon2id$x$z"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListMCPGrants(0); len(list) != 0 {
		t.Fatalf("grants after a password reset: %+v", list)
	}
	s.CreateMCPGrant(u.ID, "cgc_1", `{}`, mcpTokens("2", now))
	if err := s.ClearMCP(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMCPClient("cgc_1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a client survived ClearMCP")
	}
	if list, _ := s.ListMCPGrants(u.ID); len(list) != 0 {
		t.Fatal("a grant survived ClearMCP")
	}
}

func TestMCPEnabledIsAPanelSetting(t *testing.T) {
	s, _ := newStore(t)
	if p, _ := s.PanelSettings(); p.MCPEnabled {
		t.Fatal("the MCP server is on by default")
	}
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error { p.MCPEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PanelSettings(); !p.MCPEnabled {
		t.Fatal("the switch did not stay on")
	}
}
