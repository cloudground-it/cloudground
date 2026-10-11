package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// R-5.4: the routing block reaches the agent with the site as stored now,
// is stored once the agent has applied it, and never when it refuses.
func TestSiteRouting(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "routing.example.com")
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.RootPath = "/srv/sites/routing.example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/sites/%d/routing", site.ID)
	const def = "location / {\n    try_files $uri $uri/ /index.php?$args;\n}\n"
	const slim = "location / {\n    try_files $uri /app.php$is_args$args;\n}\n"

	agent.mu.Lock()
	agent.results[agentrpc.SiteRouting.Name()] = rpc.RoutingView{Default: def, Before: "server {\n", After: "}\n"}
	agent.mu.Unlock()
	resp, body := c.do("GET", base, nil)
	var v routingView
	_ = json.Unmarshal(body, &v)
	if resp.StatusCode != http.StatusOK || v.Routing != def || v.Custom || !v.Editable || v.Before != "server {\n" {
		t.Fatalf("view = %d %s", resp.StatusCode, body)
	}

	agent.mu.Lock()
	agent.results[agentrpc.SetSiteRouting.Name()] = rpc.RoutingResult{Routing: slim, Files: []rpc.ManagedFile{{Path: "/etc/nginx/sites-enabled/routing.example.com.conf", SHA256: "ab"}}}
	agent.mu.Unlock()
	if resp, body := c.do("PUT", base, map[string]any{"routing": strings.ReplaceAll(slim, "\n", "\r\n")}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set = %d %s", resp.StatusCode, body)
	}
	p, ok := agent.last(agentrpc.SetSiteRouting.Name()).(rpc.RoutingParams)
	if !ok || p.Routing != slim || p.Site.ID != site.ID {
		t.Fatalf("agent got %+v", agent.last(agentrpc.SetSiteRouting.Name()))
	}
	got, _ := s.Store.GetSite(site.ID)
	if got.Config.Routing != slim {
		t.Fatalf("stored %q", got.Config.Routing)
	}
	if files, _ := s.Store.ManagedFiles(); files["/etc/nginx/sites-enabled/routing.example.com.conf"] != "ab" {
		t.Errorf("the rendered vhost's hash is not recorded: %v", files)
	}

	// A refused directive never reaches the agent.
	calls := len(agent.calls)
	resp, body = c.do("PUT", base, map[string]any{"routing": "location / { include /etc/passwd; }"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "include") {
		t.Fatalf("include = %d %s", resp.StatusCode, body)
	}
	if len(agent.calls) != calls {
		t.Fatal("a refused routing reached the agent")
	}
	if resp, _ := c.do("PUT", base, map[string]any{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no routing = %d", resp.StatusCode)
	}

	// nginx refusing it (the agent's answer) leaves the stored one.
	agent.mu.Lock()
	agent.errs[agentrpc.SetSiteRouting.Name()] = agentrpc.Invalid("nginx refused the routing, nothing was changed: unknown directive")
	agent.mu.Unlock()
	resp, body = c.do("PUT", base, map[string]any{"routing": "location / { expires notatime; }"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "nginx refused") {
		t.Fatalf("refused = %d %s", resp.StatusCode, body)
	}
	if got, _ := s.Store.GetSite(site.ID); got.Config.Routing != slim {
		t.Fatalf("a refused routing changed the stored one: %q", got.Config.Routing)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.SetSiteRouting.Name())
	agent.results[agentrpc.SetSiteRouting.Name()] = rpc.RoutingResult{}
	agent.mu.Unlock()
	if resp, body := c.do("PUT", base, map[string]any{"routing": ""}); resp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d %s", resp.StatusCode, body)
	}
	if got, _ := s.Store.GetSite(site.ID); got.Config.Routing != "" {
		t.Fatalf("restore stored %q", got.Config.Routing)
	}
}

// The routing is an administrator's, from a session: an API token, an
// admin's included, gets 403, and so does an operator of the site.
func TestSiteRoutingIsAnAdminsSession(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createWPSite(t, s, admin, "tokrouting.example.com")
	base := fmt.Sprintf("/api/sites/%d/routing", site.ID)
	tok := mintToken(t, admin, "script", 0)
	if code, b := bearer(t, ts, tok.Token, "PUT", base, map[string]any{"routing": ""}); code != http.StatusForbidden {
		t.Fatalf("token PUT = %d %s", code, b)
	}
	if code, b := bearer(t, ts, tok.Token, "GET", base, nil); code != http.StatusForbidden {
		t.Fatalf("token GET = %d %s", code, b)
	}
	if resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	if resp, body := op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator login = %d %s", resp.StatusCode, body)
	}
	if resp, _ := op.do("PUT", base, map[string]any{"routing": ""}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator PUT = %d", resp.StatusCode)
	}
	if agent.called(agentrpc.SetSiteRouting.Name()) || agent.called(agentrpc.SiteRouting.Name()) {
		t.Fatal("a refused caller reached the agent")
	}
}

// Two saves at once are applied one after the other, each on the site as
// the other left it: the site's lock is held across the agent's render.
func TestSiteRoutingSavesAreSerialised(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "race.example.com")
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.RootPath = "/srv/sites/race.example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const stored = "location / {\n    try_files $uri /app.php$is_args$args;\n}\n"
	agent.mu.Lock()
	agent.results[agentrpc.SetSiteRouting.Name()] = rpc.RoutingResult{Routing: stored}
	agent.mu.Unlock()
	var mu sync.Mutex
	inFlight, most := 0, 0
	agent.setHook(func(method string) {
		if method != agentrpc.SetSiteRouting.Name() {
			return
		}
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := c.do("PUT", fmt.Sprintf("/api/sites/%d/routing", site.ID), map[string]any{"routing": stored})
			codes[i] = resp.StatusCode
		}()
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("codes %v", codes)
	}
	if most != 1 {
		t.Fatalf("%d saves reached the agent at once", most)
	}
	var seen []string
	agent.mu.Lock()
	for _, h := range agent.history {
		if p, ok := h.params.(rpc.RoutingParams); ok {
			seen = append(seen, p.Site.Config.Routing)
		}
	}
	agent.mu.Unlock()
	if len(seen) != 2 || seen[0] != "" || seen[1] != stored {
		t.Fatalf("the second save did not see the first's result: %q", seen)
	}
}

// The site's routing block is in its record only for those who may read
// GET /routing: an administrator's session. An operator of the site, a
// read-only account and an admin's API token get the site without it, in
// the list and one by one (SPEC.md §3.1).
func TestSiteRecordCarriesRoutingOnlyToAnAdminsSession(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createWPSite(t, s, admin, "hidden-routing.example.com")
	const block = "location /secret-upstream { proxy_pass http://10.9.8.7:8080; }\n"
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Config.Routing = block
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hash, _ := accounts.HashPassword("viewer-password!")
	if _, err := s.Store.CreateUser("viewer@example.com", hash, "readonly"); err != nil {
		t.Fatal(err)
	}
	if resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	viewer := signIn(t, ts, "viewer@example.com", "viewer-password!")
	op := signIn(t, ts, "op@example.com", "operator-password!")
	tok := mintToken(t, admin, "script", 0)

	one := fmt.Sprintf("/api/sites/%d", site.ID)
	for _, p := range []string{"/api/sites", one} {
		if resp, body := admin.do("GET", p, nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "10.9.8.7") {
			t.Errorf("admin session GET %s = %d, routing missing: %s", p, resp.StatusCode, body)
		}
		for name, c := range map[string]*client{"read-only": viewer, "operator": op} {
			resp, body := c.do("GET", p, nil)
			if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "10.9.8.7") || strings.Contains(string(body), `"routing"`) {
				t.Errorf("%s GET %s = %d, routing shown: %s", name, p, resp.StatusCode, body)
			}
		}
		code, body := bearer(t, ts, tok.Token, "GET", p, nil)
		if code != http.StatusOK || strings.Contains(string(body), "10.9.8.7") || strings.Contains(string(body), `"routing"`) {
			t.Errorf("API token GET %s = %d, routing shown: %s", p, code, body)
		}
	}
	// A PATCH answer carries the record too.
	resp, body := op.do("PATCH", one, map[string]any{"cache_ttl_sec": 600})
	if resp.StatusCode != http.StatusAccepted || strings.Contains(string(body), "10.9.8.7") {
		t.Errorf("operator PATCH answer = %d, routing shown: %s", resp.StatusCode, body)
	}
}
