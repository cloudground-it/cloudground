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

// createAnswer is POST /api/sites's 202.
type createAnswer struct {
	Site    state.Site  `json:"site"`
	Summary siteSummary `json:"summary"`
	Fields  map[string]string
}

func postSite(t *testing.T, c *client, req map[string]any) (int, createAnswer, []byte) {
	t.Helper()
	resp, body := c.do("POST", "/api/sites", req)
	var a createAnswer
	json.Unmarshal(body, &a)
	var p struct {
		Fields map[string]string `json:"fields"`
	}
	json.Unmarshal(body, &p)
	a.Fields = p.Fields
	return resp.StatusCode, a, body
}

// Fase 9b/2: a WordPress site made with the database name, user and
// password its creator chose: the agent gets them, the store records them,
// the creation's answer holds the password once and GET never does.
func TestCreateWithAChosenDatabase(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	req := wordpressSite("blog.example.com")
	req["database_name"], req["database_user"], req["database_password"] = "blog_example", "blog_admin", "a chosen db pass 42"
	code, a, body := postSite(t, c, req)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d %s", code, body)
	}
	waitForTasks(t, s)
	site := a.Site
	if d := site.Config.Database; !d.Enabled || d.Name != "blog_example" || d.User != "blog_admin" {
		t.Fatalf("config.database = %+v", d)
	}
	p, ok := agent.last(agentrpc.CreateSite.Name()).(rpc.CreateSiteParams)
	if !ok || p.DBPassword != "a chosen db pass 42" || p.Site.Config.Database.Name != "blog_example" || p.Site.Config.Database.User != "blog_admin" {
		t.Fatalf("CreateSite params %+v", agent.last(agentrpc.CreateSite.Name()))
	}
	if pw, _ := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretDBPassword)); pw != "a chosen db pass 42" {
		t.Fatalf("db_password secret = %q", pw)
	}
	dbs, _ := s.Store.ListSiteDatabases(site.ID)
	users, _ := s.Store.ListDBUsers(site.ID)
	if len(dbs) != 1 || dbs[0].Name != "blog_example" || len(users) != 1 || users[0].Name != "blog_admin" || users[0].Grants[0].Database != "blog_example" {
		t.Fatalf("rows %+v %+v", dbs, users)
	}
	sum := a.Summary
	if sum.SiteURL != "https://blog.example.com/" || sum.PanelPath != fmt.Sprintf("/sites/%d", site.ID) ||
		sum.WordPress == nil || sum.WordPress.AdminURL != "https://blog.example.com/wp-admin/" || sum.WordPress.AdminUser != "owner" ||
		sum.Database == nil || sum.Database.Name != "blog_example" || sum.Database.User != "blog_admin" ||
		sum.Database.Password != "a chosen db pass 42" || sum.Database.Host != "127.0.0.1" || sum.Database.Port != 3306 ||
		sum.Database.Socket != "/run/mysqld/mysqld.sock" || sum.SFTP.Enabled || sum.SFTP.User != site.SystemUser || sum.SFTP.Host != "" {
		t.Fatalf("summary %s", body)
	}
	if !strings.HasPrefix(sum.PanelURL, "https://127.0.0.1:") || !strings.HasSuffix(sum.PanelURL, sum.PanelPath) {
		t.Fatalf("panel_url = %q", sum.PanelURL)
	}
	// The password is never answered again.
	for _, path := range []string{fmt.Sprintf("/api/sites/%d", site.ID), "/api/sites", fmt.Sprintf("/api/sites/%d/databases", site.ID), "/api/tasks"} {
		if _, got := c.do("GET", path, nil); strings.Contains(string(got), "a chosen db pass 42") {
			t.Errorf("GET %s answers the password", path)
		}
	}
}

// The panel's own names and a generated password when nothing is chosen;
// the summary still carries the password, once.
func TestCreateWithThePanelsDatabase(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	code, a, body := postSite(t, c, map[string]any{"domain": "app.example.com", "type": "laravel"})
	if code != http.StatusAccepted {
		t.Fatalf("create = %d %s", code, body)
	}
	waitForTasks(t, s)
	want := fmt.Sprintf("site_%d", a.Site.ID)
	pw, _ := s.Store.Secret(state.SiteSecret(a.Site.ID, state.SiteSecretDBPassword))
	if a.Summary.Database == nil || a.Summary.Database.Name != want || a.Summary.Database.User != want ||
		len(a.Summary.Database.Password) < 24 || a.Summary.Database.Password != pw || a.Summary.WordPress != nil {
		t.Fatalf("summary %s", body)
	}
}

// A static or PHP site gets a database when asked (create_database), with
// the chosen names; without the toggle the database fields are refused; a
// proxy never gets one; WordPress cannot opt out.
func TestCreateDatabaseToggle(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	code, a, body := postSite(t, c, map[string]any{"domain": "static.example.com", "type": "static",
		"create_database": true, "database_name": "static_data"})
	if code != http.StatusAccepted {
		t.Fatalf("static with a database = %d %s", code, body)
	}
	waitForTasks(t, s)
	want := fmt.Sprintf("site_%d", a.Site.ID)
	if d := a.Site.Config.Database; !d.Enabled || d.Name != "static_data" || d.User != want {
		t.Fatalf("config.database = %+v", d)
	}
	if p, ok := agent.last(agentrpc.CreateSite.Name()).(rpc.CreateSiteParams); !ok || p.Site.Config.Database.Name != "static_data" || p.DBPassword == "" {
		t.Fatalf("CreateSite params %+v", agent.last(agentrpc.CreateSite.Name()))
	}
	if dbs, _ := s.Store.ListSiteDatabases(a.Site.ID); len(dbs) != 1 || dbs[0].Name != "static_data" {
		t.Fatalf("rows %+v", dbs)
	}
	if a.Summary.Database == nil || a.Summary.Database.Name != "static_data" || a.Summary.WordPress != nil {
		t.Fatalf("summary %s", body)
	}
	code, a, body = postSite(t, c, map[string]any{"domain": "plain.example.com", "type": "php"})
	waitForTasks(t, s)
	if code != http.StatusAccepted || a.Site.Config.Database.Enabled || a.Summary.Database != nil {
		t.Fatalf("php without a database = %d %s", code, body)
	}

	no, yes := false, true
	for _, tc := range []struct {
		req   map[string]any
		field string
	}{
		{map[string]any{"domain": "x1.example.com", "type": "static", "database_name": "x_data"}, fieldDBName},
		{map[string]any{"domain": "x2.example.com", "type": "php", "database_password": "long enough password"}, fieldDBPassword},
		{map[string]any{"domain": "x3.example.com", "type": "proxy", "proxy_upstream": "http://127.0.0.1:3000", "create_database": yes}, fieldCreateDB},
		{withFields(wordpressSite("x4.example.com"), map[string]any{"create_database": no}), fieldCreateDB},
		{withFields(wordpressSite("x5.example.com"), map[string]any{"database_name": "mysql"}), fieldDBName},
		{withFields(wordpressSite("x6.example.com"), map[string]any{"database_name": "Blog"}), fieldDBName},
		{withFields(wordpressSite("x7.example.com"), map[string]any{"database_name": "site_99"}), fieldDBName},
		{withFields(wordpressSite("x8.example.com"), map[string]any{"database_user": "cgq_reader"}), fieldDBUser},
		{withFields(wordpressSite("x9.example.com"), map[string]any{"database_password": "short"}), fieldDBPassword},
		{withFields(wordpressSite("xa.example.com"), map[string]any{"database_password": " leading space ok?"}), fieldDBPassword},
		{withFields(wordpressSite("xb.example.com"), map[string]any{"database_password": "tab\tinside the pw"}), fieldDBPassword},
		// What wp-cli cannot write into wp-config.php as it is.
		{withFields(wordpressSite("xc.example.com"), map[string]any{"database_password": `x\'.system(chr(105)).\'`}), fieldDBPassword},
		{withFields(wordpressSite("xd.example.com"), map[string]any{"database_password": `with "double" quotes`}), fieldDBPassword},
		{withFields(wordpressSite("xe.example.com"), map[string]any{"database_password": `it's a password`}), fieldDBPassword},
		{withFields(wordpressSite("xf.example.com"), map[string]any{"database_password": `ends in a backslash\`}), fieldDBPassword},
	} {
		code, _, body := postSite(t, c, tc.req)
		var p struct {
			Fields map[string]string `json:"fields"`
		}
		json.Unmarshal(body, &p)
		if code != http.StatusBadRequest || p.Fields[tc.field] == "" {
			t.Errorf("%v = %d %s, want 400 naming %s", tc.req["domain"], code, body, tc.field)
		}
		if _, err := s.Store.GetSiteByDomain(fmt.Sprint(tc.req["domain"])); err == nil {
			t.Errorf("%v was recorded", tc.req["domain"])
		}
	}
}

func withFields(base, more map[string]any) map[string]any {
	for k, v := range more {
		base[k] = v
	}
	return base
}

// A chosen name another site has is 409 naming the field, before anything
// is written; a user name and a database name are separate.
func TestCreateWithATakenDatabaseName(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	first := withFields(wordpressSite("one.example.com"), map[string]any{"database_name": "shared_name", "database_user": "shared_user"})
	if code, _, body := postSite(t, c, first); code != http.StatusAccepted {
		t.Fatalf("first = %d %s", code, body)
	}
	waitForTasks(t, s)
	for _, tc := range []struct {
		more  map[string]any
		field string
	}{
		{map[string]any{"database_name": "shared_name"}, fieldDBName},
		{map[string]any{"database_user": "shared_user"}, fieldDBUser},
	} {
		code, a, body := postSite(t, c, withFields(wordpressSite("two.example.com"), tc.more))
		if code != http.StatusConflict || a.Fields[tc.field] == "" {
			t.Errorf("%v = %d %s, want 409 naming %s", tc.more, code, body, tc.field)
		}
		if _, err := s.Store.GetSiteByDomain("two.example.com"); err == nil {
			t.Fatalf("the refused site was recorded")
		}
	}
	// The user may be called like a database, its own or another's.
	if code, _, body := postSite(t, c, withFields(wordpressSite("three.example.com"), map[string]any{"database_name": "three_db", "database_user": "shared_name"})); code != http.StatusAccepted {
		t.Fatalf("a user named like a database = %d %s", code, body)
	}
	waitForTasks(t, s)
}

// The race the pre-check cannot see: the rows' insert finds the name
// taken, answers 409 naming the field, and leaves nothing behind.
func TestRecordPrimaryDBNamesTheTakenField(t *testing.T) {
	s, _, _ := testServer(t)
	owner, err := s.Store.CreateSite(state.Site{Domain: "owner.example.com", Type: state.SiteStatic, SystemUser: "cg-site-x", RootPath: "/srv/sites/owner.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.AddSiteDatabase(owner.ID, "raced"); err != nil {
		t.Fatal(err)
	}
	rec := state.Site{ID: owner.ID, Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "raced", User: "raced"}}}
	p := takenProblem(recordPrimaryDB(s.Store, rec))
	if p.kind != probConflict || p.fields[fieldDBName] == "" {
		t.Fatalf("problem = %+v", p)
	}
}

func TestPanelHost(t *testing.T) {
	for _, tc := range []struct {
		domain, host, want string
	}{
		{"", "203.0.113.10:8443", "203.0.113.10:8443"},
		{"panel.example.com", "panel.example.com:8443", "panel.example.com:8443"},
		{"panel.example.com", "cloudground", "panel.example.com:8443"},
		{"", "cloudground", ""},
	} {
		if got := panelHost(state.PanelSettings{PanelDomain: tc.domain}, tc.host); got != tc.want {
			t.Errorf("panelHost(%q, %q) = %q, want %q", tc.domain, tc.host, got, tc.want)
		}
	}
	on := state.Site{ID: 3, Domain: "a.example.com", SystemUser: "cg-site-3", Config: state.SiteConfig{SFTPEnabled: true}}
	if s := summaryOf(on, "", "", "", "[2001:db8::1]:8443"); s.SFTP.Host != "[2001:db8::1]" || s.SFTP.Port != 22 {
		t.Fatalf("sftp %+v", s.SFTP)
	}
	if s := summaryOf(on, "", "", "", "203.0.113.10:8443"); s.SFTP.Host != "203.0.113.10" || s.Database != nil {
		t.Fatalf("summary %+v", s)
	}
}
