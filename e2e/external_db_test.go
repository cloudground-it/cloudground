//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	extPort  = 3307
	extAdmin = "cgadmin"
	extPW    = "e2e-admin-password-1"
)

// extDB is a second MariaDB on the box, on its own port and data directory,
// started for the section: the "external" server the panel registers. The
// panel's own MariaDB is not touched.
type extDB struct {
	h    *harness
	dir  string
	sock string
}

func (e *extDB) run(q string) string {
	return e.h.run("mariadb", "--no-defaults", "--socket="+e.sock, "-uroot", "-N", "-e", q).Trim()
}

// as runs a statement as a database user, over TCP like the application.
func (e *extDB) as(user, pw, db, q string) result {
	return e.h.run("env", "MYSQL_PWD="+pw, "mariadb", "--no-defaults", "--protocol=tcp", "-h127.0.0.1", fmt.Sprintf("-P%d", extPort),
		"-u"+user, "-N", "-e", q, db)
}

// start installs and starts the instance, and makes its administrator. The
// server binary is a copy at a path of its own, which no AppArmor profile
// of the distribution's mariadbd covers, and the copy is also what
// mariadb-install-db runs (through a basedir of its own: the distribution's
// profile denies the install a data directory outside /var/lib/mysql).
func (e *extDB) start(t *testing.T) bool {
	h := e.h
	e.dir = "/var/lib/cg-e2e-extdb"
	e.sock = e.dir + "/ext.sock"
	h.run("systemctl", "stop", "cg-e2e-extdb.service")
	h.remove(e.dir)
	bin := strings.TrimSpace(h.run("sh", "-c", "command -v mariadbd || command -v mysqld").Out)
	install := strings.TrimSpace(h.run("sh", "-c", "command -v mariadb-install-db || command -v mysql_install_db").Out)
	if bin == "" || install == "" {
		h.fail(t, "#30 control: MariaDB's server and install tools are on the box", "mariadbd %q, install-db %q", bin, install)
		return false
	}
	// The server binary under the name the install looks for, the shared files
	// and the two tools it calls beside it.
	if r := h.run("sh", "-c", fmt.Sprintf("mkdir -p %[1]s/tmp %[1]s/base/sbin %[1]s/base/bin && cp -p %[2]s %[1]s/base/sbin/mariadbd && ln -s /usr/share %[1]s/base/share && "+
		"for t in my_print_defaults resolveip; do p=$(command -v $t) && ln -s $p %[1]s/base/bin/$t; done; chown -R mysql:mysql %[1]s", e.dir, bin)); !r.OK() {
		h.fail(t, "#30 control: the second instance's directory is made", "%s", r)
		return false
	}
	h.undo("stop and remove the second MariaDB instance (#30)", func() {
		h.run("systemctl", "stop", "cg-e2e-extdb.service")
		h.remove(e.dir)
	})
	if r := h.run(install, "--no-defaults", "--basedir="+e.dir+"/base", "--datadir="+e.dir+"/data", "--user=mysql", "--auth-root-authentication-method=normal", "--skip-test-db"); !r.OK() {
		h.fail(t, "#30 control: the second instance's data directory is initialised", "%s", r)
		return false
	}
	if r := h.run("systemd-run", "--unit=cg-e2e-extdb", "--uid=mysql", "--collect", e.dir+"/base/sbin/mariadbd", "--no-defaults",
		"--datadir="+e.dir+"/data", "--tmpdir="+e.dir+"/tmp", "--socket="+e.sock, "--pid-file="+e.dir+"/ext.pid", "--port="+fmt.Sprint(extPort),
		"--bind-address=127.0.0.1", "--skip-name-resolve", "--innodb-buffer-pool-size=64M", "--performance-schema=OFF"); !r.OK() {
		h.fail(t, "#30 control: the second instance starts", "%s", r)
		return false
	}
	up := false
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if h.run("mariadb", "--no-defaults", "--socket="+e.sock, "-uroot", "-e", "SELECT 1").OK() {
			up = true
			break
		}
	}
	h.assert(t, "#30 control: the second instance answers", up, "%s", head([]byte(h.run("journalctl", "-u", "cg-e2e-extdb", "-n", "20", "--no-pager").Out), 800))
	if !up {
		return false
	}
	e.run(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%' WITH GRANT OPTION", extAdmin, extPW, extAdmin))
	return true
}

// externalDatabaseServer (#30): a second MariaDB, registered in the panel,
// holds a database and a user of a site: made there and not on the box's own
// server, reachable by the user over TCP with its own password and nothing
// else, dumped over the network by an export and a backup (whose restore test
// passes), restored into the same server, and dropped there; a wrong password
// is refused at registration and a server with a database on it cannot be
// removed.
func (h *harness) externalDatabaseServer(t *testing.T) {
	ext := &extDB{h: h}
	if !ext.start(t) {
		return
	}
	d := &dbFx{h: h, tag: fmt.Sprintf("e30%d", h.pid%100000)}
	if !d.backupRepo(t) {
		return
	}
	defer d.restoreSettings()
	id := d.staticSite(t, "ext30")
	if id == 0 {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()

	// A wrong password is refused and nothing is registered.
	c, b := h.api.post("/api/db-servers", map[string]any{"name": "cge2e-ext", "host": "127.0.0.1", "port": extPort, "tls": "off",
		"admin_user": extAdmin, "admin_password": "not-the-password"})
	h.assert(t, "#30: a server whose password is wrong is refused at registration, with the server's words", c == 400 && strings.Contains(string(b), "denied"),
		"= %d %s", c, head(b, 300))
	// H-5-1: the cloud metadata address is never a database server; the
	// administrator's credentials go nowhere near it.
	c, b = h.api.post("/api/db-servers", map[string]any{"name": "cge2e-meta", "host": "169.254.169.254", "port": 3306, "tls": "off",
		"admin_user": extAdmin, "admin_password": extPW})
	h.assert(t, "H-5-1: a link-local host (the metadata address) is refused at registration", c == 400 && strings.Contains(string(b), "never connects there"),
		"= %d %s", c, head(b, 300))
	_, lb := h.api.get("/api/db-servers")
	h.assert(t, "#30: and none is stored", strings.TrimSpace(string(lb)) == "[]" || (!strings.Contains(string(lb), "cge2e-ext") && !strings.Contains(string(lb), "cge2e-meta")), "%s", head(lb, 300))

	c, b = h.api.post("/api/db-servers", map[string]any{"name": "cge2e-ext", "host": "127.0.0.1", "port": extPort, "tls": "off",
		"admin_user": extAdmin, "admin_password": extPW})
	var srv struct {
		ID          int64 `json:"id"`
		PasswordSet bool  `json:"password_set"`
	}
	_ = json.Unmarshal(b, &srv)
	h.assert(t, "#30: the server is registered", c == 201 && srv.ID > 0 && srv.PasswordSet, "= %d %s", c, head(b, 300))
	if srv.ID == 0 {
		return
	}
	defer h.api.del(fmt.Sprintf("/api/db-servers/%d", srv.ID))
	h.assert(t, "#30: the answer never has the password", !strings.Contains(string(b), extPW), "%s", head(b, 300))
	_, lb = h.api.get("/api/db-servers")
	h.assert(t, "#30: nor the list", !strings.Contains(string(lb), extPW), "%s", head(lb, 300))
	c, tb := h.api.post(fmt.Sprintf("/api/db-servers/%d/test", srv.ID), map[string]any{})
	h.assert(t, "#30: the panel can test it, and the server answers its version", c == 200 && strings.Contains(string(tb), `"version"`), "= %d %s", c, head(tb, 300))

	// A database on it.
	name, user := d.tag+"_ext", d.tag+"_extu"
	c, b = h.api.post(d.base(id)+"/databases", map[string]any{"name": name, "server_id": srv.ID})
	h.check(t, "#30: creating a database on the external server is accepted", fmt.Sprint(c), "202")
	h.check(t, "#30: its task succeeds", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	h.check(t, "#30: the database is on the external server", ext.run("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'"), "1")
	h.check(t, "#30: and not on the panel's own server", d.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'"), "0")
	l := d.list(id)
	var onServer bool
	for _, db := range l.Databases {
		onServer = onServer || db.Name == name
	}
	h.assert(t, "#30: the site's list has it", onServer, "%+v", l.Databases)
	_, raw := h.api.get(d.base(id) + "/databases")
	h.assert(t, "#30: the list says where an application connects (the host and port)", strings.Contains(string(raw), `"127.0.0.1"`) && strings.Contains(string(raw), fmt.Sprint(extPort)), "%s", head(raw, 500))

	// A user on it, and what it may do over TCP.
	c, b = h.api.post(d.base(id)+"/database-users", map[string]any{"name": user, "grants": []map[string]string{{"database": name, "level": "all"}}})
	pw := jsonField(b, "password")
	h.assert(t, "#30: the user is created with a password shown once", c == 201 && len(pw) >= 24, "= %d %s", c, head(b, 300))
	h.check(t, "#30: the user is on the external server, for any host", ext.run("SELECT Host FROM mysql.user WHERE User='"+user+"'"), "%")
	h.check(t, "#30: and not on the panel's own", d.root("SELECT COUNT(*) FROM mysql.global_priv WHERE User='"+user+"'"), "0")
	w := ext.as(user, pw, name, "CREATE TABLE t (id INT PRIMARY KEY); INSERT INTO t VALUES (7)")
	h.assert(t, "#30: the user signs in over TCP with its own password and writes", w.OK(), "%s", w)
	r := ext.as(user, pw, "mysql", "SELECT 1")
	h.assert(t, "#30: it reaches only its own database", !r.OK(), "on mysql: %s", r)
	r = ext.as(user, "not-its-password", name, "SELECT 1")
	h.assert(t, "#30: a wrong password is refused", !r.OK(), "%s", r)

	// The console (a scoped account made on the server) and an export.
	c, b = h.api.post(d.base(id)+"/database/query", map[string]any{"database": name, "sql": "SELECT id FROM t"})
	h.assert(t, "#30: the console reads the table on the external server", c == 200 && strings.Contains(string(b), `"7"`), "= %d %s", c, head(b, 300))
	h.check(t, "#30: the console's scoped account is gone from the external server afterwards",
		ext.run("SELECT COUNT(*) FROM mysql.global_priv WHERE User LIKE 'cgq\\_%'"), "0")
	c, b = h.api.post(d.base(id)+"/database/export", map[string]any{"database": name})
	h.check(t, "#30: an export of the database is accepted", fmt.Sprint(c), "202")
	h.check(t, "#30: its task succeeds", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	// The dump is in the site's shared/exports, named after the database.
	exp := h.run("sh", "-c", "ls -t /srv/sites/"+h.api.site(id).Domain+"/shared/exports/"+name+"-*.sql 2>/dev/null | head -1").Trim()
	h.assert(t, "#30: the export is a file of the site", exp != "", "no %s-*.sql under shared/exports", name)
	if exp != "" {
		dump, _ := os.ReadFile(exp)
		h.assert(t, "#30: it holds the rows dumped over the network", regexp.MustCompile("INSERT INTO `t` (\\(`id`\\) )?VALUES\\s*\\(7\\)").Match(dump), "%s", head(dump[max(0, len(dump)-700):], 700))
	}

	// F-5-1: a tenant's import into its own database on the external server
	// runs as a one-database account, never the server's administrator, so a
	// dump that tries to touch another database or the server itself has no
	// effect. A second database on the same server stands in for a neighbour.
	victim, evil := d.tag+"_victim", "cge2e_evil30"
	ext.run("CREATE DATABASE `" + victim + "`; CREATE TABLE `" + victim + "`.keep (id INT PRIMARY KEY); INSERT INTO `" + victim + "`.keep VALUES (1)")
	defer ext.run("DROP DATABASE IF EXISTS `" + victim + "`; DROP USER IF EXISTS '" + evil + "'@'%'")
	siteRoot := "/srv/sites/" + h.api.site(id).Domain
	crafted := "CREATE USER IF NOT EXISTS '" + evil + "'@'%' IDENTIFIED BY 'x';\n" +
		"DROP DATABASE IF EXISTS `" + victim + "`;\n" +
		"CREATE TABLE cg_f51_marker (id INT);\n"
	h.runIn(crafted, "tee", siteRoot+"/shared/cg-f51-crafted.sql")
	_, b = h.api.post(d.base(id)+"/database/import", map[string]any{"database": name, "path": "shared/cg-f51-crafted.sql", "confirm": h.api.site(id).Domain})
	h.api.waitTask(taskID(b), 3*time.Minute) // succeed or fail: the effect is what matters
	h.check(t, "#30 F-5-1: the import did not create a privileged user on the server", ext.run("SELECT COUNT(*) FROM mysql.user WHERE User='"+evil+"'"), "0")
	h.check(t, "#30 F-5-1: and did not drop the neighbour's database", ext.run("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+victim+"'"), "1")
	h.check(t, "#30 F-5-1: the neighbour's row is untouched", ext.run("SELECT id FROM `"+victim+"`.keep"), "1")
	// A clean dump with a trigger imports, and the trigger fires afterwards:
	// it is owned by the site's own account (the import ran as it), not a
	// dropped temporary one.
	clean := "CREATE TABLE cg_f51_ok (id INT PRIMARY KEY, n INT);\n" +
		"CREATE TRIGGER cg_f51_trg BEFORE INSERT ON cg_f51_ok FOR EACH ROW SET NEW.n = 100;\n" +
		"INSERT INTO cg_f51_ok (id) VALUES (42);\n"
	h.runIn(clean, "tee", siteRoot+"/shared/cg-f51-ok.sql")
	_, b = h.api.post(d.base(id)+"/database/import", map[string]any{"database": name, "path": "shared/cg-f51-ok.sql", "confirm": h.api.site(id).Domain})
	h.check(t, "#30 F-5-1: a clean import into the site's own database succeeds", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	h.check(t, "#30 F-5-1: its row is there (the trigger set n on import)", ext.run("SELECT n FROM `"+name+"`.cg_f51_ok WHERE id=42"), "100")
	// The trigger still fires on a new write: it is owned by a live account.
	ext.as(user, pw, name, "INSERT INTO cg_f51_ok (id) VALUES (7)")
	h.check(t, "#30 F-5-1: the imported trigger still fires (owned by the site account, not a dropped one)", ext.run("SELECT n FROM `"+name+"`.cg_f51_ok WHERE id=7"), "100")
	ext.run("DROP DATABASE IF EXISTS `" + victim + "`; DROP USER IF EXISTS '" + evil + "'@'%'")
	h.run("rm", "-f", siteRoot+"/shared/cg-f51-crafted.sql", siteRoot+"/shared/cg-f51-ok.sql")

	// A backup (with its restore test) and a restore into the same server.
	_, b = h.api.post(d.base(id)+"/backups", `{}`)
	task := taskID(b)
	h.check(t, "#30: the backup of a site whose database is on the external server succeeds", h.api.waitTask(task, 6*time.Minute), "succeeded")
	var rows []backupRow
	_, bl := h.api.get(d.base(id) + "/backups")
	_ = json.Unmarshal(bl, &rows)
	bk, ok := newest(rows, func(backupRow) bool { return true })
	h.check(t, "#30: its restore test passed", bk.VerifyStatus, "passed")
	if ok {
		ext.run("DELETE FROM `" + name + "`.t; INSERT INTO `" + name + "`.t VALUES (99)")
		_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", bk.ID), fmt.Sprintf(`{"confirm":%q,"mode":"database"}`, h.api.site(id).Domain))
		h.check(t, "#30: the database restore succeeds", h.api.waitTask(taskID(b), 6*time.Minute), "succeeded")
		h.check(t, "#30: the restore put the rows back on the external server", ext.run("SELECT id FROM `"+name+"`.t"), "7")
		h.check(t, "#30: the user still reads them (its grant survived)", strings.TrimSpace(ext.as(user, pw, name, "SELECT id FROM t").Out), "7")
		h.check(t, "#30: the panel's own server still has no such database", d.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'"), "0")
		h.check(t, "#30: no scratch database is left on the external server", ext.run("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'cg_\\_%'"), "0")
	}

	// The server in use cannot go; after the database is dropped it can.
	c, _ = h.api.del(fmt.Sprintf("/api/db-servers/%d", srv.ID))
	h.check(t, "#30: a server with a database on it cannot be removed", fmt.Sprint(c), "409")
	c, _ = h.api.del(d.base(id) + "/database-users/" + user + "?confirm=" + user)
	h.check(t, "#30: the user is deleted", fmt.Sprint(c), "200")
	h.check(t, "#30: and is gone from the external server", ext.run("SELECT COUNT(*) FROM mysql.user WHERE User='"+user+"'"), "0")
	c, b = h.api.del(d.base(id) + "/databases/" + name + "?confirm=" + name)
	h.check(t, "#30: dropping the database is accepted", fmt.Sprint(c), "202")
	h.check(t, "#30: its task succeeds", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	h.check(t, "#30: the database is gone from the external server", ext.run("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'"), "0")
	c, _ = h.api.del(fmt.Sprintf("/api/db-servers/%d", srv.ID))
	h.check(t, "#30: and then the server can be removed", fmt.Sprint(c), "200")
}
