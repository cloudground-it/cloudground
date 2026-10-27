package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const dbsPath = "/api/db-servers"

func newServer(t *testing.T, c *client, agent *fakeAgent, name string) dbServerView {
	t.Helper()
	agent.mu.Lock()
	agent.results[agentrpc.TestDBServer.Name()] = rpc.DBServerTestResult{Version: "10.11.6-MariaDB"}
	agent.mu.Unlock()
	resp, body := c.do("POST", dbsPath, map[string]any{"name": name, "host": "db.example.com", "tls": "required",
		"admin_user": "cgadmin", "admin_password": "s3cret-admin-pw"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	var v dbServerView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// §6.5.1: registering a server tests it first, seals the password, hands the
// agent the list and never shows the password again.
func TestRegisterDBServer(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)

	// A refusal of the agent is 400 with its words, and nothing is stored.
	agent.mu.Lock()
	agent.errs[agentrpc.TestDBServer.Name()] = agentrpc.Invalid("could not connect to db.example.com as cgadmin: access denied")
	agent.mu.Unlock()
	resp, body := admin.do("POST", dbsPath, map[string]any{"name": "managed", "host": "db.example.com", "admin_user": "cgadmin", "admin_password": "wrong-pw"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "access denied") {
		t.Fatalf("refused server: %d %s", resp.StatusCode, body)
	}
	if list, _ := s.Store.ListDBServers(); len(list) != 0 {
		t.Fatalf("a refused server was stored: %+v", list)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.TestDBServer.Name())
	agent.mu.Unlock()

	for name, req := range map[string]map[string]any{
		"no name":      {"host": "h.example.com", "admin_user": "a", "admin_password": "p"},
		"a bad name":   {"name": "Bad_Name", "host": "h.example.com", "admin_user": "a", "admin_password": "p"},
		"no host":      {"name": "managed", "admin_user": "a", "admin_password": "p"},
		"a host path":  {"name": "managed", "host": "h.example.com/x", "admin_user": "a", "admin_password": "p"},
		"a bad port":   {"name": "managed", "host": "h.example.com", "port": 70000, "admin_user": "a", "admin_password": "p"},
		"a bad tls":    {"name": "managed", "host": "h.example.com", "tls": "maybe", "admin_user": "a", "admin_password": "p"},
		"no user":      {"name": "managed", "host": "h.example.com", "admin_password": "p"},
		"no password":  {"name": "managed", "host": "h.example.com", "admin_user": "a"},
		"a bad CA":     {"name": "managed", "host": "h.example.com", "admin_user": "a", "admin_password": "p", "ca_cert": "not a certificate"},
		"a line break": {"name": "managed", "host": "h.example.com", "admin_user": "a", "admin_password": "p\nq"},
	} {
		if resp, body := admin.do("POST", dbsPath, req); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, body)
		}
	}

	v := newServer(t, admin, agent, "managed")
	if v.ID == 0 || v.Port != 3306 || v.TLS != "required" || !v.PasswordSet || v.Kind != "mariadb" {
		t.Fatalf("%+v", v)
	}
	if strings.Contains(fmt.Sprint(v), "s3cret") {
		t.Fatal("the password is in the view")
	}
	// The agent was asked to connect with it, then given the list with it.
	agent.mu.Lock()
	test, _ := agent.lastParams[agentrpc.TestDBServer.Name()].(rpc.DBServerParams)
	set, _ := agent.lastParams[agentrpc.SetDBServers.Name()].(rpc.SetDBServersParams)
	agent.mu.Unlock()
	if test.AdminPassword != "s3cret-admin-pw" || test.Host != "db.example.com" || !test.TLS || test.Port != 3306 {
		t.Fatalf("TestDBServer params %+v", test)
	}
	if len(set.Servers) != 1 || set.Servers[0].ID != v.ID || set.Servers[0].AdminPassword != "s3cret-admin-pw" {
		t.Fatalf("SetDBServers params %+v", set)
	}
	if pw, _ := s.Store.DBServerPassword(v.ID); pw != "s3cret-admin-pw" {
		t.Fatal("the password is not stored sealed")
	}
	// The name is unique.
	resp, _ = admin.do("POST", dbsPath, map[string]any{"name": "managed", "host": "h2.example.com", "admin_user": "a", "admin_password": "pw"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second server of the same name: %d", resp.StatusCode)
	}

	// The list; a change keeps the password when it is masked or left out.
	resp, body = admin.do("GET", dbsPath, nil)
	var list []dbServerView
	_ = json.Unmarshal(body, &list)
	if resp.StatusCode != 200 || len(list) != 1 || list[0].Databases != 0 || strings.Contains(string(body), "s3cret") {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	resp, body = admin.do("PUT", fmt.Sprintf("%s/%d", dbsPath, v.ID), map[string]any{"host": "db2.example.com", "port": 3307, "admin_password": maskedPassword})
	if resp.StatusCode != 200 {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
	if pw, _ := s.Store.DBServerPassword(v.ID); pw != "s3cret-admin-pw" {
		t.Fatalf("a masked password replaced the stored one: %q", pw)
	}
	agent.mu.Lock()
	test, _ = agent.lastParams[agentrpc.TestDBServer.Name()].(rpc.DBServerParams)
	agent.mu.Unlock()
	if test.Host != "db2.example.com" || test.Port != 3307 || test.AdminPassword != "s3cret-admin-pw" {
		t.Fatalf("the change was tested with %+v", test)
	}
	if resp, _ := admin.do("PUT", dbsPath+"/999", map[string]any{"host": "x"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update of a server that is not there: %d", resp.StatusCode)
	}
	// The test route.
	resp, body = admin.do("POST", fmt.Sprintf("%s/%d/test", dbsPath, v.ID), nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "10.11.6-MariaDB") {
		t.Fatalf("test: %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	agent.errs[agentrpc.TestDBServer.Name()] = agentrpc.Invalid("connection refused")
	agent.mu.Unlock()
	if resp, body = admin.do("POST", fmt.Sprintf("%s/%d/test", dbsPath, v.ID), nil); resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "connection refused") {
		t.Fatalf("a failed test: %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.TestDBServer.Name())
	agent.mu.Unlock()

	// Audit: what, never the password.
	events, _ := s.Store.ListAuditEvents(50)
	seen := map[string]bool{}
	for _, e := range events {
		if strings.HasPrefix(e.Action, "dbserver.") {
			seen[e.Action] = true
			if strings.Contains(e.Detail+e.Subject, "s3cret") {
				t.Errorf("audit %+v", e)
			}
		}
	}
	if !seen["dbserver.create"] || !seen["dbserver.update"] {
		t.Fatalf("audit %v", seen)
	}

	// Delete.
	if resp, _ := admin.do("DELETE", fmt.Sprintf("%s/%d", dbsPath, v.ID), nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := admin.do("DELETE", fmt.Sprintf("%s/%d", dbsPath, v.ID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete again: %d", resp.StatusCode)
	}
	if pw, _ := s.Store.DBServerPassword(v.ID); pw != "" {
		t.Fatal("the password survives its server")
	}
	agent.mu.Lock()
	set, _ = agent.lastParams[agentrpc.SetDBServers.Name()].(rpc.SetDBServersParams)
	agent.mu.Unlock()
	if len(set.Servers) != 0 {
		t.Fatalf("the agent still has the server: %+v", set)
	}
}

// A database is made on the server picked, with a refusal for one that does
// not exist, and the server is shown with it; the server in use cannot go.
func TestDatabaseOnAnExternalServer(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "ext.example.com", state.SiteStatic, "", 0)
	srv := newServer(t, admin, agent, "managed")
	base := fmt.Sprintf("/api/sites/%d", site.ID)

	if resp, body := admin.do("POST", base+"/databases", map[string]any{"name": "shop_data", "server_id": 999}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "unknown database server") {
		t.Fatalf("an unknown server: %d %s", resp.StatusCode, body)
	}
	resp, body := admin.do("POST", base+"/databases", map[string]any{"name": "shop_data", "server_id": srv.ID})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	waitTaskEnd(t, s, taskOf(t, body).ID)
	agent.mu.Lock()
	p, _ := agent.lastParams[agentrpc.CreateDatabase.Name()].(rpc.SiteDatabaseParams)
	agent.mu.Unlock()
	if p.Name != "shop_data" || p.ServerID != srv.ID {
		t.Fatalf("CreateDatabase params %+v", p)
	}
	// A database of the box's own, for contrast.
	resp, body = admin.do("POST", base+"/databases", map[string]any{"name": "local_data"})
	waitTaskEnd(t, s, taskOf(t, body).ID)

	resp, body = admin.do("GET", base+"/databases", nil)
	var list dbListView
	if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	byName := map[string]dbDatabaseView{}
	for _, d := range list.Databases {
		byName[d.Name] = d
	}
	if byName["shop_data"].ServerID != srv.ID || byName["local_data"].ServerID != 0 {
		t.Fatalf("databases %+v", list.Databases)
	}
	refs := map[int64]dbServerRef{}
	for _, r := range list.Servers {
		refs[r.ID] = r
	}
	if r := refs[srv.ID]; r.Host != "db.example.com" || r.Port != 3306 || r.TLS != "required" || r.Name != "managed" {
		t.Fatalf("servers %+v", list.Servers)
	}
	if _, ok := refs[0]; !ok {
		t.Fatalf("this server is not listed: %+v", list.Servers)
	}

	// Users: with no grants the body's server; with grants their databases'.
	user := func(name string, body map[string]any) (int, string) {
		body["name"], body["password"] = name, "a-good-password-1"
		resp, b := admin.do("POST", base+"/database-users", body)
		return resp.StatusCode, string(b)
	}
	if code, b := user("mixed_user", map[string]any{"grants": []state.DBGrant{{Database: "shop_data", Level: "read"}, {Database: "local_data", Level: "read"}}}); code != http.StatusBadRequest || !strings.Contains(b, "one server") {
		t.Fatalf("grants on two servers: %d %s", code, b)
	}
	other := newServer(t, admin, agent, "other")
	if code, b := user("wrong_user", map[string]any{"server_id": other.ID, "grants": []state.DBGrant{{Database: "shop_data", Level: "read"}}}); code != http.StatusBadRequest {
		t.Fatalf("a server other than the grants': %d %s", code, b)
	}
	code, b := user("shop_user", map[string]any{"grants": []state.DBGrant{{Database: "shop_data", Level: "all"}}})
	if code != http.StatusCreated {
		t.Fatalf("create user: %d %s", code, b)
	}
	agent.mu.Lock()
	up, _ := agent.lastParams[agentrpc.CreateDBUser.Name()].(rpc.DBUserParams)
	agent.mu.Unlock()
	if up.ServerID != srv.ID || up.User != "shop_user" {
		t.Fatalf("CreateDatabaseUser params %+v", up)
	}
	if u, err := s.Store.DBUserByName(site.ID, "shop_user"); err != nil || u.ServerID != srv.ID {
		t.Fatalf("%+v %v", u, err)
	}
	if code, b := user("empty_user", map[string]any{"server_id": srv.ID}); code != http.StatusCreated {
		t.Fatalf("an account with no grants on the server: %d %s", code, b)
	}
	resp, body = admin.do("PUT", base+"/database-users/shop_user/grants", map[string]any{"grants": []state.DBGrant{{Database: "local_data", Level: "read"}}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("grants moved to another server: %d %s", resp.StatusCode, body)
	}

	// The server is in use.
	if resp, body := admin.do("DELETE", fmt.Sprintf("%s/%d", dbsPath, srv.ID), nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete in use: %d %s", resp.StatusCode, body)
	}
}

// An agent that restarted does not know the servers: the call is repeated
// once, after the panel has handed them over.
func TestAgentRestartedGetsTheServersAgain(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "sync.example.com", state.SiteStatic, "", 0)
	srv := newServer(t, admin, agent, "managed")
	agent.mu.Lock()
	before := 0
	for _, m := range agent.calls {
		if m == agentrpc.SetDBServers.Name() {
			before++
		}
	}
	agent.errs[agentrpc.CreateDatabase.Name()] = &agentrpc.Error{Code: agentrpc.CodeUnavailable, Message: "unknown database server 1"}
	agent.mu.Unlock()
	agent.setHook(func(m string) {
		if m == agentrpc.SetDBServers.Name() {
			agent.mu.Lock()
			delete(agent.errs, agentrpc.CreateDatabase.Name()) // the agent knows them now
			agent.mu.Unlock()
		}
	})
	resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/databases", site.ID), map[string]any{"name": "shop_data", "server_id": srv.ID})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if j := waitTaskEnd(t, s, taskOf(t, body).ID); j.Status != state.JobSucceeded {
		t.Fatalf("the job: %s %q", j.Status, j.Error)
	}
	if n := countCalls(agent, agentrpc.CreateDatabase.Name()); n != 2 {
		t.Fatalf("CreateDatabase was called %d times, want 2 (the second after the push)", n)
	}
	if n := countCalls(agent, agentrpc.SetDBServers.Name()); n != before+1 {
		t.Fatalf("the servers were pushed %d times after registering, want 1", n-before)
	}
	agent.mu.Lock()
	set, _ := agent.lastParams[agentrpc.SetDBServers.Name()].(rpc.SetDBServersParams)
	agent.mu.Unlock()
	if len(set.Servers) != 1 || set.Servers[0].AdminPassword != "s3cret-admin-pw" {
		t.Fatalf("pushed %+v", set)
	}
}
