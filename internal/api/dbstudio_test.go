package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Quarry as a service of its own (SPEC.md §6.4): its sessions, the two
// ways in, and the data routes.

// dbStudioSetup makes a WordPress site with a managed database and a
// signed-in read-only client.
func dbStudioSetup(t *testing.T) (*Server, *fakeAgent, *client, *client, state.Site) {
	t.Helper()
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, _ := c.do("POST", "/api/sites", map[string]any{
		"domain": "studio.example.com", "type": "wordpress", "title": "Studio",
		"admin_email": "owner@studio.example.com", "admin_user": "owner", "admin_password": "wordpress-admin-pass",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatal("site create failed")
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain("studio.example.com")
	if err != nil {
		t.Fatal(err)
	}
	ro := panelClient(t, s, c.base, "viewer@example.com", "readonly")
	return s, agent, c, ro, site
}

// panelClient is a client signed in to the panel as a new account.
func panelClient(t *testing.T, s *Server, base, email, role string) *client {
	t.Helper()
	hash, _ := hashPassword("unused-password")
	u, err := s.Store.CreateUser(email, hash, role)
	if err != nil {
		t.Fatal(err)
	}
	tok := randomToken(32)
	if err := s.Store.CreateSession(tok, u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	return &client{t: t, base: base, cookies: []*http.Cookie{{Name: insecureSessionName, Value: tok}}}
}

func (f *fakeAgent) last(method string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastParams[method]
}

// sdo is c.do keeping c's cookies: a Quarry client carries the panel's
// cookie and its own, and a refusal that expires one must not drop the
// other from the test's jar.
func sdo(c *client, method, path string, body any) (*http.Response, []byte) {
	c.t.Helper()
	saved := append([]*http.Cookie(nil), c.cookies...)
	resp, b := c.do(method, path, body)
	c.cookies = saved
	return resp, b
}

func cookieOf(resp *http.Response, name string) *http.Cookie {
	for _, ck := range resp.Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// handoffToken asks the panel for a handoff into Quarry.
func handoffToken(t *testing.T, panel *client, siteID int64, database string) string {
	t.Helper()
	resp, body := sdo(panel, "POST", fmt.Sprintf("/api/sites/%d/quarry/handoff", siteID), map[string]any{"database": database})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("handoff: %d %s", resp.StatusCode, body)
	}
	return jsonString(t, body, "token")
}

// openStudio is a Quarry client opened from panel's session.
func openStudio(t *testing.T, panel *client, siteID int64) *client {
	t.Helper()
	tok := handoffToken(t, panel, siteID, "")
	sc := &client{t: t, base: panel.base, cookies: append([]*http.Cookie(nil), panel.cookies...)}
	resp, body := sdo(sc, "POST", "/api/quarry/handoff", map[string]string{"token": tok})
	ck := cookieOf(resp, insecureStudioCookie)
	if resp.StatusCode != http.StatusOK || ck == nil {
		t.Fatalf("exchange: %d %s", resp.StatusCode, body)
	}
	sc.cookies = append(sc.cookies, ck)
	return sc
}

func jsonString(t *testing.T, body []byte, key string) string {
	t.Helper()
	s, _ := jsonField(t, body, key).(string)
	return s
}

// jsonField is one field of a JSON object.
func jsonField(t *testing.T, body []byte, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return m[key]
}

// The handoff (SPEC.md §6.4): minted for a panel session (never an API
// token), exchanged once, within its minute, with that same panel
// session; the Quarry session then lasts only as long as that panel
// session does.
func TestStudioHandoff(t *testing.T) {
	s, _, admin, _, site := dbStudioSetup(t)
	tok := handoffToken(t, admin, site.ID, "")

	other := panelClient(t, s, admin.base, "second@example.com", "admin")
	resp, body := sdo(other, "POST", "/api/quarry/handoff", map[string]string{"token": tok})
	if resp.StatusCode != http.StatusUnauthorized || cookieOf(resp, insecureStudioCookie) != nil {
		t.Fatalf("another panel session exchanged it: %d %s", resp.StatusCode, body)
	}
	// A refused try spends it too.
	if resp, _ := sdo(admin, "POST", "/api/quarry/handoff", map[string]string{"token": tok}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a handoff tried once was taken again: %d", resp.StatusCode)
	}

	tok = handoffToken(t, admin, site.ID, "")
	resp, body = sdo(admin, "POST", "/api/quarry/handoff", map[string]string{"token": tok})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"mode":"panel"`) || !strings.Contains(string(body), `"read_only":false`) {
		t.Fatalf("exchange: %d %s", resp.StatusCode, body)
	}
	if resp, _ := sdo(admin, "POST", "/api/quarry/handoff", map[string]string{"token": tok}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a handoff worked twice: %d", resp.StatusCode)
	}

	at := time.Now()
	s.studio.now = func() time.Time { return at }
	tok = handoffToken(t, admin, site.ID, "")
	at = at.Add(handoffTTL + time.Second)
	if resp, _ := sdo(admin, "POST", "/api/quarry/handoff", map[string]string{"token": tok}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired handoff: %d", resp.StatusCode)
	}
	s.studio.now = nil

	// An API token cannot open Quarry.
	if resp, _ := admin.do("POST", "/api/tokens", map[string]any{"name": "script"}); resp.StatusCode != http.StatusCreated {
		t.Fatal("token create failed")
	}
	_, list := admin.do("POST", "/api/tokens", map[string]any{"name": "script2"})
	secret := jsonString(t, list, "token")
	req, _ := http.NewRequest("POST", admin.base+fmt.Sprintf("/api/sites/%d/quarry/handoff", site.ID), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("an API token minted a handoff: %d", r.StatusCode)
	}

	// The Quarry session works with its panel session only: without the
	// panel's cookie, or once the panel session ends, it is gone.
	sc := openStudio(t, admin, site.ID)
	if resp, body := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("databases: %d %s", resp.StatusCode, body)
	}
	alone := &client{t: t, base: sc.base, cookies: []*http.Cookie{sc.cookies[len(sc.cookies)-1]}}
	if resp, body := sdo(alone, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), msgStudioPanel) {
		t.Fatalf("without the panel session: %d %s", resp.StatusCode, body)
	}
	sc = openStudio(t, admin, site.ID)
	if resp, _ := sdo(admin, "POST", "/api/logout", map[string]any{}); resp.StatusCode != http.StatusOK {
		t.Fatal("panel logout failed")
	}
	before := s.studio.count()
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the panel signed out: %d", resp.StatusCode)
	}
	if s.studio.count() != before-1 {
		t.Fatal("a Quarry session refused for its panel session was kept")
	}
}

// An operator opens Quarry for its own sites only; losing the site
// ends the Quarry session.
func TestStudioHandoffNeedsTheSite(t *testing.T) {
	s, _, admin, _, site := dbStudioSetup(t)
	op := panelClient(t, s, admin.base, "op@example.com", "operator")
	if resp, _ := sdo(op, "POST", fmt.Sprintf("/api/sites/%d/quarry/handoff", site.ID), map[string]any{}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an operator of other sites: %d", resp.StatusCode)
	}
	u, _ := s.Store.GetUserByEmail("op@example.com")
	if err := s.Store.SetUserSites(u.ID, []int64{site.ID}); err != nil {
		t.Fatal(err)
	}
	sc := openStudio(t, op, site.ID)
	if resp, body := sdo(sc, "POST", "/api/quarry/db/site_1/tables/wp_posts/rows", map[string]any{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("an operator on its site: %d %s", resp.StatusCode, body)
	}
	if err := s.Store.SetUserSites(u.ID, nil); err != nil {
		t.Fatal(err)
	}
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the site was taken away: %d", resp.StatusCode)
	}
}

// SR-T4 in Quarry: opened from the panel by a read-only account, it
// lists the tables and reads their structure, but neither reads rows nor
// runs SQL nor writes; a manager's write is audited with the database,
// table and key, never the row's values.
func TestStudioRolesAndAudit(t *testing.T) {
	s, agent, admin, ro, site := dbStudioSetup(t)
	db := site.Config.Database.Name
	base := "/api/quarry/db/" + db
	rs := openStudio(t, ro, site.ID)
	as := openStudio(t, admin, site.ID)
	_, view := sdo(rs, "GET", "/api/quarry/session", nil)
	if !strings.Contains(string(view), `"read_only":true`) {
		t.Fatalf("a read-only account's session: %s", view)
	}
	for _, q := range []map[string]any{{"sql": "DELETE FROM wp_posts", "allow_writes": true}, {"sql": "SELECT user_pass FROM wp_users"}} {
		if resp, body := sdo(rs, "POST", base+"/query", q); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), msgStudioReadOnly) {
			t.Fatalf("a read-only account ran %v: %d %s", q, resp.StatusCode, body)
		}
	}
	for _, path := range []string{base + "/tables/wp_users/rows", base + "/tables/wp_posts/edit", base + "/search", base + "/export"} {
		if resp, _ := sdo(rs, "POST", path, map[string]any{}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("a read-only account reached %s: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := sdo(rs, "GET", "/api/quarry/files", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a read-only account listed the files: %d", resp.StatusCode)
	}
	if agent.called(agentrpc.DBStudioQuery.Name()) || agent.called(agentrpc.DBStudioRows.Name()) {
		t.Fatal("a refused read reached the agent")
	}
	if resp, body := sdo(rs, "GET", base+"/tables", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a read-only account cannot list the tables: %d %s", resp.StatusCode, body)
	}
	if p := agent.last(agentrpc.DBStudioTables.Name()).(rpc.DBStudioTablesParams); p.Login != "" || p.Database != db || p.Site.ID != site.ID {
		t.Fatalf("tables sent %+v", p)
	}
	if resp, _ := sdo(as, "GET", "/api/quarry/db/other_db/tables", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a database the site does not have: %d", resp.StatusCode)
	}

	resp, body := sdo(as, "POST", base+"/query", map[string]any{"sql": "SELECT 1", "explain": true})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"read_only":true`) {
		t.Fatalf("a manager's query without writes: %d %s", resp.StatusCode, body)
	}
	if p := agent.last(agentrpc.DBStudioQuery.Name()).(rpc.DBStudioQueryParams); !p.ReadOnly || p.SQL != "EXPLAIN SELECT 1" || p.Database != db {
		t.Fatalf("sent %+v", p)
	}

	const secret = "row-value-never-in-the-audit"
	resp, body = sdo(as, "POST", base+"/tables/wp_posts/edit", map[string]any{"changes": []map[string]any{
		{"op": "update", "key": map[string]string{"ID": "7"}, "values": map[string]any{"post_title": secret}},
		{"op": "insert", "values": map[string]any{"post_title": secret}},
	}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit: %d %s", resp.StatusCode, body)
	}
	if p := agent.last(agentrpc.DBStudioEdit.Name()).(rpc.DBStudioEditParams); p.ReadOnly || p.Table != "wp_posts" || len(p.Changes) != 2 || p.Login != "" {
		t.Fatalf("sent %+v", p)
	}
	// A dry run is not audited.
	sdo(as, "POST", base+"/tables/wp_posts/edit", map[string]any{"dry_run": true, "changes": []map[string]any{{"op": "delete", "key": map[string]string{"ID": "8"}}}})
	sdo(as, "POST", base+"/tables/wp_log/alter", map[string]any{"kind": "drop", "confirm": "wp_log"})
	events, _ := s.Store.ListAuditEvents(50)
	var got []string
	for _, e := range events {
		if strings.HasPrefix(e.Action, "quarry.") && e.Action != "quarry.handoff" && e.Action != "quarry.open" {
			got = append(got, e.Action+" "+e.Detail)
		}
		if strings.Contains(e.Detail, secret) {
			t.Fatalf("a row's value is in the audit: %+v", e)
		}
	}
	want := "quarry.drop db=" + db + " table=wp_log|quarry.insert db=" + db + " table=wp_posts|quarry.update db=" + db + " table=wp_posts key=ID=7"
	if strings.Join(got, "|") != want {
		t.Fatalf("audit %q, want %q", got, want)
	}
}

// loginSetup: a site with a database user "reader" the store records, and
// the agent's answer to its sign-in.
func loginSetup(t *testing.T) (*Server, *fakeAgent, *client, state.Site) {
	t.Helper()
	s, agent, admin, _, site := dbStudioSetup(t)
	if _, err := s.Store.AddDBUser(site.ID, "reader", state.DBUserSecret(site.ID, "reader"), "reader-password-1",
		[]state.DBGrant{{Database: site.Config.Database.Name, Level: "read"}}); err != nil {
		t.Fatal(err)
	}
	s.studioGate.sleep = func(context.Context, time.Duration) error { return nil }
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Login: strings.Repeat("ab", 32), Databases: []string{site.Config.Database.Name}})
	return s, agent, &client{t: t, base: admin.base}, site
}

func signInStudio(c *client, user, password string) (*http.Response, []byte) {
	resp, body := c.do("POST", "/api/quarry/login", map[string]string{"user": user, "password": password})
	return resp, body
}

// A database user signs in with its own password, which goes to the agent
// (the server checks it) and is not kept here; everything it does then
// carries the agent's handle and is as that user, without a panel
// account. Its session reaches no panel route.
func TestStudioLogin(t *testing.T) {
	s, agent, anon, site := loginSetup(t)
	resp, body := signInStudio(anon, "reader", "reader-password-1")
	ck := cookieOf(resp, insecureStudioCookie)
	if resp.StatusCode != http.StatusOK || ck == nil || !strings.Contains(string(body), `"mode":"login"`) || !strings.Contains(string(body), `"user":"reader"`) {
		t.Fatalf("sign in: %d %s", resp.StatusCode, body)
	}
	if !ck.HttpOnly || ck.Path != "/" || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", ck)
	}
	p := agent.last(agentrpc.DBStudioOpen.Name()).(rpc.DBStudioOpenParams)
	if p.User != "reader" || p.Password != "reader-password-1" || p.Site.ID != site.ID {
		t.Fatalf("sign-in sent %+v", p)
	}
	for _, st := range s.studio.byKey {
		if strings.Contains(strings.Join([]string{st.key, st.dbUser, st.login, st.panelKey, st.database, st.address}, " "), "reader-password-1") {
			t.Fatal("the API keeps the password")
		}
	}
	sc := &client{t: t, base: anon.base, cookies: []*http.Cookie{ck}}
	db := site.Config.Database.Name
	if resp, body := sdo(sc, "POST", "/api/quarry/db/"+db+"/tables/wp_posts/rows", map[string]any{"search": "hello"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("rows: %d %s", resp.StatusCode, body)
	}
	if p := agent.last(agentrpc.DBStudioRows.Name()).(rpc.DBStudioRowsParams); p.Login != strings.Repeat("ab", 32) || p.Search != "hello" || p.Database != db {
		t.Fatalf("rows sent %+v", p)
	}
	// Any database name goes to the server, which decides.
	if resp, _ := sdo(sc, "GET", "/api/quarry/db/another_site_db/tables", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a signed-in user's database is the server's to refuse: %d", resp.StatusCode)
	}
	resp, body = sdo(sc, "POST", "/api/quarry/db/"+db+"/query", map[string]any{"sql": "UPDATE wp_posts SET post_title = 'x'"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"read_only":false`) {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	events, _ := s.Store.ListAuditEvents(10)
	found := false
	for _, e := range events {
		found = found || (e.Action == "database.write" && e.Actor == "reader (database user)" && strings.HasPrefix(e.Detail, "db="+db+" UPDATE"))
	}
	if !found {
		t.Fatalf("a signed-in user's write statement is not audited: %+v", events)
	}
	// It is no panel session.
	for _, path := range []string{"/api/sites", "/api/session", fmt.Sprintf("/api/sites/%d", site.ID)} {
		resp, body := sdo(sc, "GET", path, nil)
		if resp.StatusCode == http.StatusOK && !strings.Contains(string(body), `"user":null`) {
			t.Fatalf("a Quarry session reached %s: %s", path, body)
		}
	}
	// The agent no longer having the session (a changed password) ends it.
	agent.setErr(agentrpc.DBStudioRows.Name(), &agentrpc.Error{Code: agentrpc.CodeUnauthorized, Message: "the database no longer accepts this user's name and password: sign in again"})
	if resp, body := sdo(sc, "POST", "/api/quarry/db/"+db+"/tables/wp_posts/rows", map[string]any{}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "sign in again") {
		t.Fatalf("a password the server dropped: %d %s", resp.StatusCode, body)
	}
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the session outlived its password: %d", resp.StatusCode)
	}
	agent.setErr(agentrpc.DBStudioRows.Name(), nil)

	// Logout ends it here and on the agent.
	resp, _ = signInStudio(anon, "reader", "reader-password-1")
	sc = &client{t: t, base: anon.base, cookies: []*http.Cookie{cookieOf(resp, insecureStudioCookie)}}
	agent.forget()
	if resp, _ := sdo(sc, "POST", "/api/quarry/logout", map[string]any{}); resp.StatusCode != http.StatusOK {
		t.Fatal("logout failed")
	}
	if !agent.called(agentrpc.DBStudioClose.Name()) {
		t.Fatal("logout did not end the agent's session")
	}
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
	if _, body := sdo(sc, "GET", "/api/quarry/session", nil); !strings.Contains(string(body), `"session":null`) {
		t.Fatalf("session after logout: %s", body)
	}
}

// A refused sign-in says only that, as slowly for an unknown user as for a
// wrong password; refusals delay the user's next attempts and never lock
// it; the windows bound what one network sends.
func TestStudioLoginRefusals(t *testing.T) {
	s, agent, anon, _ := loginSetup(t)
	var waits []time.Duration
	var mu sync.Mutex
	s.studioGate.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return nil
	}
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Refused: true})
	for _, c := range []struct{ user, why string }{{"reader", "a wrong password"}, {"nobody_here", "an unknown user"}, {"root", "root"}} {
		agent.forget()
		start := time.Now()
		resp, body := signInStudio(anon, c.user, "wrong-password")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), msgStudioBadLogin) || cookieOf(resp, insecureStudioCookie) != nil {
			t.Fatalf("%s: %d %s", c.why, resp.StatusCode, body)
		}
		if time.Since(start) < studioFloor {
			t.Fatalf("%s answered in %v, faster than the floor", c.why, time.Since(start))
		}
		if c.user != "reader" && agent.called(agentrpc.DBStudioOpen.Name()) {
			t.Fatalf("%s reached the database server", c.why)
		}
	}
	events, _ := s.Store.ListAuditEvents(5)
	if events[0].Action != "quarry.login_failed" {
		t.Fatalf("audit %+v", events[0])
	}
	// Five refusals for a user: the next attempt waits; the right password
	// still signs in (a delay, never a lock).
	for range 4 {
		signInStudio(anon, "reader", "wrong-password")
	}
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Login: strings.Repeat("cd", 32)})
	mu.Lock()
	waits = nil
	mu.Unlock()
	resp, body := signInStudio(anon, "reader", "reader-password-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the right password after refusals: %d %s", resp.StatusCode, body)
	}
	mu.Lock()
	if len(waits) != 1 || waits[0] < time.Second {
		t.Fatalf("waits %v, want one delay of a second or more", waits)
	}
	mu.Unlock()
	if s.studioGate.delays.Failures("reader") != 0 {
		t.Fatal("a success left the failures counted")
	}
	// Six a minute from one network for one user.
	if resp, _ := signInStudio(anon, "reader", "reader-password-1"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a seventh attempt from one network: %d", resp.StatusCode)
	}
	// Bounds before anything runs.
	if resp, _ := signInStudio(anon, strings.Repeat("u", maxStudioUser+1), "x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a long user name: %d", resp.StatusCode)
	}
	if resp, _ := signInStudio(anon, "reader", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no password: %d", resp.StatusCode)
	}
}

// The windows: twenty a minute from one network, whatever the user.
func TestStudioLoginNetworkWindow(t *testing.T) {
	s, agent, anon, _ := loginSetup(t)
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Refused: true})
	s.studioGate.network = newStudioGate().network
	refused := 0
	for i := range studioPerNetwork + 1 {
		resp, _ := signInStudio(anon, fmt.Sprintf("user_%d", i), "wrong-password")
		if resp.StatusCode == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("%d of %d attempts from one network refused, want the last one", refused, studioPerNetwork+1)
	}
}

// A signed-in user sees only the files it made: the exports its tasks
// wrote, the uploads it sent; it cannot replace another's upload.
func TestStudioLoginFiles(t *testing.T) {
	s, agent, anon, site := loginSetup(t)
	resp, _ := signInStudio(anon, "reader", "reader-password-1")
	sc := &client{t: t, base: anon.base, cookies: []*http.Cookie{cookieOf(resp, insecureStudioCookie)}}
	db := site.Config.Database.Name
	agent.setResult(agentrpc.DBStudioFiles.Name(), rpc.DBStudioFilesResult{
		Exports: []rpc.DBStudioFile{{Name: "site_1-20261010-000000.sql"}},
		Imports: []rpc.DBStudioFile{{Name: "admins.sql"}},
	})
	_, body := sdo(sc, "GET", "/api/quarry/files", nil)
	if strings.Contains(string(body), "site_1-2026") || strings.Contains(string(body), "admins.sql") {
		t.Fatalf("a signed-in user sees others' files: %s", body)
	}
	if resp, _ := sdo(sc, "GET", "/api/quarry/exports/site_1-20261010-000000.sql", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("downloaded another's export: %d", resp.StatusCode)
	}
	if resp, _ := sdo(sc, "DELETE", "/api/quarry/files/imports/admins.sql", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted another's upload: %d", resp.StatusCode)
	}
	upload := func(name string, offset int) int {
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/quarry/import/upload?name=%s&offset=%d", sc.base, name, offset), strings.NewReader("data"))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.AddCookie(sc.cookies[0])
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := upload("admins.sql", 0); code != http.StatusConflict {
		t.Fatalf("replaced another's upload: %d", code)
	}
	if code := upload("mine.sql", 4); code != http.StatusNotFound {
		t.Fatalf("appended to an upload it never started: %d", code)
	}
	if code := upload("mine.sql", 0); code != http.StatusOK {
		t.Fatalf("its own upload: %d", code)
	}
	if code := upload("mine.sql", 4); code != http.StatusOK {
		t.Fatalf("its own upload's next chunk: %d", code)
	}
	if resp, _ := sdo(sc, "POST", "/api/quarry/db/"+db+"/import", map[string]string{"name": "admins.sql", "format": "sql", "confirm": db}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("imported another's upload: %d", resp.StatusCode)
	}
	if resp, _ := sdo(sc, "POST", "/api/quarry/db/"+db+"/import", map[string]string{"name": "mine.sql", "format": "sql", "confirm": "wrong"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an import without the database's name: %d", resp.StatusCode)
	}
	resp, body = sdo(sc, "POST", "/api/quarry/db/"+db+"/import", map[string]string{"name": "mine.sql", "format": "sql", "confirm": db})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if p := agent.last(agentrpc.DBStudioImportSQL.Name()).(rpc.DBStudioImportSQLParams); p.Login != strings.Repeat("ab", 32) || p.Name != "mine.sql" || p.Database != db {
		t.Fatalf("import sent %+v", p)
	}
	if agent.called(agentrpc.DBImport.Name()) {
		t.Fatal("a signed-in user's import ran as the site (DBImport)")
	}
	// Its export becomes its own, and the task is its to follow.
	agent.setResult(agentrpc.DBStudioExport.Name(), rpc.DBStudioExportResult{Name: "site_1-20261010-120000.sql", SizeBytes: 9})
	resp, body = sdo(sc, "POST", "/api/quarry/db/"+db+"/export", map[string]any{"format": "sql"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("export: %d %s", resp.StatusCode, body)
	}
	task := jsonField(t, body, "task").(map[string]any)
	waitForTasks(t, s)
	if resp, body := sdo(sc, "GET", fmt.Sprintf("/api/quarry/tasks/%.0f", task["id"]), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("its task: %d %s", resp.StatusCode, body)
	}
	agent.setResult(agentrpc.DBStudioFiles.Name(), rpc.DBStudioFilesResult{
		Exports: []rpc.DBStudioFile{{Name: "site_1-20261010-120000.sql"}, {Name: "site_1-20261010-000000.sql"}},
	})
	_, body = sdo(sc, "GET", "/api/quarry/files", nil)
	if !strings.Contains(string(body), "site_1-20261010-120000.sql") || strings.Contains(string(body), "site_1-20261010-000000.sql") {
		t.Fatalf("files after its export: %s", body)
	}
	if resp, _ := sdo(sc, "GET", "/api/quarry/tasks/1", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another's task: %d", resp.StatusCode)
	}
}

// A download is streamed from the agent a chunk at a time; an upload chunk
// carries its name and offset; an SQL import from the panel needs the
// database's name typed and runs DBImport (the rollback point) with the
// job's site.
func TestStudioTransfer(t *testing.T) {
	s, agent, admin, _, site := dbStudioSetup(t)
	sc := openStudio(t, admin, site.ID)
	db := site.Config.Database.Name
	file := bytes.Repeat([]byte("0123456789"), (downloadChunk/10)+7)
	var mu sync.Mutex
	var offsets []int64
	agent.setHook(func(method string) {
		if method != agentrpc.DBStudioReadChunk.Name() {
			return
		}
		p := agent.last(method).(rpc.DBStudioChunkParams)
		mu.Lock()
		offsets = append(offsets, p.Offset)
		mu.Unlock()
		end := min(p.Offset+int64(p.Length), int64(len(file)))
		agent.mu.Lock()
		agent.results[method] = rpc.DBStudioChunkResult{Data: file[p.Offset:end], Size: int64(len(file))}
		agent.mu.Unlock()
	})
	req, _ := http.NewRequest("GET", sc.base+"/api/quarry/exports/site_1-x.sql", nil)
	for _, ck := range sc.cookies {
		req.AddCookie(ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, file) || resp.Header.Get("Content-Length") != fmt.Sprint(len(file)) ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d bytes of %d, headers %v", len(got), len(file), resp.Header)
	}
	mu.Lock()
	read := fmt.Sprint(offsets)
	mu.Unlock()
	if read != fmt.Sprintf("[0 %d]", downloadChunk) {
		t.Fatalf("chunks read at %v", offsets)
	}
	agent.setHook(nil)
	if r, _ := sdo(sc, "GET", "/api/quarry/exports/..%2Fwp-config.php", nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a path in the name: %d", r.StatusCode)
	}

	up, _ := http.NewRequest("POST", sc.base+"/api/quarry/import/upload?name=dump.sql&offset=16", strings.NewReader("chunk"))
	up.Header.Set("Content-Type", "application/octet-stream")
	for _, ck := range sc.cookies {
		up.AddCookie(ck)
	}
	resp, err = http.DefaultClient.Do(up)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %v %v", resp, err)
	}
	if p := agent.last(agentrpc.DBStudioUploadChunk.Name()).(rpc.DBStudioUploadParams); p.Name != "dump.sql" || p.Offset != 16 || string(p.Data) != "chunk" {
		t.Fatalf("sent %+v", p)
	}
	base := "/api/quarry/db/" + db
	if r, _ := sdo(sc, "POST", base+"/import", map[string]string{"name": "dump.sql", "format": "sql", "confirm": site.Domain}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("an import confirmed with the domain, not the database: %d", r.StatusCode)
	}
	if r, _ := sdo(sc, "POST", base+"/import", map[string]string{"name": "dump.sql", "format": "csv", "table": "wp_posts"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a .sql file as CSV: %d", r.StatusCode)
	}
	if r, b := sdo(sc, "POST", base+"/import", map[string]string{"name": "dump.sql", "format": "sql", "confirm": db}); r.StatusCode != http.StatusAccepted {
		t.Fatalf("import: %d %s", r.StatusCode, b)
	}
	waitForTasks(t, s)
	if p := agent.last(agentrpc.DBImport.Name()).(rpc.DBImportParams); p.RelPath != "shared/imports/dump.sql" || p.Site.ID != site.ID || p.Database != db {
		t.Fatalf("imported %+v", p)
	}
}

// The cookie: __Host- with Secure, Path=/, HttpOnly, SameSite=Strict, so a
// site on a sibling subdomain can neither set nor replace it.
func TestStudioCookie(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.setStudioCookie(w, "v", 60)
	got := w.Header().Get("Set-Cookie")
	for _, want := range []string{"__Host-cg-quarry=v", "Path=/", "HttpOnly", "Secure", "SameSite=Strict", "Max-Age=60"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Set-Cookie %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "Domain") {
		t.Fatalf("Set-Cookie %q names a domain", got)
	}
}

// An idle Quarry session ends, and so does one past its life.
func TestStudioSessionLifetime(t *testing.T) {
	s, _, anon, _ := loginSetup(t)
	at := time.Now()
	s.studio.now = func() time.Time { return at }
	resp, _ := signInStudio(anon, "reader", "reader-password-1")
	sc := &client{t: t, base: anon.base, cookies: []*http.Cookie{cookieOf(resp, insecureStudioCookie)}}
	at = at.Add(studioIdle - time.Minute)
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a session used in time: %d", resp.StatusCode)
	}
	// A background request is not a use.
	req, _ := http.NewRequest("GET", sc.base+"/api/quarry/session", nil)
	req.Header.Set(backgroundHeader, "1")
	req.AddCookie(sc.cookies[0])
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	at = at.Add(studioIdle + time.Second)
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an idle session: %d", resp.StatusCode)
	}
	resp, _ = signInStudio(anon, "reader", "reader-password-1")
	sc = &client{t: t, base: anon.base, cookies: []*http.Cookie{cookieOf(resp, insecureStudioCookie)}}
	for i := 0; i < int(studioLife/(studioIdle-time.Minute))+2; i++ {
		at = at.Add(studioIdle - time.Minute)
		sdo(sc, "GET", "/api/quarry/databases", nil)
	}
	if resp, _ := sdo(sc, "GET", "/api/quarry/databases", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session past its life: %d", resp.StatusCode)
	}
}
