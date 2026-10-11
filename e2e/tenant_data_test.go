//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tenantData: the final security review's findings on tenant data (SR-T…),
// on the WooCommerce site's database. Each check would have failed on the
// build before its fix.
func (h *harness) tenantData(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	cfg, _ := os.ReadFile(w.root + "/shared/wp-config.php")
	td := &tenantDB{h: h, w: w, db: WPConfigDBName(string(cfg))}
	td.base = "/api/quarry/db/" + td.db
	if td.db == "" {
		h.fail(t, "SR-T control: the site's database name is read", "no DB_NAME in %s/shared/wp-config.php", w.root)
		return
	}
	td.localInfile(t)
	td.linkProtection(t)
	td.uploadsPHP(t)
	td.reservedNames(t)
	td.statementLimit(t)
	td.foreignSnapshot(t)
	if !td.siteAccount(t) {
		return
	}
	td.grantPattern(t)
	td.dataDirectory(t)
	td.definers(t)
}

type tenantDB struct {
	h    *harness
	w    *wooFx
	db   string // the site's database (and account)
	base string // its Quarry routes (F9b: Quarry's own, opened from the panel)
	cnf  string // a client defaults file with the site account's password
}

// siteAccount writes a client defaults file for the site's own account,
// with the password from wp-config.php.
func (td *tenantDB) siteAccount(t *testing.T) bool {
	pw := strings.TrimSpace(td.h.wp(td.w.user, td.w.path, "config", "get", "DB_PASSWORD").Out)
	if len(pw) < 16 {
		td.h.fail(t, "SR-T control: the site account's password is read", "DB_PASSWORD from wp-config.php: %q", pw)
		return false
	}
	td.cnf = filepath.Join(t.TempDir(), "site.cnf")
	if err := os.WriteFile(td.cnf, []byte("[client]\nuser="+td.db+"\npassword="+pw+"\n"), 0o600); err != nil {
		td.h.fail(t, "SR-T control: the site account's password is read", "%v", err)
		return false
	}
	return true
}

// asSite runs one statement as the site's own account, in its database.
func (td *tenantDB) asSite(q string) result {
	return td.h.run("mariadb", "--defaults-extra-file="+td.cnf, "-N", "-e", q, td.db)
}

// root runs one statement as MariaDB's root.
func (td *tenantDB) root(q string) string { return td.h.run("mariadb", "-N", "-e", q).Trim() }

// importSQL uploads sql to Quarry as name and imports it: the task's
// status and its record.
func (td *tenantDB) importSQL(t *testing.T, name, sql string) (string, string) {
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		return "upload failed", err.Error()
	}
	defer td.h.remove(td.w.root + "/shared/imports/" + name)
	if !td.h.api.openStudio(td.w.id, td.db) || !td.h.api.upload("/api/quarry", name, path) {
		return "upload failed", ""
	}
	_, b := td.h.api.post(td.base+"/import", map[string]string{"name": name, "format": "sql", "confirm": td.db})
	id := taskID(b)
	st := td.h.api.waitTask(id, 5*time.Minute)
	_, rec := td.h.api.get(fmt.Sprintf("/api/tasks/%d", id))
	return st, string(rec)
}

// SR-T1: a tenant's dump cannot read a file of the server through LOAD DATA
// LOCAL INFILE (the import's client runs as root).
func (td *tenantDB) localInfile(t *testing.T) {
	h := td.h
	h.check(t, "SR-T1: the server's local_infile is off", td.root("SELECT @@GLOBAL.local_infile"), "0")
	host, _ := os.ReadFile("/etc/hostname")
	secret := strings.TrimSpace(string(host))
	defer td.root("DROP TABLE IF EXISTS `" + td.db + "`.cg_e2e_loot")
	st, rec := td.importSQL(t, "cg-e2e-loot.sql",
		"CREATE TABLE cg_e2e_loot (v TEXT);\nLOAD DATA LOCAL INFILE '/etc/hostname' INTO TABLE cg_e2e_loot;\n")
	got := ""
	if td.root("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='"+td.db+"' AND TABLE_NAME='cg_e2e_loot'") == "1" {
		got = td.root("SELECT COALESCE(GROUP_CONCAT(v), '') FROM `" + td.db + "`.cg_e2e_loot")
	}
	h.assert(t, "SR-T1: an import's LOAD DATA LOCAL INFILE reads no file of the server",
		secret != "" && !strings.Contains(got, secret), "the table holds %q (the file holds %q)", got, secret)
	h.check(t, "SR-T1: the import with LOAD DATA LOCAL fails", st, "failed")
	h.assert(t, "SR-T1: the task says the file has LOAD DATA statements", strings.Contains(rec, "LOAD DATA"),
		"the task record does not mention it: %.400s", rec)
}

// SR-T2: the site's grant is on its own database only: `_` in site_N is a
// LIKE wildcard in a GRANT, and the account reached (and could create) any
// database of that shape.
func (td *tenantDB) grantPattern(t *testing.T) {
	h := td.h
	n := strings.TrimPrefix(td.db, "site_")
	other, made := "siteZ"+n, "site9"+n
	defer td.root("DROP DATABASE IF EXISTS `" + other + "`")
	defer td.root("DROP DATABASE IF EXISTS `" + made + "`")
	td.root("CREATE DATABASE IF NOT EXISTS `" + other + "`")
	td.root("CREATE TABLE IF NOT EXISTS `" + other + "`.secret (v TEXT)")
	td.root("INSERT INTO `" + other + "`.secret VALUES ('cg-e2e-other-tenant')")
	h.check(t, "SR-T2 control: the site's account reads its own database", td.asSite("SELECT COUNT(*) > 0 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()").Trim(), "1")
	r := td.asSite("SELECT v FROM `" + other + "`.secret")
	h.assert(t, "SR-T2: the site's account cannot read "+other, !r.OK() && !strings.Contains(r.Out, "cg-e2e-other-tenant"), "%s", r)
	r = td.asSite("CREATE DATABASE `" + made + "`")
	h.assert(t, "SR-T2: the site's account cannot create "+made, !r.OK() && td.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '"+made+"'") == "0", "%s", r)
	// The backslash shown as / (the client escapes it in its output).
	h.check(t, "SR-T2: the site's account is granted its database by its escaped name only",
		td.root("SELECT GROUP_CONCAT(REPLACE(Db, '\\\\', '/')) FROM mysql.db WHERE User = '"+td.db+"' AND Host = 'localhost'"), "site/_"+n)
}

// SR-T3: DATA DIRECTORY needs no FILE privilege: with symbolic links on,
// the site's account put a table's files in any directory the server could
// write. The directory is outside /tmp (MariaDB's unit may have its own).
func (td *tenantDB) dataDirectory(t *testing.T) {
	h := td.h
	h.check(t, "SR-T3: the server runs without symbolic links", td.root("SELECT @@have_symlink"), "DISABLED")
	dir := "/srv/cg-e2e-datadir"
	defer td.root("DROP TABLE IF EXISTS `" + td.db + "`.cg_e2e_dd")
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o777); err != nil || os.Chmod(dir, 0o777) != nil {
		h.fail(t, "SR-T3 control: a directory the server can write", "%v", err)
		return
	}
	td.asSite("CREATE TABLE cg_e2e_dd (i INT) ENGINE=InnoDB DATA DIRECTORY='" + dir + "'")
	entries, _ := os.ReadDir(dir)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	h.assert(t, "SR-T3: a table's DATA DIRECTORY puts nothing outside the database's directory", len(entries) == 0,
		"%s holds %v", dir, names)
}

// SR-R1: the agent creates and drops only databases and accounts named
// site_<id>. The probes use throwaway names, not mysql or root: on the old
// build CreateDatabase with user root would have given root a password and
// cut the panel off its own server.
func (td *tenantDB) reservedNames(t *testing.T) {
	h := td.h
	const keep, made = "cg_e2e_reserved_keep", "cg_e2e_reserved_made"
	defer td.root("DROP DATABASE IF EXISTS " + keep + "; DROP DATABASE IF EXISTS " + made +
		"; DROP USER IF EXISTS '" + keep + "'@'localhost'; DROP USER IF EXISTS '" + made + "'@'localhost'")
	td.root("CREATE DATABASE IF NOT EXISTS " + keep + "; CREATE USER IF NOT EXISTS '" + keep + "'@'localhost'")
	rpc, done, err := newAgentRPC(h)
	if err != nil {
		h.fail(t, "SR-R1 control: the agent's socket", "%v", err)
		return
	}
	defer done()
	// Fase 9b: names are chosen now; whose a name is, the agent's record
	// says. A database the server has that is no site's is never dropped
	// or taken over for a site, and a reserved name is never made.
	_, site := h.api.get(fmt.Sprintf("/api/sites/%d", td.w.id))
	var siteRec any
	_ = json.Unmarshal(site, &siteRec)
	status, body := rpc.call("DropDatabase", map[string]any{"site": siteRec, "name": keep}, 60)
	h.assert(t, "SR-R1: the agent refuses to drop a database that is not the site's",
		td.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '"+keep+"'") == "1" &&
			td.root("SELECT COUNT(*) FROM mysql.user WHERE User = '"+keep+"'") == "1",
		"DropDatabase %s answered %s %.200s", keep, status, body)
	h.assert(t, "SR-R1: the refusal says it is not the site's", strings.Contains(body, "is not a database of"), "answered %s %.200s", status, body)
	status, body = rpc.call("CreateDatabase", map[string]any{"site": siteRec, "name": keep}, 60)
	h.assert(t, "SR-R1: the agent refuses to take over a database no site has", strings.Contains(body, "already exists"),
		"CreateDatabase %s answered %s %.200s", keep, status, body)
	for _, name := range []string{"mysql", "performance_schema", made + "-x"} {
		status, body = rpc.call("CreateDatabase", map[string]any{"site": siteRec, "name": name}, 60)
		h.assert(t, "SR-R1: the agent refuses to create the reserved or malformed database "+name,
			td.root("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '"+name+"'") == map[bool]string{true: "1", false: "0"}[name != made+"-x"] && status != "200",
			"CreateDatabase %s answered %s %.200s", name, status, body)
	}
	status, body = rpc.call("CreateDatabaseUser", map[string]any{"site": siteRec, "user": "root", "password": "cg-e2e-reserved-password-1"}, 60)
	h.assert(t, "SR-R1: the agent refuses to create the account root", status != "200", "CreateDatabaseUser root answered %s %.200s", status, body)
}

// SR-T7: a dump with a trigger and a view whose DEFINER is root imports
// (the old import, as a scoped account without TRIGGER or SET USER, failed
// on them, and so did every restore, staging push and rollback of such a
// dump); the objects are the site account's and run as it.
func (td *tenantDB) definers(t *testing.T) {
	h := td.h
	defer td.root("DROP VIEW IF EXISTS `" + td.db + "`.cg_e2e_v; DROP TABLE IF EXISTS `" + td.db + "`.cg_e2e_trg; DROP TABLE IF EXISTS `" + td.db + "`.cg_e2e_audit")
	dump := "CREATE TABLE cg_e2e_trg (i INT);\nCREATE TABLE cg_e2e_audit (i INT);\n" +
		"DELIMITER ;;\n" +
		"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER cg_e2e_t BEFORE INSERT ON cg_e2e_trg FOR EACH ROW BEGIN SET NEW.i = NEW.i + 1; INSERT INTO cg_e2e_audit VALUES (NEW.i); END */;;\n" +
		"DELIMITER ;\n" +
		"/*!50001 CREATE ALGORITHM=UNDEFINED */\n/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n/*!50001 VIEW `cg_e2e_v` AS select `i` from `cg_e2e_trg` */;\n"
	st, rec := td.importSQL(t, "cg-e2e-definers.sql", dump)
	h.assert(t, "SR-T7: a dump with a trigger and a view of a foreign DEFINER imports", st == "succeeded", "the import %s: %.400s", st, rec)
	definers := td.root("SELECT CONCAT_WS(',', (SELECT GROUP_CONCAT(DEFINER) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = '" + td.db + "' AND TRIGGER_NAME = 'cg_e2e_t'), " +
		"(SELECT GROUP_CONCAT(DEFINER) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = '" + td.db + "' AND TABLE_NAME = 'cg_e2e_v'))")
	h.check(t, "SR-T7: the trigger and the view are the site account's, not root's", definers, td.db+"@localhost,"+td.db+"@localhost")
	r := td.asSite("INSERT INTO cg_e2e_trg VALUES (41); SELECT (SELECT GROUP_CONCAT(i) FROM cg_e2e_audit), (SELECT COUNT(*) FROM cg_e2e_v)")
	h.assert(t, "SR-T7: the imported trigger and view run as the site's account", r.OK() && strings.Fields(r.Trim())[0] == "42" && strings.Fields(r.Trim())[1] == "1", "%s", r)
	h.check(t, "SR-T7: the import leaves the site's account with its own password only",
		td.root("SELECT COALESCE(JSON_LENGTH(Priv, '$.auth_or'), 0) <= 1 FROM mysql.global_priv WHERE User = '"+td.db+"' AND Host = 'localhost'"), "1")
}

// SR-T5: an account runs at most two console or Quarry statements at
// once; the third is 429 at once, and a statement after them runs.
func (td *tenantDB) statementLimit(t *testing.T) {
	h := td.h
	h.assert(t, "SR-T5 control: Quarry opens from the panel", h.api.openStudio(td.w.id, td.db), "the handoff failed")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = h.api.post(td.base+"/query", `{"sql":"SELECT SLEEP(8)"}`)
		}(i)
	}
	time.Sleep(3 * time.Second)
	start := time.Now()
	c, b := h.api.post(fmt.Sprintf("/api/sites/%d/database/query", td.w.id), `{"sql":"SELECT 1"}`)
	h.assert(t, "SR-T5: a third statement of one account at once is refused (429)", c == 429 && time.Since(start) < 3*time.Second,
		"answered %d after %s: %.200s", c, time.Since(start), b)
	wg.Wait()
	h.assert(t, "SR-T5 control: the two statements ran", codes[0] == 200 && codes[1] == 200, "answered %v", codes)
	h.check(t, "SR-T5: a statement after them runs", code(h.api.post(td.base+"/query", `{"sql":"SELECT 1"}`)), "200")
}

// SR-T6: a restore checks the files snapshot it swaps in: one that is not
// tagged with the site (here the site's own backup, retagged as another
// site's) is refused by the restore and fails the restore test, which
// passed it before (and a files restore swapped it in). A throwaway
// repository of the section's own.
func (td *tenantDB) foreignSnapshot(t *testing.T) {
	h := td.h
	realRestic, err := exec.LookPath("restic")
	if err != nil {
		h.fail(t, "SR-T6 control: restic is on PATH", "%v", err)
		return
	}
	rpc, done, err := newAgentRPC(h)
	if err != nil {
		h.fail(t, "SR-T6 control: the agent's socket", "%v", err)
		return
	}
	defer done()
	repo := fmt.Sprintf("/var/backups/cloudground/e2e-sr-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	pw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	defer os.RemoveAll(repo)
	restic := h.resticOn(realRestic, repo, pw)
	_, siteJSON := h.api.get(fmt.Sprintf("/api/sites/%d", td.w.id))
	params := func(extra map[string]any) map[string]any {
		p := map[string]any{"site": json.RawMessage(siteJSON), "repository": repo, "password": pw}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	status, body := rpc.call("RunBackup", params(map[string]any{"kind": "full"}), 900)
	snap := jsonField([]byte(body), "snapshot_id")
	if status != "200" || snap == "" {
		h.fail(t, "SR-T6 control: a backup of the site", "RunBackup %s: %.300s", status, body)
		return
	}
	status, body = rpc.call("VerifyBackup", params(map[string]any{"snapshot_id": snap}), 900)
	h.assert(t, "SR-T6 control: the site's own backup passes its restore test", status == "200" && strings.Contains(body, `"passed":true`),
		"VerifyBackup %s: %.300s", status, body)
	r := restic("tag", "--remove", "site:"+td.w.dom, "--add", "site:cg-e2e-other.example", snap)
	ids := restic("snapshots", "--json", "--tag", "site:cg-e2e-other.example")
	foreign := ""
	if list, err := ResticSnapshotIDs(ids.Out); err == nil && len(list) == 1 {
		foreign = list[0]
	}
	if !r.OK() || foreign == "" {
		h.fail(t, "SR-T6 control: the backup retagged as another site's", "restic tag: %s; snapshots: %s", r, ids)
		return
	}
	status, body = rpc.call("VerifyBackup", params(map[string]any{"snapshot_id": foreign}), 900)
	h.assert(t, "SR-T6: another site's snapshot fails the restore test", !strings.Contains(body, `"passed":true`),
		"VerifyBackup %s: %.300s", status, body)
	status, body = rpc.call("RestoreSnapshot", params(map[string]any{"snapshot_id": foreign, "mode": "files"}), 900)
	h.assert(t, "SR-T6: a files restore of another site's snapshot is refused", status == "400" && strings.Contains(body, "not a backup of "+td.w.dom),
		"RestoreSnapshot %s: %.300s", status, body)
}

// SR-T8: the installer pins the kernel's link protections, so one tenant
// cannot lead root or another site to a file through /tmp.
func (td *tenantDB) linkProtection(t *testing.T) {
	h := td.h
	conf, _ := os.ReadFile("/etc/sysctl.d/60-cloudground.conf")
	for _, key := range []string{"fs.protected_hardlinks", "fs.protected_symlinks"} {
		h.assert(t, "SR-T8: the installer's sysctl file pins "+key, strings.Contains(string(conf), key+" = 1"),
			"%s is not in /etc/sysctl.d/60-cloudground.conf", key)
		live, _ := os.ReadFile("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
		h.check(t, "SR-T8: the running kernel has "+key+" = 1", strings.TrimSpace(string(live)), "1")
	}
}

// SR-T9: a .php in wp-content/uploads is never run (a plugin that lets a
// visitor upload one would otherwise hand them the site's PHP). Control: a
// .php elsewhere in the site still runs.
func (td *tenantDB) uploadsPHP(t *testing.T) {
	h := td.h
	uploads, err := filepath.EvalSymlinks(td.w.path + "/wp-content/uploads")
	if err != nil {
		h.fail(t, "SR-T9 control: the site's uploads directory", "%v", err)
		return
	}
	const marker = "cg-e2e-php-ran"
	php := "<?php echo '" + marker + "';"
	inUploads := uploads + "/cg-e2e-shell.php"
	outside := td.w.path + "/cg-e2e-control.php"
	defer h.remove(inUploads, outside)
	if h.writeAs(inUploads, php, td.w.user, 0o644) != nil || h.writeAs(outside, php, td.w.user, 0o644) != nil {
		h.fail(t, "SR-T9 control: PHP files written as the site", "could not write %s and %s", inUploads, outside)
		return
	}
	r := h.get(td.w.dom, "/cg-e2e-control.php")
	h.assert(t, "SR-T9 control: a .php outside the uploads runs", r.Code == 200 && strings.Contains(string(r.Body), marker), "answered %d: %.200s", r.Code, r.Body)
	for _, path := range []string{"/wp-content/uploads/cg-e2e-shell.php", "/wp-content/uploads/cg-e2e-shell.PHP"} {
		r = h.get(td.w.dom, path)
		h.assert(t, "SR-T9: "+path+" is not run", r.Code == 403 || r.Code == 404, "answered %d: %.200s", r.Code, r.Body)
		h.assert(t, "SR-T9: "+path+" leaks no output", !strings.Contains(string(r.Body), marker), "answered %d: %.200s", r.Code, r.Body)
	}
}
