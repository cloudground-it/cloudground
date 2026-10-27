//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	pgPort  = 5433
	pgAdmin = "cgadmin"
	pgPW    = "e2e-pg-admin-password-1"
	pgDir   = "/var/lib/cg-e2e-extpg"
)

// extPG is a second PostgreSQL cluster on the box, on its own port and
// data directory, with TLS from a certificate made for the section (its own
// certificate is the CA the panel is given): the "external" server the panel
// registers. Its administrator may create roles and databases and nothing
// more, as on a managed service. The distribution's own cluster is not
// touched.
type extPG struct {
	h       *harness
	version string
	crt     string
}

// psql runs a statement on the cluster as the postgres operating system user.
func (e *extPG) admin(db, q string) string {
	return e.h.run("runuser", "-u", "postgres", "--", "psql", "-p", fmt.Sprint(pgPort), "-d", db, "-tAqc", q).Trim()
}

// as runs a statement as a database account, over TCP with the certificate
// verified, like the application.
func (e *extPG) as(user, pw, db, q string) result {
	return e.h.run("env", "PGPASSWORD="+pw, "PGSSLMODE=verify-full", "PGSSLROOTCERT="+e.crt, "psql", "-h", "127.0.0.1", "-p", fmt.Sprint(pgPort),
		"-U", user, "-d", db, "-tAqc", q)
}

func (e *extPG) start(t *testing.T) bool {
	h := e.h
	e.crt = pgDir + "/server.crt"
	e.version = strings.TrimSpace(h.run("sh", "-c", "ls /usr/lib/postgresql 2>/dev/null | sort -n | tail -1").Out)
	if e.version == "" {
		hadClient := h.run("dpkg", "-s", "postgresql-client-common").OK()
		h.undo("remove PostgreSQL if this run installed it (#81)", func() {
			if !hadClient {
				h.run("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "purge", "-y", "-qq", "postgresql", "postgresql-client", "postgresql-common", "postgresql-client-common")
			}
		})
		if r := h.run("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-qq", "postgresql", "postgresql-client", "openssl"); !r.OK() {
			h.fail(t, "#81 control: PostgreSQL's server and client are installed", "%s", r)
			return false
		}
		e.version = strings.TrimSpace(h.run("sh", "-c", "ls /usr/lib/postgresql | sort -n | tail -1").Out)
	}
	if e.version == "" {
		h.fail(t, "#81 control: a PostgreSQL version is on the box", "none under /usr/lib/postgresql")
		return false
	}
	h.run("pg_dropcluster", "--stop", e.version, "cge2e")
	h.remove(pgDir)
	h.undo("stop and remove the second PostgreSQL cluster (#81)", func() {
		h.run("pg_dropcluster", "--stop", e.version, "cge2e")
		h.remove(pgDir)
	})
	if r := h.run("sh", "-c", fmt.Sprintf("mkdir -p %[1]s && openssl req -x509 -newkey rsa:2048 -nodes -keyout %[1]s/server.key -out %[1]s/server.crt -days 2 "+
		"-subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 && chown postgres:postgres %[1]s/server.key %[1]s/server.crt && chmod 600 %[1]s/server.key", pgDir)); !r.OK() {
		h.fail(t, "#81 control: the second cluster's certificate is made", "%s", r)
		return false
	}
	if r := h.run("pg_createcluster", e.version, "cge2e", "-d", pgDir+"/data", "-p", fmt.Sprint(pgPort), "--start-conf=manual",
		"-o", "listen_addresses=127.0.0.1", "-o", "ssl=on", "-o", "ssl_cert_file="+pgDir+"/server.crt", "-o", "ssl_key_file="+pgDir+"/server.key"); !r.OK() {
		h.fail(t, "#81 control: the second cluster is created", "%s", r)
		return false
	}
	if r := h.run("pg_ctlcluster", e.version, "cge2e", "start"); !r.OK() {
		h.fail(t, "#81 control: the second cluster starts", "%s", r)
		return false
	}
	up := false
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if h.run("pg_isready", "-h", "127.0.0.1", "-p", fmt.Sprint(pgPort)).OK() {
			up = true
			break
		}
	}
	h.assert(t, "#81 control: the second cluster answers", up, "%s", head([]byte(h.run("journalctl", "-u", "postgresql@"+e.version+"-cge2e", "-n", "20", "--no-pager").Out), 800))
	if !up {
		return false
	}
	e.admin("postgres", fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' CREATEROLE CREATEDB", pgAdmin, pgPW))
	return true
}

// externalPostgresServer (#81): a second PostgreSQL cluster with TLS,
// registered in the panel, holds the databases and accounts of a PHP site:
// registration refuses a wrong password, an untrusted certificate and no TLS,
// and a static site is not offered it; a database made on it is owned by its
// own owner role, an account with all privileges creates tables the owner
// owns, a read account reads what exists and what is made later and writes
// nothing, and neither reaches another database; the SQL console says it is
// not there yet; an export holds the rows; a backup of a server CloudGround
// dumps passes its restore test (a scratch database and its roles gone
// afterwards) and a restore puts the rows back; a server the provider backs
// up has its databases left out of a backup; accounts and database are
// dropped from the server and then the server can be removed.
func (h *harness) externalPostgresServer(t *testing.T) {
	pg := &extPG{h: h}
	if !pg.start(t) {
		return
	}
	d := &dbFx{h: h, tag: fmt.Sprintf("e81%d", h.pid%100000)}
	if !d.backupRepo(t) {
		return
	}
	defer d.restoreSettings()
	dom := h.dom("pg81")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#81 control: the PHP site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	static := d.staticSite(t, "pgs81")
	if static != 0 {
		defer func() {
			h.api.deleteSite(static)
			h.api.teardownGone(static)
		}()
	}
	crt, _ := os.ReadFile(pg.crt)

	// Registration: what is refused, with the server's words, and stored nowhere.
	reg := func(extra map[string]any) (int, []byte) {
		body := map[string]any{"kind": "postgres", "name": "cge2e-pg", "host": "127.0.0.1", "port": pgPort, "tls": "required",
			"ca_cert": string(crt), "admin_user": pgAdmin, "admin_password": pgPW, "backup": "dump"}
		for k, v := range extra {
			body[k] = v
		}
		return h.api.post("/api/db-servers", body)
	}
	c, b := reg(map[string]any{"admin_password": "not-the-password"})
	h.assert(t, "#81: a server whose password is wrong is refused at registration, with the server's words", c == 400 && strings.Contains(string(b), "password authentication failed"),
		"= %d %s", c, head(b, 300))
	c, b = reg(map[string]any{"ca_cert": ""})
	h.assert(t, "#81: a server whose certificate is not trusted is refused", c == 400 && strings.Contains(strings.ToLower(string(b)), "certificate"),
		"= %d %s", c, head(b, 300))
	c, b = reg(map[string]any{"tls": "off"})
	h.assert(t, "#81: PostgreSQL without TLS is refused", c == 400 && strings.Contains(string(b), "TLS"), "= %d %s", c, head(b, 300))
	_, lb := h.api.get("/api/db-servers")
	h.assert(t, "#81: and none is stored", !strings.Contains(string(lb), "cge2e-pg"), "%s", head(lb, 300))

	c, b = reg(nil)
	var srv struct {
		ID          int64  `json:"id"`
		Kind        string `json:"kind"`
		Backup      string `json:"backup"`
		PasswordSet bool   `json:"password_set"`
	}
	_ = json.Unmarshal(b, &srv)
	h.assert(t, "#81: the server is registered as PostgreSQL, dumped by CloudGround", c == 201 && srv.ID > 0 && srv.PasswordSet && srv.Kind == "postgres" && srv.Backup == "dump",
		"= %d %s", c, head(b, 300))
	if srv.ID == 0 {
		return
	}
	defer h.api.del(fmt.Sprintf("/api/db-servers/%d", srv.ID))
	h.assert(t, "#81: the answer never has the password", !strings.Contains(string(b), pgPW), "%s", head(b, 300))
	_, lb = h.api.get("/api/db-servers")
	h.assert(t, "#81: nor the list", !strings.Contains(string(lb), pgPW), "%s", head(lb, 300))
	c, tb := h.api.post(fmt.Sprintf("/api/db-servers/%d/test", srv.ID), map[string]any{})
	h.assert(t, "#81: the panel can test it, and the server answers its version", c == 200 && strings.Contains(string(tb), "PostgreSQL"), "= %d %s", c, head(tb, 300))

	// A static site is not offered PostgreSQL.
	if static != 0 {
		c, b = h.api.post(d.base(static)+"/databases", map[string]any{"name": d.tag + "_st", "server_id": srv.ID})
		h.assert(t, "#81: a static site cannot put a database on a PostgreSQL server", c == 400 && strings.Contains(string(b), "PHP and Laravel"), "= %d %s", c, head(b, 300))
	}

	// Two databases and their owner roles.
	name, other, rw, ro := d.tag+"_pg", d.tag+"_pg2", d.tag+"_pgu", d.tag+"_pgr"
	for _, n := range []string{name, other} {
		c, b = h.api.post(d.base(id)+"/databases", map[string]any{"name": n, "server_id": srv.ID})
		h.check(t, "#81: creating "+n+" on the PostgreSQL server is accepted", fmt.Sprint(c), "202")
		h.check(t, "#81: its task succeeds", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	}
	h.check(t, "#81: the database is on the PostgreSQL server", pg.admin("postgres", "SELECT count(*) FROM pg_database WHERE datname='"+name+"'"), "1")
	h.check(t, "#81: owned by its own owner role", pg.admin("postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='"+name+"'"), "cgo_"+name)
	h.check(t, "#81: and not on the panel's own MariaDB", d.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+name+"'"), "0")
	_, raw := h.api.get(d.base(id) + "/databases")
	h.assert(t, "#81: the list says where an application connects, and that it is PostgreSQL", strings.Contains(string(raw), "127.0.0.1") && strings.Contains(string(raw), fmt.Sprint(pgPort)) && strings.Contains(string(raw), `"postgres"`),
		"%s", head(raw, 500))

	// An account with all privileges on one database.
	c, b = h.api.post(d.base(id)+"/database-users", map[string]any{"name": rw, "grants": []map[string]string{{"database": name, "level": "all"}}})
	rwPW := jsonField(b, "password")
	h.assert(t, "#81: the account is created with a password shown once", c == 201 && len(rwPW) >= 24, "= %d %s", c, head(b, 300))
	h.check(t, "#81: it is a role of the PostgreSQL server", pg.admin("postgres", "SELECT count(*) FROM pg_roles WHERE rolname='"+rw+"' AND rolcanlogin"), "1")
	w := pg.as(rw, rwPW, name, "CREATE TABLE t (id int PRIMARY KEY); INSERT INTO t VALUES (7)")
	h.assert(t, "#81: it signs in over TLS with its own password and creates a table", w.OK(), "%s", w)
	h.check(t, "#81: the table is the owner role's", pg.admin(name, "SELECT tableowner FROM pg_tables WHERE tablename='t'"), "cgo_"+name)
	r := pg.as(rw, rwPW, other, "SELECT 1")
	h.assert(t, "#81: it cannot connect to the other database", !r.OK(), "%s", r)
	r = pg.as(rw, "not-its-password", name, "SELECT 1")
	h.assert(t, "#81: a wrong password is refused", !r.OK(), "%s", r)

	// A read account reads what exists and what is made later, and writes nothing.
	c, b = h.api.post(d.base(id)+"/database-users", map[string]any{"name": ro, "grants": []map[string]string{{"database": name, "level": "read"}}})
	roPW := jsonField(b, "password")
	h.assert(t, "#81: a read account is created", c == 201 && roPW != "", "= %d %s", c, head(b, 300))
	h.check(t, "#81: it reads the table made before it", strings.TrimSpace(pg.as(ro, roPW, name, "SELECT id FROM t").Out), "7")
	r = pg.as(ro, roPW, name, "INSERT INTO t VALUES (8)")
	h.assert(t, "#81: it cannot write", !r.OK(), "%s", r)
	pg.as(rw, rwPW, name, "CREATE TABLE later (v text); INSERT INTO later VALUES ('x')")
	h.check(t, "#81: it reads a table made after it", strings.TrimSpace(pg.as(ro, roPW, name, "SELECT v FROM later").Out), "x")
	r = pg.as(ro, roPW, other, "SELECT 1")
	h.assert(t, "#81: and cannot connect to the other database either", !r.OK(), "%s", r)

	// The SQL console is not there yet; an export is.
	c, b = h.api.post(d.base(id)+"/database/query", map[string]any{"database": name, "sql": "SELECT id FROM t", "read_only": true})
	h.assert(t, "#81: the SQL console says it is not available for PostgreSQL yet", c == 400 && strings.Contains(string(b), "PostgreSQL"), "= %d %s", c, head(b, 300))
	c, b = h.api.post(d.base(id)+"/database/export", map[string]any{"database": name})
	exp := jsonField(b, "path")
	h.assert(t, "#81: an export of the database is made", c == 200 && exp != "", "= %d %s", c, head(b, 300))
	if exp != "" {
		dump, _ := os.ReadFile(exp)
		h.assert(t, "#81: it holds the rows dumped over TLS", strings.Contains(string(dump), "COPY public.t") && strings.Contains(string(dump), "\n7\n"), "%s", head(dump, 600))
	}

	// A backup of a server CloudGround dumps, its restore test, and a restore.
	_, b = h.api.post(d.base(id)+"/backups", `{}`)
	task := taskID(b)
	h.check(t, "#81: the backup of a site whose database is on the PostgreSQL server succeeds", h.api.waitTask(task, 6*time.Minute), "succeeded")
	_, rec := h.api.get(fmt.Sprintf("/api/tasks/%d", task))
	h.assert(t, "#81: its restore test imported the databases", strings.Contains(string(rec), "databases imported") && strings.Contains(string(rec), name+": 2 tables"), "task record: %s", head(rec, 600))
	var rows []backupRow
	_, bl := h.api.get(d.base(id) + "/backups")
	_ = json.Unmarshal(bl, &rows)
	bk, found := newest(rows, func(backupRow) bool { return true })
	h.check(t, "#81: the backup passed its restore test (verify_status)", bk.VerifyStatus, "passed")
	h.check(t, "#81: no scratch database is left on the PostgreSQL server", pg.admin("postgres", "SELECT count(*) FROM pg_database WHERE datname LIKE 'cgv\\_%'"), "0")
	h.check(t, "#81: nor a scratch role", pg.admin("postgres", "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'cgv\\_%' OR rolname LIKE 'cgq\\_%' OR rolname LIKE 'cgo\\_cgv\\_%'"), "0")
	if found {
		pg.as(rw, rwPW, name, "UPDATE t SET id = 99; DROP TABLE later")
		_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", bk.ID), fmt.Sprintf(`{"confirm":%q,"mode":"database"}`, h.api.site(id).Domain))
		h.check(t, "#81: the database restore succeeds", h.api.waitTask(taskID(b), 6*time.Minute), "succeeded")
		h.check(t, "#81: the restore put the rows back on the PostgreSQL server", pg.admin(name, "SELECT id FROM t"), "7")
		h.check(t, "#81: and the table made later", pg.admin(name, "SELECT v FROM later"), "x")
		h.check(t, "#81: the tables are the owner role's again", pg.admin(name, "SELECT string_agg(DISTINCT tableowner, ',') FROM pg_tables WHERE schemaname='public'"), "cgo_"+name)
		h.check(t, "#81: the account still works (its grant survived)", strings.TrimSpace(pg.as(rw, rwPW, name, "SELECT id FROM t").Out), "7")
		h.check(t, "#81: so does the read account, on what the restore made", strings.TrimSpace(pg.as(ro, roPW, name, "SELECT id FROM t").Out), "7")
		h.check(t, "#81: no scratch database or role is left after the restore", pg.admin("postgres", "SELECT count(*) FROM pg_database WHERE datname LIKE 'cgv\\_%'")+
			"/"+pg.admin("postgres", "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'cgq\\_%' OR rolname LIKE 'cgo\\_cgv\\_%'"), "0/0")
	}

	// A server the provider backs up has its databases left out.
	c, b = h.api.put(fmt.Sprintf("/api/db-servers/%d", srv.ID), map[string]any{"backup": "provider"})
	h.assert(t, "#81: the backup mode is changed to the provider's", c == 200 && strings.Contains(string(b), `"provider"`), "= %d %s", c, head(b, 300))
	_, b = h.api.post(d.base(id)+"/backups", `{}`)
	task = taskID(b)
	h.check(t, "#81: a backup of the site succeeds", h.api.waitTask(task, 6*time.Minute), "succeeded")
	_, rec = h.api.get(fmt.Sprintf("/api/tasks/%d", task))
	h.assert(t, "#81: and leaves the PostgreSQL databases out (no database in its restore test)", !strings.Contains(string(rec), name+":") && !strings.Contains(string(rec), other+":"), "task record: %s", head(rec, 600))

	// Dropping.
	c, _ = h.api.del(fmt.Sprintf("/api/db-servers/%d", srv.ID))
	h.check(t, "#81: a server with a database on it cannot be removed", fmt.Sprint(c), "409")
	for _, u := range []string{rw, ro} {
		c, _ = h.api.del(d.base(id) + "/database-users/" + u + "?confirm=" + u)
		h.check(t, "#81: the account "+u+" is deleted", fmt.Sprint(c), "200")
		h.check(t, "#81: and is gone from the PostgreSQL server", pg.admin("postgres", "SELECT count(*) FROM pg_roles WHERE rolname='"+u+"'"), "0")
	}
	for _, n := range []string{name, other} {
		c, b = h.api.del(d.base(id) + "/databases/" + n + "?confirm=" + n)
		h.check(t, "#81: dropping "+n+" is accepted", fmt.Sprint(c), "202")
		h.check(t, "#81: its task succeeds", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
		h.check(t, "#81: it is gone from the PostgreSQL server, with its owner role",
			pg.admin("postgres", "SELECT count(*) FROM pg_database WHERE datname='"+n+"'")+"/"+pg.admin("postgres", "SELECT count(*) FROM pg_roles WHERE rolname='cgo_"+n+"'"), "0/0")
	}
	c, _ = h.api.del(fmt.Sprintf("/api/db-servers/%d", srv.ID))
	h.check(t, "#81: and then the server can be removed", fmt.Sprint(c), "200")
}
