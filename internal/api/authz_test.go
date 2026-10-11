package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// routePolicy is who may reach each route. Every route Routes registers must
// be listed here, so a new route has to be given a policy on purpose:
//
//	public  anyone, signed in or not
//	token   authenticates with its own bearer token (tested elsewhere)
//	auth    any signed-in account
//	global  server-wide data: admins and read-only accounts, never operators
//	manage  admins and operators (operators on their own sites only)
//	admin   admins only
var routePolicy = map[string]string{
	"POST /api/setup":           "public",
	"POST /api/login":           "public",
	"GET /api/bootstrap":        "public",
	"GET /api/session":          "public",
	"GET /healthz":              "public",
	"/":                         "public",
	"POST /api/connector/purge": "token",

	"POST /api/logout":                  "auth",
	"POST /api/account/password":        "auth",
	"POST /api/account/2fa/begin":       "auth",
	"POST /api/account/2fa/confirm":     "auth",
	"POST /api/account/2fa/disable":     "auth",
	"GET /api/account/sessions":         "auth",
	"DELETE /api/account/sessions/{id}": "auth",
	"PUT /api/account/profile":          "auth",

	"GET /api/users":                "global",
	"POST /api/users":               "admin",
	"DELETE /api/users/{id}":        "admin",
	"PUT /api/users/{id}":           "admin",
	"POST /api/users/{id}/password": "admin",

	"GET /api/tokens":         "admin",
	"POST /api/tokens":        "admin",
	"DELETE /api/tokens/{id}": "admin",

	"GET /api/sites/{id}/cron":  "auth",
	"POST /api/sites/{id}/cron": "manage",
	"PATCH /api/cron/{id}":      "manage",
	"DELETE /api/cron/{id}":     "manage",
	"POST /api/cron/{id}/run":   "manage",

	"GET /api/sites/{id}/databases":                       "auth",
	"POST /api/sites/{id}/databases":                      "manage",
	"DELETE /api/sites/{id}/databases/{name}":             "manage",
	"POST /api/sites/{id}/database-users":                 "manage",
	"DELETE /api/sites/{id}/database-users/{name}":        "manage",
	"POST /api/sites/{id}/database-users/{name}/password": "manage",
	"PUT /api/sites/{id}/database-users/{name}/grants":    "manage",
	"GET /api/sites/{id}/database":                        "auth",
	"POST /api/sites/{id}/database/query":                 "manage",
	"POST /api/sites/{id}/database/export":                "manage",
	"POST /api/sites/{id}/database/import":                "manage",

	// Quarry's own routes take a Quarry session, which no panel
	// role holds (studio_routes_test.go tests them with one).
	"POST /api/quarry/login":                        "public",
	"POST /api/quarry/handoff":                      "public",
	"GET /api/quarry/session":                       "public",
	"POST /api/sites/{id}/quarry/handoff":           "auth",
	"POST /api/quarry/logout":                       "dbstudio",
	"GET /api/quarry/databases":                     "dbstudio",
	"GET /api/quarry/db/{db}/tables":                "dbstudio",
	"GET /api/quarry/db/{db}/tables/{table}":        "dbstudio",
	"GET /api/quarry/tasks/{id}":                    "dbstudio",
	"POST /api/quarry/db/{db}/tables/{table}/rows":  "dbstudio",
	"POST /api/quarry/db/{db}/tables/{table}/edit":  "dbstudio",
	"POST /api/quarry/db/{db}/tables/{table}/alter": "dbstudio",
	"POST /api/quarry/db/{db}/query":                "dbstudio",
	"POST /api/quarry/db/{db}/simulate":             "dbstudio",
	"POST /api/quarry/db/{db}/search":               "dbstudio",
	"POST /api/quarry/db/{db}/export":               "dbstudio",
	"POST /api/quarry/db/{db}/import":               "dbstudio",
	"GET /api/quarry/files":                         "dbstudio",
	"GET /api/quarry/exports/{name}":                "dbstudio",
	"DELETE /api/quarry/files/{dir}/{name}":         "dbstudio",
	"POST /api/quarry/import/upload":                "dbstudio",
	"GET /api/sites/{id}/quarry/wp/autoload":        "auth",
	"POST /api/sites/{id}/quarry/wp/search-replace": "manage",
	"POST /api/sites/{id}/quarry/wp/transients":     "manage",
	"POST /api/sites/{id}/migration":                "manage",

	"GET /api/sites/{id}/files":                     "auth",
	"GET /api/sites/{id}/files/read":                "manage",
	"POST /api/sites/{id}/files/write":              "manage",
	"GET /api/sites/{id}/files/download":            "manage",
	"POST /api/sites/{id}/files/upload":             "manage",
	"POST /api/sites/{id}/files/manage":             "manage",
	"POST /api/sites/{id}/sftp":                     "manage",
	"GET /api/sites/{id}/ssh-keys":                  "auth",
	"POST /api/sites/{id}/ssh-keys":                 "manage",
	"DELETE /api/sites/{id}/ssh-keys/{fingerprint}": "manage",
	"POST /api/sites/{id}/ssh-shell":                "admin",
	"GET /api/sites/{id}/routing":                   "admin",
	"PUT /api/sites/{id}/routing":                   "admin",
	"POST /api/sites/{id}/wp/login":                 "manage",
	"GET /api/sites/{id}/wp/plugins":                "auth",
	"POST /api/sites/{id}/wp/update":                "manage",
	"GET /api/sites/{id}/cache-stats":               "auth",
	"GET /api/sites/{id}/logs":                      "auth",
	"POST /api/sites/{id}/warm":                     "manage",

	"GET /api/services":                  "global",
	"POST /api/services/{name}/restart":  "admin",
	"GET /api/logs":                      "global",
	"GET /api/firewall":                  "global",
	"GET /api/php/{version}/extensions":  "auth",
	"POST /api/php/{version}/extensions": "admin",
	"POST /api/firewall/rule":            "admin",
	"POST /api/firewall/enable":          "admin",
	"POST /api/panel/certificate":        "admin",
	"POST /api/panel/update":             "admin",

	"GET /api/sites":                                             "auth",
	"POST /api/sites":                                            "admin",
	"GET /api/sites/{id}":                                        "auth",
	"DELETE /api/sites/{id}":                                     "admin",
	"GET /api/sites/{id}/uptime":                                 "auth",
	"GET /api/sites/{id}/vulnerabilities":                        "auth",
	"POST /api/sites/{id}/vulnerabilities/check":                 "manage",
	"GET /api/sites/{id}/malware":                                "auth",
	"POST /api/sites/{id}/malware/scan":                          "manage",
	"POST /api/sites/{id}/malware/findings/{finding}/quarantine": "manage",
	"GET /api/sites/{id}/usage":                                  "auth",
	"PUT /api/sites/{id}/uptime":                                 "manage",
	"POST /api/sites/{id}/purge":                                 "manage",
	"POST /api/sites/{id}/protected-paths":                       "manage",
	"DELETE /api/sites/{id}/protected-paths":                     "manage",
	"POST /api/sites/{id}/certificate":                           "manage",
	"POST /api/sites/{id}/staging":                               "manage",
	"GET /api/sites/{id}/staging/credentials":                    "manage",
	"POST /api/sites/{id}/staging/recreate":                      "manage",
	"GET /api/sites/{id}/staging/diff":                           "manage",
	"GET /api/sites/{id}/certificate":                            "auth",
	"PUT /api/sites/{id}/certificate/custom":                     "manage",
	"DELETE /api/sites/{id}/certificate/custom":                  "manage",
	"GET /api/system/alerts":                                     "auth",
	"POST /api/system/alerts/{id}/dismiss":                       "manage",
	"PATCH /api/sites/{id}":                                      "manage",
	"PUT /api/sites/{id}/backups/policy":                         "manage",
	"GET /api/sites/{id}/deployments":                            "auth",
	"POST /api/sites/{id}/deployments":                           "manage",
	"POST /api/sites/{id}/safe-push":                             "manage",
	"GET /api/sites/{id}/staging/tables":                         "auth",
	"POST /api/sites/{id}/staging/database":                      "manage",
	"POST /api/deployments/{id}/promote":                         "manage",
	"POST /api/sites/{id}/rollback":                              "manage",

	"GET /api/sites/{id}/backups":            "auth",
	"POST /api/sites/{id}/backups":           "manage",
	"POST /api/backups/{id}/verify":          "manage",
	"POST /api/backups/{id}/restore":         "manage",
	"POST /api/backups/test":                 "admin",
	"DELETE /api/backups/{id}":               "admin",
	"POST /api/sites/{id}/backups/retention": "admin",

	// Task lists are filtered to an operator's own sites in the handler; so
	// is the event stream, per event (stream_test.go).
	"GET /api/tasks":              "auth",
	"GET /api/tasks/names":        "auth",
	"GET /api/tasks/{id}":         "auth",
	"POST /api/tasks/{id}/cancel": "manage",
	"GET /api/events":             "auth",

	"GET /api/system/status":              "global",
	"GET /api/system/capabilities":        "auth",
	"GET /api/system/metrics":             "global",
	"GET /api/system/update":              "auth", // the panel's own version, shown to everyone
	"GET /api/system/drift":               "global",
	"POST /api/system/drift/{id}/resolve": "admin",
	"POST /api/system/maintenance":        "admin",
	"GET /api/audit":                      "global",
	"GET /api/settings":                   "global",
	"PUT /api/settings":                   "admin",
}

var policyAllows = map[string]map[string]bool{
	"public": {"anonymous": true, "readonly": true, "operator": true, "admin": true},
	"auth":   {"readonly": true, "operator": true, "admin": true},
	"global": {"readonly": true, "admin": true},
	"manage": {"operator": true, "admin": true},
	"admin":  {"admin": true},
	// A panel session reaches none of Quarry's own routes.
	"dbstudio": {},
}

// guardDenials are the refusals the route guards give. Anything else (a 400,
// a 404, a handler's own 403 for a wrong password) means the request got past
// the guard.
var guardDenials = map[string]bool{
	"authentication required":         true,
	"this is a read-only account":     true,
	"site management access required": true,
	"server-wide access required":     true,
	"sign in to Quarry":               true,
}

// Every registered route, for every role, is reached or refused exactly as its
// policy says. Requests go through the real router with real sessions, so the
// test asserts on what the panel answers, not on how the table is declared.
func TestEveryRouteEnforcesItsPolicy(t *testing.T) {
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.UpdateCheck = false; return nil })
	site, err := s.Store.CreateSite(state.Site{
		Domain: "own.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		SystemUser: "cg-site-1", RootPath: "/srv/sites/own.example.com", Status: state.SiteActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := s.Store.GetUserByEmail("admin@example.com")
	hash, _ := hashPassword("unused-password")
	readonly, _ := s.Store.CreateUser("readonly@example.com", hash, "readonly")
	operator, _ := s.Store.CreateUser("operator@example.com", hash, "operator")
	if err := s.Store.SetUserSites(operator.ID, []int64{site.ID}); err != nil {
		t.Fatal(err)
	}
	roles := []struct {
		name string
		id   int64
	}{{"anonymous", 0}, {"readonly", readonly.ID}, {"operator", operator.ID}, {"admin", admin.ID}}

	// The table and the router agree, both ways.
	registered := map[string]bool{}
	for _, p := range s.routePatterns {
		registered[p] = true
		if _, ok := routePolicy[p]; !ok {
			t.Errorf("route %q has no entry in routePolicy: decide who may reach it", p)
		}
	}
	var stale []string
	for p := range routePolicy {
		if !registered[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		t.Errorf("routePolicy lists %q, which is not registered", p)
	}

	// Admin last: its requests change state (it may delete the site).
	for _, role := range roles {
		for _, pattern := range s.routePatterns {
			policy := routePolicy[pattern]
			if policy == "token" || policy == "" {
				continue
			}
			method, path := "GET", pattern
			if m, p, ok := strings.Cut(pattern, " "); ok {
				method, path = m, p
			}
			if strings.HasPrefix(path, "/api/sites/{id}") {
				path = strings.Replace(path, "{id}", strconv.FormatInt(site.ID, 10), 1)
			}
			path = strings.NewReplacer("{id}", "999999", "{name}", "nginx", "{fingerprint}", "SHA256:none", "{table}", "wp_options", "{dir}", "exports", "{db}", "site_1").Replace(path)

			cookie := ""
			if role.id != 0 {
				// A fresh session per request: some routes (logout, password
				// change) end the one they were called with.
				cookie = randomToken(32)
				if err := s.Store.CreateSession(cookie, role.id, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			status, msg := probeRoute(t, ts.URL, method, path, cookie)
			denied := (status == http.StatusUnauthorized || status == http.StatusForbidden) &&
				(guardDenials[msg] || strings.HasPrefix(msg, "only administrators can "))
			if want := policyAllows[policy][role.name]; denied == want {
				verb := "reached"
				if denied {
					verb = "was refused"
				}
				t.Errorf("%s %s %s (%d %q); policy %q", role.name, verb, pattern, status, msg, policy)
			}
		}
	}
	waitForTasks(t, s)
}

func probeRoute(t *testing.T, base, method, path, cookie string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var body io.Reader
	if method != http.MethodGet {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: insecureSessionName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	// Only refusals are read: an allowed stream (/api/events) never ends.
	var e struct {
		Error string `json:"error"`
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
	}
	return resp.StatusCode, e.Error
}
