//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// siteCreation (Fase 9b, stream 2): a site is created with the database
// name, user and password its creator chose. WordPress is configured with
// them and serves; MariaDB accepts the chosen account; a name another site
// has is 409 naming the field; a static site asked for a database gets
// one; the creation's answer holds the password and no read ever does;
// cgctl sites create takes the same flags and prints the summary.
func (h *harness) siteCreation(t *testing.T) {
	c := &creationFx{h: h, tag: fmt.Sprintf("e2e%d", h.pid%100000)}
	id, ok := c.wordpressWithChosenDB(t)
	if ok {
		c.takenNamesRefused(t)
		c.passwordNeverReadBack(t, id)
		c.unquotablePasswordsRefused(t, id)
	}
	c.staticWithDatabase(t)
	c.cgctlCreate(t)
}

type creationFx struct {
	h   *harness
	tag string
	// pw is the WordPress site's chosen database password.
	pw     string
	dbName string
	dbUser string
	task   int64
}

// created is the part of POST /api/sites's answer the section reads.
type created struct {
	Site struct {
		ID int64 `json:"id"`
	} `json:"site"`
	Task struct {
		ID int64 `json:"id"`
	} `json:"task"`
	Summary struct {
		SiteURL   string `json:"site_url"`
		PanelPath string `json:"panel_path"`
		WordPress *struct {
			AdminURL  string `json:"admin_url"`
			AdminUser string `json:"admin_user"`
		} `json:"wordpress"`
		Database *struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Socket   string `json:"socket"`
			Name     string `json:"name"`
			User     string `json:"user"`
			Password string `json:"password"`
		} `json:"database"`
	} `json:"summary"`
	Fields map[string]string `json:"fields"`
}

// post creates a site and, when made, deletes it at the end of the run.
func (c *creationFx) post(body map[string]any) (int, created, []byte) {
	h := c.h
	code, b := h.api.post("/api/sites", body)
	var r created
	_ = json.Unmarshal(b, &r)
	if id := r.Site.ID; id != 0 {
		h.undo(fmt.Sprintf("delete site %d (F9b creation)", id), func() {
			h.api.relogin()
			if code, _ := h.api.get(fmt.Sprintf("/api/sites/%d", id)); code == http.StatusNotFound {
				return
			}
			h.api.deleteSite(id)
			h.api.teardownGone(id)
		})
	}
	return code, r, b
}

func (c *creationFx) root(q string) string { return c.h.run("mariadb", "-N", "-e", q).Trim() }

func (c *creationFx) dbExists(name string) bool {
	return c.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'") == "1"
}

// signsIn reports whether MariaDB accepts user with password on database.
func (c *creationFx) signsIn(t *testing.T, user, password, database string) result {
	cnf := filepath.Join(t.TempDir(), user+".cnf")
	_ = os.WriteFile(cnf, []byte("[client]\nuser="+user+"\npassword=\""+password+"\"\n"), 0o600)
	return c.h.run("mariadb", "--defaults-extra-file="+cnf, "-N", "-e", "SELECT DATABASE()", database)
}

// wordpressWithChosenDB: the chosen names and password reach wp-config.php
// and MariaDB, the site serves, and the answer's summary holds them.
func (c *creationFx) wordpressWithChosenDB(t *testing.T) (int64, bool) {
	h := c.h
	dom := h.dom("f9bc")
	c.dbName, c.dbUser = c.tag+"_blog", c.tag+"_blogusr"
	// A space inside: the password reaches wp-cli on its prompt whole.
	c.pw = fmt.Sprintf("e2e chosen-pw %d.x", h.pid)
	code, r, b := c.post(map[string]any{
		"domain": dom, "type": "wordpress", "title": "F9b", "admin_email": "owner@" + dom,
		"admin_user": "owner", "admin_password": "f9b-admin-password-1",
		"database_name": c.dbName, "database_user": c.dbUser, "database_password": c.pw,
	})
	h.check(t, "F9b/2: a WordPress site with a chosen database is accepted", fmt.Sprint(code), "202")
	if r.Site.ID == 0 {
		h.fail(t, "F9b/2 control: the create answered a site", "%s", head(b, 300))
		return 0, false
	}
	c.task = r.Task.ID
	db := r.Summary.Database
	h.assert(t, "F9b/2: the creation's summary holds the chosen database, user and password",
		db != nil && db.Name == c.dbName && db.User == c.dbUser && db.Password == c.pw && db.Port == 3306 && db.Socket != "",
		"summary %s", head(b, 600))
	h.assert(t, "F9b/2: the summary names the site and the WordPress admin",
		r.Summary.SiteURL == "https://"+dom+"/" && r.Summary.WordPress != nil && r.Summary.WordPress.AdminUser == "owner" &&
			r.Summary.WordPress.AdminURL == "https://"+dom+"/wp-admin/", "summary %s", head(b, 600))
	if h.api.waitActive(r.Site.ID, 6*time.Minute) != "active" {
		h.fail(t, "F9b/2: the site with a chosen database provisions", "%s is not active", dom)
		return r.Site.ID, false
	}
	cfg, _ := os.ReadFile("/srv/sites/" + dom + "/shared/wp-config.php")
	h.check(t, "F9b/2: wp-config.php names the chosen database", WPConfigValue(string(cfg), "DB_NAME"), c.dbName)
	h.check(t, "F9b/2: wp-config.php names the chosen user", WPConfigValue(string(cfg), "DB_USER"), c.dbUser)
	h.assert(t, "F9b/2: wp-config.php holds the chosen password", WPConfigValue(string(cfg), "DB_PASSWORD") == c.pw,
		"DB_PASSWORD differs from the chosen one")
	h.assert(t, "F9b/2: the chosen database is on the server", c.dbExists(c.dbName), "%s missing", c.dbName)
	in := c.signsIn(t, c.dbUser, c.pw, c.dbName)
	h.check(t, "F9b/2: MariaDB accepts the chosen user with the chosen password", in.Trim(), c.dbName)
	st, fatal := h.serves(dom, "/")
	h.check(t, "F9b/2: the site serves its home page", st, "200")
	h.check(t, "F9b/2: the home page has no PHP fatal", fatal, "no")
	l := h.api.site(r.Site.ID)
	h.check(t, "F9b/2: the site's record names the chosen database", l.Config.Database.Name, c.dbName)
	return r.Site.ID, true
}

// takenNamesRefused: a database or user name another site has is 409
// naming the field, and nothing is made.
func (c *creationFx) takenNamesRefused(t *testing.T) {
	h := c.h
	for field, name := range map[string]string{"database_name": c.dbName, "database_user": c.dbUser} {
		dom := h.dom("f9bt")
		code, r, b := c.post(map[string]any{"domain": dom, "type": "static", "create_database": true, field: name})
		h.assert(t, "F9b/2: a taken "+field+" is 409 naming the field", code == 409 && r.Fields[field] != "",
			"= %d %s", code, head(b, 300))
		all, _ := h.api.sites()
		made := false
		for _, s := range all {
			made = made || s.Domain == dom
		}
		h.assert(t, "F9b/2: the refused site is not recorded", len(all) > 0 && !made, "%s was recorded", dom)
	}
}

// passwordNeverReadBack: no read of the site, its databases, its task or
// the audit holds the password.
func (c *creationFx) passwordNeverReadBack(t *testing.T, id int64) {
	h := c.h
	for _, path := range []string{fmt.Sprintf("/api/sites/%d", id), "/api/sites", fmt.Sprintf("/api/sites/%d/databases", id),
		fmt.Sprintf("/api/tasks/%d", c.task), "/api/tasks", "/api/audit"} {
		_, b := h.api.get(path)
		h.assert(t, "F9b/2: GET "+path+" never holds the database password", len(b) > 0 && !strings.Contains(string(b), c.pw),
			"the password is in GET %s", path)
	}
}

// unquotablePasswordsRefused: a password wp-cli cannot write into
// wp-config.php (with ' " or \; config set turns \' into \\', which ends
// the PHP string and runs the rest as code) is refused, at the creation
// naming the field and on the Database tab's reset of the primary
// account, and wp-config.php keeps the working password.
func (c *creationFx) unquotablePasswordsRefused(t *testing.T, id int64) {
	h := c.h
	bad := `e2e-pw\'.chr(65).\'`
	dom := h.dom("f9bq")
	code, r, b := c.post(map[string]any{"domain": dom, "type": "static", "create_database": true, "database_password": bad})
	h.assert(t, "F9b/2: a database password with ' \" or \\ is 400 naming the field", code == 400 && r.Fields["database_password"] != "",
		"= %d %s", code, head(b, 300))
	code, b = h.api.post(fmt.Sprintf("/api/sites/%d/database-users/%s/password", id, c.dbUser), map[string]string{"password": bad})
	h.check(t, "F9b/2: the Database tab refuses the same password for the primary account", fmt.Sprint(code), "400")
	cfg, _ := os.ReadFile("/srv/sites/" + h.api.site(id).Domain + "/shared/wp-config.php")
	h.assert(t, "F9b/2: wp-config.php keeps the working password", WPConfigValue(string(cfg), "DB_PASSWORD") == c.pw,
		"DB_PASSWORD changed (%d %s)", code, head(b, 200))
	h.check(t, "F9b/2: MariaDB still accepts the chosen password", c.signsIn(t, c.dbUser, c.pw, c.dbName).Trim(), c.dbName)
}

// staticWithDatabase: a static site asked for a database gets it and its
// user on the server, recorded as its primary.
func (c *creationFx) staticWithDatabase(t *testing.T) {
	h := c.h
	dom := h.dom("f9bs")
	name := c.tag + "_static"
	code, r, b := c.post(map[string]any{"domain": dom, "type": "static", "create_database": true, "database_name": name})
	h.check(t, "F9b/2: a static site with a database is accepted", fmt.Sprint(code), "202")
	if r.Site.ID == 0 || h.api.waitActive(r.Site.ID, 4*time.Minute) != "active" {
		h.fail(t, "F9b/2: the static site with a database provisions", "%s", head(b, 300))
		return
	}
	h.assert(t, "F9b/2: the static site's database is on the server", c.dbExists(name), "%s missing", name)
	db := r.Summary.Database
	if db == nil {
		h.fail(t, "F9b/2: the static site's summary has its database", "%s", head(b, 400))
		return
	}
	h.check(t, "F9b/2: its generated password signs its user in", c.signsIn(t, db.User, db.Password, name).Trim(), name)
	var l dbList
	_, lb := h.api.get(fmt.Sprintf("/api/sites/%d/databases", r.Site.ID))
	_ = json.Unmarshal(lb, &l)
	h.assert(t, "F9b/2: the static site lists the database as its primary", len(l.Databases) == 1 && l.Databases[0].Name == name && l.Databases[0].Primary,
		"databases %s", head(lb, 300))
}

// cgctlCreate: cgctl sites create with the database flags makes the site
// and prints the summary with the password.
func (c *creationFx) cgctlCreate(t *testing.T) {
	h := c.h
	code, b := h.api.post("/api/tokens", map[string]any{"name": fmt.Sprintf("e2e-f9b-%d", h.pid), "expires_in_days": 1})
	var tok struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(b, &tok)
	if code != 201 || tok.Token == "" {
		h.fail(t, "F9b/2 control: an API token for cgctl", "= %d %s", code, head(b, 200))
		return
	}
	h.undo("revoke the F9b token", func() { h.api.del(fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID)) })
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	_ = os.WriteFile(tokenFile, []byte(tok.Token+"\n"), 0o600)
	pw := fmt.Sprintf("cgctl-chosen-%d-pw", h.pid)
	dom := h.dom("f9bcli")
	name := c.tag + "_cli"
	cgctlEnv := []string{"-u", "CLOUDGROUND_EMAIL", "-u", "CLOUDGROUND_PASSWORD", "-u", "CLOUDGROUND_TOKEN", "-u", "CLOUDGROUND_CONFIG",
		"CLOUDGROUND_TOKEN_FILE=" + tokenFile, cgctlBinary}
	// An empty stdin is refused before anything reaches the panel.
	empty := h.run("env", append(cgctlEnv, "sites", "create", h.dom("f9bnopw"), "php", "--db-password-stdin")...)
	h.assert(t, "F9b/2: cgctl --db-password-stdin with nothing on stdin is a usage error",
		empty.Code == 2 && strings.Contains(empty.Err, "no password on stdin"), "%s", empty)
	// The password travels on stdin, never in argv (ps, shell history).
	r := h.runIn(pw+"\n", "env", append(cgctlEnv, "--wait", "--timeout", "5m",
		"sites", "create", dom, "php", "--with-database", "--db-name", name, "--db-password-stdin")...)
	all, _ := h.api.sites()
	for _, s := range all {
		if s.Domain == dom {
			id := s.ID
			h.undo("delete the cgctl site", func() { h.api.relogin(); h.api.deleteSite(id); h.api.teardownGone(id) })
		}
	}
	h.assert(t, "F9b/2: cgctl sites create with the database flags succeeds", r.OK() && strings.Contains(r.Out, "Created."), "%s", r)
	h.assert(t, "F9b/2: cgctl prints the summary with the database name and password",
		strings.Contains(r.Out, "Name:        "+name) && strings.Contains(r.Out, "Password:    "+pw) && strings.Contains(r.Out, "https://"+dom+"/"),
		"%s", r)
	h.assert(t, "F9b/2: the database cgctl asked for is on the server", c.dbExists(name), "%s missing", name)
}
