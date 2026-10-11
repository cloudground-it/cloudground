package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #26: a path is protected, its password changed and its protection taken
// away; each change is rendered by the agent before it is stored, the hash
// never leaves the panel, and a refusal stores nothing.
func TestProtectedPaths(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "guarded.example.com", state.SiteWordPress, "8.4", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/protected-paths", site.ID)
	sentPaths := func() []state.ProtectedPath {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
		return ref.Site.Config.ProtectedPaths
	}

	resp, body := admin.do("POST", path, map[string]any{"path": "/wp-admin", "username": "boss", "password": "correct horse"})
	var got []state.ProtectedPath
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || len(got) != 1 || got[0].Path != "/wp-admin" || got[0].Username != "boss" || got[0].PasswordHash != "" {
		t.Fatalf("add = %d %s", resp.StatusCode, body)
	}
	sent := sentPaths()
	if len(sent) != 1 || bcrypt.CompareHashAndPassword([]byte(sent[0].PasswordHash), []byte("correct horse")) != nil {
		t.Fatalf("the agent rendered %+v", sent)
	}
	stored, _ := s.Store.GetSite(site.ID)
	if len(stored.Config.ProtectedPaths) != 1 || stored.Config.ProtectedPaths[0].PasswordHash != sent[0].PasswordHash {
		t.Fatalf("stored %+v", stored.Config.ProtectedPaths)
	}
	// The site as the API shows it carries no hash.
	if _, b := admin.do("GET", fmt.Sprintf("/api/sites/%d", site.ID), nil); strings.Contains(string(b), "$2a$") {
		t.Fatalf("GET site shows a password hash: %s", b)
	}

	// The same path again: new credentials, still one entry.
	if resp, body := admin.do("POST", path, map[string]any{"path": "/wp-admin", "username": "other", "password": "another one"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("replace = %d %s", resp.StatusCode, body)
	}
	if sent := sentPaths(); len(sent) != 1 || sent[0].Username != "other" {
		t.Fatalf("replace rendered %+v", sent)
	}

	for _, bad := range []map[string]any{
		{"path": "/", "username": "a", "password": "12345678"},
		{"path": "/wp-admin/../x", "username": "a", "password": "12345678"},
		{"path": "/x", "username": "a:b", "password": "12345678"},
		{"path": "/x", "username": "a", "password": "short"},
		{"path": "/x", "username": "a", "password": strings.Repeat("p", 73)},
	} {
		if resp, body := admin.do("POST", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, body)
		}
	}

	// An agent refusal stores nothing.
	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Invalid("nginx refused it")
	agent.mu.Unlock()
	if resp, _ := admin.do("POST", path, map[string]any{"path": "/private", "username": "a", "password": "12345678"}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.ProtectedPaths) != 1 {
		t.Fatalf("a refused render was stored: %+v", st.Config.ProtectedPaths)
	}

	if resp, _ := admin.do("DELETE", path, map[string]any{"path": "/nothing"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete of an unprotected path = %d", resp.StatusCode)
	}
	resp, body = admin.do("DELETE", path, map[string]any{"path": "/wp-admin"})
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.ProtectedPaths) != 0 || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("after delete %+v, generations %d/%d", st.Config.ProtectedPaths, st.AppliedGeneration, st.DesiredGeneration)
	}

	// A staging copy is protected as a whole.
	stg, err := s.Store.CreateSite(state.Site{Domain: "staging.guarded.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "stg", RootPath: "/srv/sites/staging.guarded.example.com", Status: state.SiteActive, StagingOf: site.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/protected-paths", stg.ID), map[string]any{"path": "/x", "username": "a", "password": "12345678"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("staging = %d %s", resp.StatusCode, body)
	}
}
