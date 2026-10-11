//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// siteDatabases (Fase 9b, stream 1): a site has zero or more databases and
// database users, each user granted all or read on some of them. Static
// sites start with none; names are chosen, checked and unique; MariaDB
// enforces the grants; the agent refuses a database of another site; a
// backup holds and restore-tests every database, an empty one included;
// a staging push moves the primary; deleting a site drops all of its.
func (h *harness) siteDatabases(t *testing.T) {
	d := &dbFx{h: h, tag: fmt.Sprintf("e2e%d", h.pid%100000)}
	if !d.backupRepo(t) {
		return
	}
	defer d.restoreSettings()
	a := d.staticSite(t, "f9ba")
	b := d.staticSite(t, "f9bb")
	if a == 0 || b == 0 {
		return
	}
	d.startsEmpty(t, a)
	data, logs, empty := d.tag+"_data", d.tag+"_logs", d.tag+"_empty"
	if !d.createDB(t, a, data) || !d.createDB(t, a, logs) || !d.createDB(t, b, empty) {
		return
	}
	d.namesChecked(t, a, b, empty)
	d.grantsEnforced(t, a, data)
	d.agentRefusesForeign(t, a, empty)
	d.backupTwoDatabases(t, a, data, logs)
	d.emptyDatabaseBackup(t, b)
	d.proxyNotBackedUp(t)
	d.stagingPushesThePrimary(t)
	d.deleteDropsEverything(t, a, data, logs)
}

type dbFx struct {
	h   *harness
	tag string // a per-run prefix for names
	// restoreSettings puts the backup repository setting back.
	restoreSettings func()
}

func (d *dbFx) base(id int64) string { return fmt.Sprintf("/api/sites/%d", id) }

// backupRepo points the panel at a throwaway repository of the run's own.
func (d *dbFx) backupRepo(t *testing.T) bool {
	h := d.h
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-9b-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	_, sb := h.api.get("/api/settings")
	prev := jsonField(sb, "backup_repository")
	d.restoreSettings = h.undo("restore the backup repository setting (F9b)", func() {
		h.api.relogin()
		if prev == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prev)
		}
		_ = os.RemoveAll(brepo)
	})
	body, _ := json.Marshal(map[string]string{"backup_repository": brepo, "backup_password": bpw})
	ok := code(h.api.put("/api/settings", body)) == "200"
	h.assert(t, "F9b control: the backup repository is accepted", ok, "PUT /api/settings refused %s", brepo)
	return ok
}

func (d *dbFx) staticSite(t *testing.T, label string) int64 {
	h := d.h
	dom := h.dom(label)
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "F9b control: "+dom+" provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return 0
	}
	return id
}

type dbList struct {
	Databases []struct {
		Name      string `json:"name"`
		Primary   bool   `json:"primary"`
		SizeBytes int64  `json:"size_bytes"`
		Tables    int64  `json:"tables"`
	} `json:"databases"`
	Users []struct {
		Name   string `json:"name"`
		Grants []struct {
			Database string `json:"database"`
			Level    string `json:"level"`
		} `json:"grants"`
	} `json:"users"`
}

func (d *dbFx) list(id int64) dbList {
	var l dbList
	_, b := d.h.api.get(d.base(id) + "/databases")
	_ = json.Unmarshal(b, &l)
	return l
}

// root runs one statement as MariaDB's root.
func (d *dbFx) root(q string) string { return d.h.run("mariadb", "-N", "-e", q).Trim() }

func (d *dbFx) exists(name string) bool {
	return d.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'") == "1"
}

func (d *dbFx) userExists(name string) bool {
	return d.root("SELECT COUNT(*) FROM mysql.global_priv WHERE User='"+name+"'") != "0"
}

// startsEmpty: a static site is made with no database and no user.
func (d *dbFx) startsEmpty(t *testing.T, id int64) {
	h := d.h
	l := d.list(id)
	h.assert(t, "F9b: a static site starts with no database and no database user", len(l.Databases) == 0 && len(l.Users) == 0,
		"GET /databases = %+v", l)
	h.assert(t, "F9b: no database is made on the server for a static site", !d.exists(fmt.Sprintf("site_%d", id)),
		"site_%d exists", id)
}

func (d *dbFx) createDB(t *testing.T, id int64, name string) bool {
	h := d.h
	_, b := h.api.post(d.base(id)+"/databases", map[string]string{"name": name})
	st := h.api.waitTask(taskID(b), 3*time.Minute)
	h.check(t, "F9b: creating the database "+name+" succeeds", st, "succeeded")
	ok := d.exists(name)
	h.assert(t, "F9b: the database "+name+" is on the server", ok, "%s is not in information_schema", name)
	h.undo("drop "+name, func() { d.root("DROP DATABASE IF EXISTS `" + name + "`") })
	return st == "succeeded" && ok
}

// namesChecked: reserved, system, malformed and another site's names are
// refused; a name in use on the server is a conflict.
func (d *dbFx) namesChecked(t *testing.T, a, b int64, bsName string) {
	h := d.h
	for _, bad := range []string{"mysql", "performance_schema", "root", "Shop", "cgq_0123456789abcdef", fmt.Sprintf("site_%d", b)} {
		c, body := h.api.post(d.base(a)+"/databases", map[string]string{"name": bad})
		h.assert(t, "F9b: the database name "+bad+" is refused", c == 400, "POST /databases %q = %d %s", bad, c, head(body, 200))
		c, body = h.api.post(d.base(a)+"/database-users", map[string]any{"name": bad})
		h.assert(t, "F9b: the user name "+bad+" is refused", c == 400, "POST /database-users %q = %d %s", bad, c, head(body, 200))
	}
	c, body := h.api.post(d.base(a)+"/databases", map[string]string{"name": bsName})
	h.assert(t, "F9b: another site's database name is a conflict", c == 409, "POST /databases %q = %d %s", bsName, c, head(body, 200))
	c, body = h.api.post(d.base(a)+"/database-users", map[string]any{"name": d.tag + "_spy",
		"grants": []map[string]string{{"database": bsName, "level": "read"}}})
	h.assert(t, "F9b: a grant on another site's database is refused", c == 400, "= %d %s", c, head(body, 200))
	h.assert(t, "F9b: a refused user is not made on the server", !d.userExists(d.tag+"_spy"), "%s_spy exists", d.tag)
}

// user creates a user and writes a client defaults file with its password.
func (d *dbFx) user(t *testing.T, id int64, name string, grants map[string]string) string {
	h := d.h
	var g []map[string]string
	for db, level := range grants {
		g = append(g, map[string]string{"database": db, "level": level})
	}
	c, b := h.api.post(d.base(id)+"/database-users", map[string]any{"name": name, "grants": g})
	pw := jsonField(b, "password")
	h.assert(t, "F9b: the user "+name+" is created with a password shown once", c == 201 && len(pw) >= 24, "= %d %s", c, head(b, 200))
	h.undo("drop the user "+name, func() { d.root("DROP USER IF EXISTS '" + name + "'@'localhost'") })
	cnf := filepath.Join(t.TempDir(), name+".cnf")
	_ = os.WriteFile(cnf, []byte("[client]\nuser="+name+"\npassword="+pw+"\n"), 0o600)
	return cnf
}

func (d *dbFx) as(cnf, db, q string) result {
	return d.h.run("mariadb", "--defaults-extra-file="+cnf, "-N", "-e", q, db)
}

// grantsEnforced: MariaDB itself refuses the read user's write and accepts
// the all user's; a deleted user cannot sign in.
func (d *dbFx) grantsEnforced(t *testing.T, a int64, data string) {
	h := d.h
	reader := d.user(t, a, d.tag+"_r", map[string]string{data: "read"})
	writer := d.user(t, a, d.tag+"_w", map[string]string{data: "all"})
	w := d.as(writer, data, "CREATE TABLE t (id INT PRIMARY KEY); INSERT INTO t VALUES (1)")
	h.assert(t, "F9b: the all user writes", w.OK(), "writer: %s", w)
	r := d.as(reader, data, "SELECT COUNT(*) FROM t")
	h.check(t, "F9b: the read user reads", r.Trim(), "1")
	r = d.as(reader, data, "INSERT INTO t VALUES (2)")
	h.assert(t, "F9b: the read user cannot write", !r.OK() && strings.Contains(r.Err, "denied"), "reader insert: %s", r)
	h.check(t, "F9b: the read user's write did not happen", d.root("SELECT COUNT(*) FROM `"+data+"`.t"), "1")
	r = d.as(reader, "mysql", "SELECT 1")
	h.assert(t, "F9b: a user reaches only its granted databases", !r.OK(), "reader on mysql: %s", r)
	l := d.list(a)
	h.assert(t, "F9b: the list shows both users with their grants", len(l.Users) == 2, "users %+v", l.Users)

	// Grants change: the read user becomes all.
	c, b := h.api.put(d.base(a)+"/database-users/"+d.tag+"_r/grants", map[string]any{"grants": []map[string]string{{"database": data, "level": "all"}}})
	h.assert(t, "F9b: the read user's grant is changed", c == 200, "= %d %s", c, head(b, 200))
	h.assert(t, "F9b: after the change it writes", d.as(reader, data, "INSERT INTO t VALUES (3)").OK(), "reader insert after the grant")

	// Delete needs the name typed.
	c, _ = h.api.del(d.base(a) + "/database-users/" + d.tag + "_r")
	h.check(t, "F9b: deleting a user without confirmation is refused", fmt.Sprint(c), "400")
	c, _ = h.api.del(d.base(a) + "/database-users/" + d.tag + "_r?confirm=" + d.tag + "_r")
	h.check(t, "F9b: deleting the user succeeds", fmt.Sprint(c), "200")
	h.assert(t, "F9b: the deleted user is gone from the server", !d.userExists(d.tag+"_r"), "%s_r exists", d.tag)
	h.assert(t, "F9b: the deleted user cannot sign in", !d.as(reader, data, "SELECT 1").OK(), "the deleted user signed in")
}

// agentRefusesForeign: an agent call for site a naming site b's database,
// or a reserved one, is refused by the agent itself (its record).
func (d *dbFx) agentRefusesForeign(t *testing.T, a int64, bsName string) {
	h := d.h
	_, site := h.api.get(d.base(a))
	for _, name := range []string{bsName, "mysql"} {
		body := fmt.Sprintf(`{"site":%s,"database":%q,"sql":"SELECT 1","read_only":true}`, site, name)
		c, b, err := h.agentCall("DBQuery", body)
		h.assert(t, "F9b: the agent refuses an RPC naming "+map[bool]string{true: "another site's", false: "a reserved"}[name == bsName]+" database",
			err == nil && c != 200 && (strings.Contains(string(b), "is not a database of") || strings.Contains(string(b), "invalid database name")),
			"DBQuery %s = %d %s (%v)", name, c, head(b, 300), err)
	}
	body := fmt.Sprintf(`{"site":%s,"name":%q}`, site, bsName)
	c, b, err := h.agentCall("DropDatabase", body)
	h.assert(t, "F9b: the agent refuses to drop another site's database", err == nil && c != 200 && d.exists(bsName),
		"DropDatabase %s = %d %s (%v)", bsName, c, head(b, 300), err)
}

// backupTwoDatabases: a backup of a site with two databases stores both,
// its restore test imports both, and a restore puts both back.
func (d *dbFx) backupTwoDatabases(t *testing.T, a int64, data, logs string) {
	h := d.h
	d.root("CREATE TABLE `" + logs + "`.l (v TEXT); INSERT INTO `" + logs + "`.l VALUES ('before')")
	d.root("DELETE FROM `" + data + "`.t; INSERT INTO `" + data + "`.t VALUES (7)")
	_, b := h.api.post(d.base(a)+"/backups", `{}`)
	task := taskID(b)
	h.check(t, "F9b: the backup of a site with two databases succeeds (with its restore test)", h.api.waitTask(task, 6*time.Minute), "succeeded")
	_, rec := h.api.get(fmt.Sprintf("/api/tasks/%d", task))
	h.assert(t, "F9b: its restore test imported both databases", strings.Contains(string(rec), "2 databases imported"),
		"task record: %s", head(rec, 600))
	var rows []backupRow
	_, lb := h.api.get(d.base(a) + "/backups")
	_ = json.Unmarshal(lb, &rows)
	bk, ok := newest(rows, func(backupRow) bool { return true })
	h.check(t, "F9b: the backup passed its restore test (verify_status)", bk.VerifyStatus, "passed")
	if !ok {
		return
	}
	d.root("DROP TABLE `" + logs + "`.l; UPDATE `" + data + "`.t SET id = 8")
	_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", bk.ID), fmt.Sprintf(`{"confirm":%q,"mode":"database"}`, h.api.site(a).Domain))
	h.check(t, "F9b: the database restore succeeds", h.api.waitTask(taskID(b), 6*time.Minute), "succeeded")
	h.check(t, "F9b: the restore brought the first database back", d.root("SELECT id FROM `"+data+"`.t"), "7")
	h.check(t, "F9b: the restore brought the second database back", d.root("SELECT v FROM `"+logs+"`.l"), "before")
	d.restoreOneOfTwo(t, a, bk.ID, data, logs)
}

// restoreOneOfTwo (#15): the backup's record names both databases (what
// the panel's restore dialog offers), and `cgctl backup restore
// --database` puts back the one named and leaves the other as it is now.
func (d *dbFx) restoreOneOfTwo(t *testing.T, a, backup int64, data, logs string) {
	h := d.h
	var rows []struct {
		ID        int64    `json:"id"`
		Databases []string `json:"databases"`
	}
	_, lb := h.api.get(d.base(a) + "/backups")
	_ = json.Unmarshal(lb, &rows)
	var held []string
	for _, r := range rows {
		if r.ID == backup {
			held = r.Databases
		}
	}
	slices.Sort(held)
	want := []string{data, logs}
	slices.Sort(want)
	h.check(t, "#15: the backup's record names the databases its snapshot holds", strings.Join(held, ","), strings.Join(want, ","))

	code, b := h.api.post("/api/tokens", map[string]any{"name": fmt.Sprintf("e2e-15-%d", h.pid), "expires_in_days": 1})
	var tok struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(b, &tok)
	if code != 201 || tok.Token == "" {
		h.fail(t, "#15 control: an API token for cgctl", "= %d %s", code, head(b, 200))
		return
	}
	revoke := h.undo("revoke the #15 token", func() { h.api.del(fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID)) })
	defer revoke()
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte(tok.Token+"\n"), 0o600)

	d.root("UPDATE `" + data + "`.t SET id = 9; UPDATE `" + logs + "`.l SET v = 'after'")
	r := h.run("env", "-u", "CLOUDGROUND_EMAIL", "-u", "CLOUDGROUND_PASSWORD", "-u", "CLOUDGROUND_TOKEN", "-u", "CLOUDGROUND_CONFIG",
		"CLOUDGROUND_TOKEN_FILE="+tokenFile, cgctlBinary, "--wait", "--timeout", "10m",
		"backup", "restore", fmt.Sprint(backup), h.api.site(a).Domain, "database", "--database", logs)
	h.assert(t, "#15: cgctl backup restore --database of one database out of two succeeds", r.OK() && strings.Contains(r.Out, `"succeeded"`), "%s", r)
	h.check(t, "#15: the database named was restored", d.root("SELECT v FROM `"+logs+"`.l"), "before")
	h.check(t, "#15: the database not named kept what it had", d.root("SELECT id FROM `"+data+"`.t"), "9")
}

// emptyDatabaseBackup: an empty database is a valid backup: its restore
// test passes (it used to fail every time with "the dump imported no
// tables", and the schedule retried it every five minutes).
func (d *dbFx) emptyDatabaseBackup(t *testing.T, b int64) {
	h := d.h
	_, body := h.api.post(d.base(b)+"/backups", `{}`)
	h.check(t, "F9b: the backup of a site whose database is empty succeeds", h.api.waitTask(taskID(body), 6*time.Minute), "succeeded")
	var rows []backupRow
	_, lb := h.api.get(d.base(b) + "/backups")
	_ = json.Unmarshal(lb, &rows)
	bk, _ := newest(rows, func(backupRow) bool { return true })
	h.check(t, "F9b: an empty database's backup passes its restore test", bk.VerifyStatus, "passed")
}

// proxyNotBackedUp: a reverse-proxy site has nothing of its own to back up
// (its restore test could only find an empty tree): it is not scheduled and
// a manual backup is refused.
func (d *dbFx) proxyNotBackedUp(t *testing.T) {
	h := d.h
	dom := h.dom("f9bp")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"proxy","proxy_upstream":"http://127.0.0.1:9"}`, dom))
	if h.api.waitActive(id, 4*time.Minute) != "active" {
		h.fail(t, "F9b control: the proxy site provisions", "%s did not become active", dom)
		return
	}
	h.assert(t, "F9b: a proxy site's scheduled backups are off", !h.api.site(id).Config.Backups.Enabled, "backups enabled")
	c, b := h.api.post(d.base(id)+"/backups", `{}`)
	h.assert(t, "F9b: a manual backup of a proxy site is refused", c == 409, "= %d %s", c, head(b, 200))
	h.api.deleteSite(id)
	h.api.teardownGone(id)
}

// stagingPushesThePrimary: staging holds only the primary database, and a
// push moves a table of it into production's primary.
func (d *dbFx) stagingPushesThePrimary(t *testing.T) {
	h := d.h
	f, stg, ok := h.featureStaging(t)
	if !ok {
		return
	}
	pl, sl := d.list(f.fid), d.list(stg.ID)
	h.assert(t, "F9b: the staging copy has its primary database only", len(sl.Databases) == 1 && sl.Databases[0].Primary,
		"staging databases %+v", sl.Databases)
	if len(pl.Databases) == 0 || len(sl.Databases) == 0 {
		return
	}
	prodDB, stgDB := pl.Databases[0].Name, sl.Databases[0].Name
	for _, x := range pl.Databases {
		if x.Primary {
			prodDB = x.Name
		}
	}
	before := d.root("SELECT name FROM `" + prodDB + "`.wp_terms WHERE term_id = 1")
	defer d.root("UPDATE `" + prodDB + "`.wp_terms SET name = '" + strings.ReplaceAll(before, "'", "''") + "' WHERE term_id = 1")
	d.root("UPDATE `" + stgDB + "`.wp_terms SET name = 'cg-e2e-9b' WHERE term_id = 1")
	_, b := h.api.post(f.api("/staging/database"), fmt.Sprintf(`{"tables":["wp_terms"],"confirm":%q}`, f.fs))
	h.check(t, "F9b: the staging push of the primary succeeds", h.api.waitTask(taskID(b), 6*time.Minute), "succeeded")
	h.check(t, "F9b: the pushed table is in production's primary database", d.root("SELECT name FROM `"+prodDB+"`.wp_terms WHERE term_id = 1"), "cg-e2e-9b")
}

// deleteDropsEverything: deleting a site drops each of its databases and
// users.
func (d *dbFx) deleteDropsEverything(t *testing.T, a int64, data, logs string) {
	h := d.h
	h.check(t, "F9b control: the site with two databases is deleted", h.api.deleteSite(a), "202")
	h.check(t, "F9b control: the site is gone", h.api.teardownGone(a), "yes")
	h.assert(t, "F9b: deleting the site dropped its databases", !d.exists(data) && !d.exists(logs), "%s or %s still exists", data, logs)
	h.assert(t, "F9b: deleting the site dropped its users", !d.userExists(d.tag+"_w"), "%s_w still exists", d.tag)
}
