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

// Fase 9b: a static or plain PHP site is made without a database; a
// WordPress or Laravel site with one, its account granted all of it and
// its password the site's db_password secret.
func TestSiteTypesAndTheirDatabases(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	static := createViaAPI(t, s, c, staticSite("static.example.com"))
	php := createViaAPI(t, s, c, map[string]any{"domain": "php.example.com", "type": "php"})
	for _, site := range []state.Site{static, php} {
		if site.Config.Database.Enabled {
			t.Errorf("%s has a primary database", site.Domain)
		}
		if dbs, _ := s.Store.ListSiteDatabases(site.ID); len(dbs) != 0 {
			t.Errorf("%s has databases %+v", site.Domain, dbs)
		}
	}
	wp := createViaAPI(t, s, c, wordpressSite("wp.example.com"))
	name := fmt.Sprintf("site_%d", wp.ID)
	dbs, _ := s.Store.ListSiteDatabases(wp.ID)
	users, _ := s.Store.ListDBUsers(wp.ID)
	if len(dbs) != 1 || dbs[0].Name != name || len(users) != 1 || users[0].Name != name ||
		users[0].Secret != state.SiteSecret(wp.ID, state.SiteSecretDBPassword) || users[0].Grants[0].Level != "all" {
		t.Fatalf("databases %+v users %+v", dbs, users)
	}
	// The list shows the primary as such, sizes from the agent.
	resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d/databases", wp.ID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	var list dbListView
	json.Unmarshal(body, &list)
	if len(list.Databases) != 1 || !list.Databases[0].Primary || !list.Users[0].Primary {
		t.Fatalf("list %s", body)
	}
}

// Databases and users of a static site: created, granted, their password
// generated and shown once (a chosen one is never echoed), deleted with a
// typed confirmation; names checked, unique on the server; every write
// reaches the agent with the site.
func TestDatabasesAndUsersOfASite(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, c, staticSite("data.example.com"))
	other := createViaAPI(t, s, c, staticSite("other.example.com"))
	base := fmt.Sprintf("/api/sites/%d", site.ID)

	for _, bad := range []string{"mysql", "root", "Shop", "ab", "cgq_0123456789abcdef", fmt.Sprintf("site_%d", other.ID)} {
		if resp, body := c.do("POST", base+"/databases", map[string]string{"name": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("database %q = %d %s", bad, resp.StatusCode, body)
		}
	}
	if resp, body := c.do("POST", base+"/databases", map[string]string{"name": "shop_data"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create database = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if p, ok := agent.last(agentrpc.CreateDatabase.Name()).(rpc.SiteDatabaseParams); !ok || p.Name != "shop_data" || p.Site.ID != site.ID {
		t.Fatalf("CreateDatabase params %+v", agent.last(agentrpc.CreateDatabase.Name()))
	}
	if resp, _ := c.do("POST", fmt.Sprintf("/api/sites/%d/databases", other.ID), map[string]string{"name": "shop_data"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a name taken on the server = %d", resp.StatusCode)
	}

	// A generated password is returned once; a chosen one is not echoed.
	resp, body := c.do("POST", base+"/database-users", map[string]any{"name": "reader",
		"grants": []map[string]string{{"database": "shop_data", "level": "read"}}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user = %d %s", resp.StatusCode, body)
	}
	var created dbUserCreated
	json.Unmarshal(body, &created)
	if len(created.Password) < 24 || created.User.Grants[0].Level != "read" {
		t.Fatalf("created %s", body)
	}
	if pw, _ := s.Store.DBUserPassword(site.ID, "reader"); pw != created.Password {
		t.Fatal("the generated password was not kept as the user's secret")
	}
	resp, body = c.do("POST", base+"/database-users", map[string]any{"name": "writer", "password": "chosen-password-1",
		"grants": []map[string]string{{"database": "shop_data", "level": "all"}}})
	if resp.StatusCode != http.StatusCreated || strings.Contains(string(body), "chosen-password-1") {
		t.Fatalf("create user with a password = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []map[string]any{
		{"name": "short_pw", "password": "short"},
		{"name": "bad_grant", "grants": []map[string]string{{"database": "shop_data", "level": "write"}}},
		{"name": "foreign", "grants": []map[string]string{{"database": fmt.Sprintf("site_%d", other.ID), "level": "read"}}},
		{"name": "root"},
	} {
		if resp, body := c.do("POST", base+"/database-users", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, body)
		}
	}

	// Grants, password.
	if resp, body := c.do("PUT", base+"/database-users/reader/grants", map[string]any{
		"grants": []map[string]string{{"database": "shop_data", "level": "all"}}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("grants = %d %s", resp.StatusCode, body)
	}
	if p := agent.last(agentrpc.SetDBGrants.Name()).(rpc.DBUserParams); p.User != "reader" || p.Grants[0].Level != "all" {
		t.Fatalf("SetDBGrants %+v", p)
	}
	resp, body = c.do("POST", base+"/database-users/reader/password", map[string]string{"password": ""})
	var pw map[string]string
	json.Unmarshal(body, &pw)
	if resp.StatusCode != http.StatusOK || len(pw["password"]) < 24 {
		t.Fatalf("password = %d %s", resp.StatusCode, body)
	}
	if got, _ := s.Store.DBUserPassword(site.ID, "reader"); got != pw["password"] {
		t.Fatal("the new password was not kept")
	}
	// The creation's rule: what wp-cli cannot write into wp-config.php,
	// and a space at either end, never reach the agent.
	for _, bad := range []string{`x\'.system(chr(105)).\'`, `with "double" quotes`, `it's a password`, `ends in a backslash\`, " leading space here", "trailing space here "} {
		if resp, body := c.do("POST", base+"/database-users/reader/password", map[string]string{"password": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("password %q = %d %s", bad, resp.StatusCode, body)
		}
		if resp, body := c.do("POST", base+"/database-users", map[string]any{"name": "bad_pw", "password": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("new user with password %q = %d %s", bad, resp.StatusCode, body)
		}
	}
	if p := agent.last(agentrpc.SetDBPassword.Name()).(rpc.DBUserParams); p.Password != pw["password"] {
		t.Fatalf("a refused password reached the agent: %q", p.Password)
	}

	// Deletes need the name typed.
	if resp, _ := c.do("DELETE", base+"/database-users/reader", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete without confirm = %d", resp.StatusCode)
	}
	if resp, body := c.do("DELETE", base+"/database-users/reader?confirm=reader", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user = %d %s", resp.StatusCode, body)
	}
	if _, err := s.Store.DBUserByName(site.ID, "reader"); err == nil {
		t.Fatal("the user is still recorded")
	}
	if resp, _ := c.do("DELETE", base+"/databases/shop_data?confirm=nope", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete database with a wrong confirm = %d", resp.StatusCode)
	}
	if resp, body := c.do("DELETE", base+"/databases/shop_data?confirm=shop_data", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete database = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if dbs, _ := s.Store.ListSiteDatabases(site.ID); len(dbs) != 0 {
		t.Fatalf("databases after the delete %+v", dbs)
	}
	if u, _ := s.Store.DBUserByName(site.ID, "writer"); len(u.Grants) != 0 {
		t.Fatalf("grants on a deleted database stayed: %+v", u.Grants)
	}
	// Another site's database or user is not found through this site.
	if resp, _ := c.do("DELETE", fmt.Sprintf("/api/sites/%d/database-users/writer?confirm=writer", other.ID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another site's user = %d", resp.StatusCode)
	}
}

// The primary database and user of a WordPress site cannot be deleted, and
// the primary user keeps all of the primary database.
func TestPrimaryDatabaseIsKept(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	wp := createViaAPI(t, s, c, wordpressSite("wp.example.com"))
	name := fmt.Sprintf("site_%d", wp.ID)
	base := fmt.Sprintf("/api/sites/%d", wp.ID)
	if resp, _ := c.do("DELETE", base+"/databases/"+name+"?confirm="+name, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete the primary database = %d", resp.StatusCode)
	}
	if resp, _ := c.do("DELETE", base+"/database-users/"+name+"?confirm="+name, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete the primary user = %d", resp.StatusCode)
	}
	if resp, _ := c.do("PUT", base+"/database-users/"+name+"/grants", map[string]any{
		"grants": []map[string]string{{"database": name, "level": "read"}}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("demote the primary user = %d", resp.StatusCode)
	}
}

// A database the agent could not make leaves no record.
func TestCreateDatabaseAgentFailure(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, c, staticSite("fail.example.com"))
	agent.mu.Lock()
	agent.errs[agentrpc.CreateDatabase.Name()] = agentrpc.Conflict("a database called taken already exists on this server")
	agent.mu.Unlock()
	if resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/databases", site.ID), map[string]string{"name": "taken"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if inUse, _ := s.Store.DBNameInUse("taken", false); inUse {
		t.Fatal("a database the agent refused is recorded")
	}
}

// A reverse-proxy site has nothing to back up: it is not scheduled and a
// manual backup is refused.
func TestProxySitesAreNotBackedUp(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	setBackupRepo(t, s, "/var/backups/x", "pw")
	proxy := createViaAPI(t, s, c, map[string]any{"domain": "proxy.example.com", "type": "proxy", "proxy_upstream": "http://127.0.0.1:3000"})
	if proxy.Config.Backups.Enabled {
		t.Fatal("a proxy site's backups are on")
	}
	if resp, _ := c.do("POST", fmt.Sprintf("/api/sites/%d/backups", proxy.ID), nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("manual backup of a proxy site = %d", resp.StatusCode)
	}
	sites, _ := s.scheduledBackupSites()
	for _, site := range sites {
		if site.ID == proxy.ID {
			t.Fatal("a proxy site is scheduled")
		}
	}
}
