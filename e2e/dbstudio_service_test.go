//go:build e2e

package e2e

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// dbStudioService: F9b stream 3, Quarry as a service of its own at
// /quarry. A database user signs in with its own password, without a
// panel account, and MariaDB decides what it sees and changes: the shop's
// read user browses and is refused its writes by the server, the shop's
// all user writes, neither reaches the feature site's database. A wrong
// password is refused and delays the next attempt without locking the
// user; a changed password ends the session; logout ends it. The panel's
// handoff opens Quarry once and expires. The session's cookie is
// __Host-, and it reaches no panel route.
func (h *harness) dbStudioService(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	f, ok := h.featureSite(t)
	if !ok {
		return
	}
	wcfg, _ := os.ReadFile(w.root + "/shared/wp-config.php")
	fcfg, _ := os.ReadFile("/srv/sites/" + f.fs + "/shared/wp-config.php")
	wdb, fdb := WPConfigDBName(string(wcfg)), WPConfigDBName(string(fcfg))
	h.assert(t, "F9b-3 control: both sites have a database", wdb != "" && fdb != "" && wdb != fdb, "databases %q and %q", wdb, fdb)
	mysql := func(q string) string { return h.run("mariadb", "-N", "-e", q).Trim() }
	tag := fmt.Sprintf("e2e%d", h.pid%100000)
	reader, writer, slow := tag+"_svc_ro", tag+"_svc_rw", tag+"_svc_slow"
	pw := map[string]string{reader: studioPassword(), writer: studioPassword(), slow: studioPassword()}
	users := fmt.Sprintf("/api/sites/%d/database-users", w.id)
	cleanup := h.undo("drop the Quarry service fixtures", func() {
		h.api.relogin()
		for _, u := range []string{reader, writer, slow} {
			h.api.del(users + "/" + u + "?confirm=" + u)
		}
		mysql("DROP TABLE IF EXISTS `" + wdb + "`.cg_e2e_svc")
		files, _ := filepath.Glob(w.root + "/shared/exports/" + wdb + "-cg_e2e_svc-*.csv")
		for _, f := range files {
			_ = os.Remove(f)
		}
	})
	defer cleanup()
	for u, level := range map[string]string{reader: "read", writer: "all", slow: "read"} {
		c, b := h.api.post(users, map[string]any{"name": u, "password": pw[u], "grants": []map[string]string{{"database": wdb, "level": level}}})
		h.assert(t, "F9b-3 control: the database user "+level+" is created", c == 201, "%s answered %d: %.300s", u, c, b)
	}
	mysql("CREATE TABLE IF NOT EXISTS `" + wdb + "`.cg_e2e_svc (id INT PRIMARY KEY, v TEXT)")
	mysql("REPLACE INTO `" + wdb + "`.cg_e2e_svc VALUES (1, 'seed-e2e-svc')")
	count := func() string { return mysql("SELECT COUNT(*) FROM `" + wdb + "`.cg_e2e_svc") }
	base := "/api/quarry/db/" + wdb

	// The read user: it browses; its writes are the server's to refuse.
	ro := newAPIClient(h, "", "")
	c, b, setCookie := studioSignIn(ro, reader, pw[reader])
	h.assert(t, "F9b-3: a site's read user signs in to Quarry without a panel account", c == 200 && contains(b, `"mode":"login"`), "sign-in answered %d: %.300s", c, b)
	h.assert(t, "F9b-3: the Quarry cookie is __Host-, Secure, HttpOnly, SameSite=Strict, Path=/",
		strings.HasPrefix(setCookie, "__Host-cg-quarry=") && strings.Contains(setCookie, "Secure") && strings.Contains(setCookie, "HttpOnly") &&
			strings.Contains(setCookie, "SameSite=Strict") && strings.Contains(setCookie, "Path=/"), "Set-Cookie: %s", setCookie)
	c, b = ro.post(base+"/tables/cg_e2e_svc/rows", `{}`)
	h.assert(t, "F9b-3: the read user browses its database", c == 200 && contains(b, "seed-e2e-svc"), "rows answered %d: %.300s", c, b)
	c, b = ro.post(base+"/tables/cg_e2e_svc/edit", `{"changes":[{"op":"insert","values":{"id":"2","v":"ro"}}]}`)
	h.assert(t, "F9b-3: MariaDB refuses the read user's edit", c != 200 && (contains(b, "1142") || contains(b, "denied")), "edit answered %d: %.300s", c, b)
	_, b = ro.post(base+"/query", `{"sql":"INSERT INTO cg_e2e_svc VALUES (3, 'ro')"}`)
	h.assert(t, "F9b-3: MariaDB refuses the read user's INSERT in the SQL editor", contains(b, "1142") || contains(b, "denied"), "query answered %.300s", b)
	h.check(t, "F9b-3: nothing the read user sent was written", count(), "1")
	c, b = ro.get(base + "/tables")
	h.assert(t, "F9b-3: the read user lists its tables", c == 200 && contains(b, "cg_e2e_svc"), "tables answered %d: %.300s", c, b)
	_, b = ro.get("/api/quarry/databases")
	h.assert(t, "F9b-3: the read user's databases are its grants'", contains(b, `"`+wdb+`"`) && !contains(b, `"`+fdb+`"`) && !contains(b, `"mysql"`),
		"databases: %.300s", b)

	// Another site's database: not with this site's user, in any form.
	c, b = ro.get("/api/quarry/db/" + fdb + "/tables")
	h.assert(t, "F9b-3: another site's database is not reachable with a site's user", c != 200 && !contains(b, "_options"), "answered %d: %.300s", c, b)
	_, b = ro.post(base+"/query", fmt.Sprintf(`{"sql":"SELECT option_value FROM %s.wp_options LIMIT 1"}`, fdb))
	h.assert(t, "F9b-3: the SQL editor cannot read another site's database", !contains(b, `"rows":[[`), "query answered %.300s", b)
	c, _ = ro.get("/api/sites")
	h.check(t, "F9b-3: a Quarry session is no panel session", fmt.Sprint(c), "401")

	// The all user writes.
	rw := newAPIClient(h, "", "")
	c, b, _ = studioSignIn(rw, writer, pw[writer])
	h.assert(t, "F9b-3 control: the all user signs in", c == 200, "sign-in answered %d: %.300s", c, b)
	c, b = rw.post(base+"/tables/cg_e2e_svc/edit", `{"changes":[{"op":"insert","values":{"id":"2","v":"rw"}}]}`)
	h.assert(t, "F9b-3: the all user's edit is written", c == 200 && contains(b, `"executed":true`), "edit answered %d: %.300s", c, b)
	h.check(t, "F9b-3: the all user's row is there", mysql("SELECT v FROM `"+wdb+"`.cg_e2e_svc WHERE id = 2"), "rw")

	// Simulate: an UPDATE and a DELETE say how many rows they would change
	// and change none; a MyISAM table, which cannot roll back, is refused.
	h.dbStudioSimulate(t, rw, base, wdb, mysql)

	// An export as the user lands among its own files, and no defaults
	// file with its password is left behind.
	_, b = rw.post(base+"/export", `{"format":"csv","tables":["cg_e2e_svc"]}`)
	h.check(t, "F9b-3: the all user's export succeeds", rw.studioTask(taskID(b), 5*time.Minute), "succeeded")
	_, b = rw.get("/api/quarry/files")
	h.assert(t, "F9b-3: its export is in its files", contains(b, "cg_e2e_svc"), "files: %.300s", b)
	_, b = ro.get("/api/quarry/files")
	h.assert(t, "F9b-3: another user's export is not in the read user's files", !contains(b, "cg_e2e_svc"), "files: %.300s", b)
	left, _ := filepath.Glob("/var/lib/cloudground-agent/work/.dbq-*.cnf")
	h.assert(t, "F9b-3: no credentials file is left in the agent's work directory", len(left) == 0, "left: %v", left)

	// A changed password ends the session.
	c, _ = h.api.post(users+"/"+writer+"/password", map[string]string{"password": studioPassword()})
	h.check(t, "F9b-3 control: the all user's password changes", fmt.Sprint(c), "200")
	c, b = rw.post(base+"/tables/cg_e2e_svc/rows", `{}`)
	h.assert(t, "F9b-3: a changed password ends the Quarry session", c == 401, "rows answered %d: %.300s", c, b)

	// Wrong passwords: refused, then delayed, never locked.
	bad := newAPIClient(h, "", "")
	slowest := time.Duration(0)
	for range 5 {
		start := time.Now()
		c, b, _ = studioSignIn(bad, slow, "not-the-password-1")
		slowest = max(slowest, time.Since(start))
		h.assert(t, "F9b-3: a wrong password is refused", c == 401 && contains(b, "invalid credentials"), "answered %d: %.300s", c, b)
	}
	c, _, _ = studioSignIn(bad, "root", "anything")
	h.check(t, "F9b-3: root cannot sign in to Quarry", fmt.Sprint(c), "401")
	start := time.Now()
	c, b, _ = studioSignIn(bad, slow, pw[slow])
	took := time.Since(start)
	h.assert(t, "F9b-3: after refusals the next attempt waits, and the right password still signs in", c == 200 && took >= time.Second,
		"answered %d after %v: %.200s", c, took, b)
	h.dbStudioSignInDuringAttack(t, slow, pw[slow])

	// The client the agent runs for a user's import reads the agent's
	// defaults file alone: a [client] user=root in the server's own option
	// files (or root's ~/.my.cnf) does not turn the read user's import into
	// root's, which unix_socket would let in.
	h.dbStudioImportIgnoresOptionFiles(t, ro, w.root, wdb, tag)

	// Logout ends the session.
	c, _ = ro.post("/api/quarry/logout", `{}`)
	h.check(t, "F9b-3 control: logout answers", fmt.Sprint(c), "200")
	c, _ = ro.get("/api/quarry/databases")
	h.check(t, "F9b-3: logout ends the Quarry session", fmt.Sprint(c), "401")

	// The handoff from the panel: once, and not after its minute.
	_, b = h.api.post(fmt.Sprintf("/api/sites/%d/quarry/handoff", w.id), map[string]string{"database": wdb})
	tok := jsonField(b, "token")
	c, _ = h.api.post("/api/quarry/handoff", map[string]string{"token": tok})
	h.check(t, "F9b-3: the panel's handoff opens Quarry", fmt.Sprint(c), "200")
	c, _ = h.api.post(base+"/tables/cg_e2e_svc/rows", `{}`)
	h.check(t, "F9b-3: the handed-off session browses the site's database", fmt.Sprint(c), "200")
	c, _ = h.api.post("/api/quarry/handoff", map[string]string{"token": tok})
	h.check(t, "F9b-3: a handoff works once", fmt.Sprint(c), "401")
	_, b = h.api.post(fmt.Sprintf("/api/sites/%d/quarry/handoff", w.id), map[string]string{"database": wdb})
	tok = jsonField(b, "token")
	h.step("wait for a Quarry handoff to expire (61 s)")
	time.Sleep(61 * time.Second)
	c, _ = h.api.post("/api/quarry/handoff", map[string]string{"token": tok})
	h.check(t, "F9b-3: a handoff expires after its minute", fmt.Sprint(c), "401")
}

// dbStudioSimulate checks the SQL editor's simulate as the all user rw:
// the counts are the server's, the table is as it was after each one, and
// a MyISAM table is refused without being touched.
func (h *harness) dbStudioSimulate(t *testing.T, rw *apiClient, base, wdb string, mysql func(string) string) {
	legacy := "`" + wdb + "`.cg_e2e_svc_myisam"
	undo := h.undo("drop the simulate fixtures", func() {
		mysql("DROP TABLE IF EXISTS " + legacy)
		mysql("DELETE FROM `" + wdb + "`.cg_e2e_svc WHERE id >= 10")
	})
	defer undo()
	mysql("INSERT INTO `" + wdb + "`.cg_e2e_svc VALUES (10, 'sim-a'), (11, 'sim-b'), (12, 'sim-c')")
	mysql("CREATE TABLE " + legacy + " (id INT PRIMARY KEY, v TEXT) ENGINE=MyISAM")
	mysql("INSERT INTO " + legacy + " VALUES (1, 'old')")
	state := func() string {
		return mysql("SELECT GROUP_CONCAT(CONCAT(id, '=', v) ORDER BY id) FROM `" + wdb + "`.cg_e2e_svc")
	}
	before := state()

	c, b := rw.post(base+"/simulate", `{"sql":"UPDATE cg_e2e_svc SET v = 'simulated' WHERE id >= 10"}`)
	h.assert(t, "F9b-3: a simulated UPDATE reports the rows it would change", c == 200 && contains(b, `"affected":3`) && contains(b, `"rolled_back":true`) && contains(b, "sim-b"),
		"simulate answered %d: %.400s", c, b)
	h.check(t, "F9b-3: a simulated UPDATE changes nothing", state(), before)
	c, b = rw.post(base+"/simulate", `{"sql":"DELETE FROM cg_e2e_svc WHERE id >= 11"}`)
	h.assert(t, "F9b-3: a simulated DELETE reports the rows it would delete", c == 200 && contains(b, `"affected":2`) && contains(b, `"rolled_back":true`),
		"simulate answered %d: %.400s", c, b)
	h.check(t, "F9b-3: a simulated DELETE deletes nothing", state(), before)
	c, b = rw.post(base+"/simulate", `{"sql":"DELETE FROM cg_e2e_svc_myisam"}`)
	h.assert(t, "F9b-3: a simulation on a MyISAM table is refused", c == 200 && contains(b, `"refused"`) && contains(b, "MyISAM"),
		"simulate answered %d: %.400s", c, b)
	h.check(t, "F9b-3: the refused simulation left the MyISAM table alone", mysql("SELECT COUNT(*) FROM "+legacy), "1")
	c, b = rw.post(base+"/simulate", `{"sql":"TRUNCATE TABLE cg_e2e_svc"}`)
	h.assert(t, "F9b-3: a simulated TRUNCATE (an implicit commit) is refused", c == 200 && contains(b, `"refused"`), "simulate answered %d: %.400s", c, b)
	h.check(t, "F9b-3: nothing a simulation was given stays", state(), before)
	h.dbStudioSimulateEscapes(t, rw, base, wdb, mysql)
}

// dbStudioSimulateEscapes tries, as a signed-in writer, the simulations
// an earlier build let change a MyISAM table for good or answered wrong: a
// table named by digits, a package function, a preview past 25 rows, and
// RETURNING.
func (h *harness) dbStudioSimulateEscapes(t *testing.T, rw *apiClient, base, wdb string, mysql func(string) string) {
	q := "`" + wdb + "`"
	undo := h.undo("drop the simulate escape fixtures", func() {
		mysql("DROP TABLE IF EXISTS " + q + ".`123`, " + q + ".cg_e2e_svc_pklog, " + q + ".cg_e2e_svc_many")
		mysql("DROP PACKAGE IF EXISTS " + q + ".cg_e2e_pk")
	})
	defer undo()
	mysql("CREATE TABLE " + q + ".`123` (id INT PRIMARY KEY, v INT) ENGINE=MyISAM")
	mysql("INSERT INTO " + q + ".`123` VALUES (1, 1)")
	mysql("CREATE TABLE " + q + ".cg_e2e_svc_pklog (id INT AUTO_INCREMENT PRIMARY KEY, n INT) ENGINE=MyISAM")
	mysql("DELIMITER //\nCREATE PACKAGE " + q + ".cg_e2e_pk FUNCTION f(x INT) RETURNS INT; END//\n" +
		"CREATE PACKAGE BODY " + q + ".cg_e2e_pk FUNCTION f(x INT) RETURNS INT BEGIN INSERT INTO " + q + ".cg_e2e_svc_pklog (n) VALUES (x); RETURN x; END; END//\nDELIMITER ;")
	mysql("CREATE TABLE " + q + ".cg_e2e_svc_many (id INT PRIMARY KEY, v INT) ENGINE=InnoDB")
	mysql("INSERT INTO " + q + ".cg_e2e_svc_many SELECT seq, seq FROM " + q + ".seq_1_to_40")

	c, b := rw.post(base+"/simulate", map[string]string{"sql": "UPDATE cg_e2e_svc_many, `123` SET cg_e2e_svc_many.v = 0, `123`.v = 9"})
	h.assert(t, "F9b-3: a simulation that writes a MyISAM table named by digits is refused", c == 200 && contains(b, `"refused"`), "simulate answered %d: %.400s", c, b)
	h.check(t, "F9b-3: the MyISAM table named by digits is unchanged", mysql("SELECT v FROM "+q+".`123`"), "1")
	c, b = rw.post(base+"/simulate", map[string]string{"sql": "UPDATE cg_e2e_svc_many SET v = cg_e2e_pk.f(v)"})
	h.assert(t, "F9b-3: a simulation that calls a package function is refused", c == 200 && contains(b, `"refused"`), "simulate answered %d: %.400s", c, b)
	h.check(t, "F9b-3: the package function wrote nothing", mysql("SELECT COUNT(*) FROM "+q+".cg_e2e_svc_pklog"), "0")
	c, b = rw.post(base+"/simulate", map[string]string{"sql": "UPDATE cg_e2e_svc_many SET v = v + 1"})
	h.assert(t, "F9b-3: a simulation whose preview passes 25 rows still answers, rolled back", c == 200 && contains(b, `"affected":40`) && contains(b, `"rolled_back":true`),
		"simulate answered %d: %.400s", c, b)
	c, b = rw.post(base+"/simulate", map[string]string{"sql": "DELETE FROM cg_e2e_svc_many WHERE id > 30 RETURNING id"})
	h.assert(t, "F9b-3: a simulated DELETE … RETURNING counts its rows", c == 200 && contains(b, `"affected":10`) && contains(b, `"rolled_back":true`),
		"simulate answered %d: %.400s", c, b)
	for _, q := range []string{"UPDATE cg_e2e_svc_many SET v = SLEEP(1)", "UPDATE cg_e2e_svc_many SET v = (SELECT v FROM `123` FOR UPDATE)"} {
		c, b = rw.post(base+"/simulate", map[string]string{"sql": q})
		h.assert(t, "F9b-3: a simulation outside the allowlists (a non-listed function, a locking read) is refused", c == 200 && contains(b, `"refused"`),
			"%s: simulate answered %d: %.400s", q, c, b)
	}
	// v is an INT: the built-ins end in a number (CONCAT(…, NOW()) itself
	// is no integer, and strict mode refuses it as the server's error).
	// CHAR_LENGTH is 20, which row 20 holds already: 39 rows matched, 38
	// changed, and the count is of the rows matched.
	c, b = rw.post(base+"/simulate", map[string]string{"sql": "UPDATE cg_e2e_svc_many SET v = CHAR_LENGTH(CONCAT(UPPER('a'), NOW())) WHERE id > (SELECT MIN(id) FROM cg_e2e_svc_many)"})
	h.assert(t, "F9b-3: a simulation with built-ins runs", c == 200 && contains(b, `"affected":39`) && contains(b, `"rolled_back":true`) && !contains(b, `"message":"ERROR`),
		"simulate answered %d: %s", c, b)
	h.check(t, "F9b-3: the simulations left the InnoDB table alone", mysql("SELECT SUM(v) FROM "+q+".cg_e2e_svc_many"), "820")
}

// studioSignIn signs a database user in to Quarry, returning the
// status, the body and the Set-Cookie header the panel sent. hdr are
// "Name: value" pairs.
func studioSignIn(a *apiClient, user, password string, hdr ...string) (int, []byte, string) {
	a.h.step("POST /api/quarry/login as " + user)
	body := fmt.Sprintf(`{"user":%q,"password":%q}`, user, password)
	req, err := http.NewRequest("POST", a.h.Panel+"/api/quarry/login", strings.NewReader(body))
	if err != nil {
		return 0, nil, ""
	}
	req.Header.Set("Content-Type", "application/json")
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, nil, ""
	}
	defer resp.Body.Close()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b, resp.Header.Get("Set-Cookie")
}

// dbStudioSignInDuringAttack (S-H3 review, SPEC §6.4 step 3): wrong
// passwords for a database user from one network, four in flight at once
// after five refusals on record, do not keep the user out: from another
// network it signs in within 8 s, never 429. The addresses are X-Real-IP
// over the API socket (accounts_test.go). The build before slept the delay
// inside the user's queue of four, so the user got 429 for the attacker's
// backlog. The attacker's network keeps its own delay afterwards.
func (h *harness) dbStudioSignInDuringAttack(t *testing.T, user, password string) {
	for i := range 5 {
		if c, b, _ := studioSignIn(newAPIClient(h, "", ""), user, "not-the-password-2", fromAddr(attackSeeder)); c != 401 {
			h.fail(t, "F9b-3 control: a wrong password is refused", "attempt %d: %d %.200s", i+1, c, b)
			return
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, _ = studioSignIn(newAPIClient(h, "", ""), user, "not-the-password-3", fromAddr(attackerAddr))
		}()
	}
	time.Sleep(500 * time.Millisecond) // the four are in flight
	start := time.Now()
	c, b, _ := studioSignIn(newAPIClient(h, "", ""), user, password, fromAddr(attackedAddr))
	took := time.Since(start)
	h.assert(t, "F9b-3: four wrong passwords in flight from one network do not keep the database user out: it signs in from another within 8 s",
		c == 200 && took < 8*time.Second, "answered %d after %v: %.200s", c, took, b)
	wg.Wait()
	h.check(t, "F9b-3 control: the attacker's four wrong passwords are refused", fmt.Sprint(codes), "[401 401 401 401]")
	studioSignIn(newAPIClient(h, "", ""), user, "not-the-password-3", fromAddr(attackerAddr))
	start = time.Now()
	c, _, _ = studioSignIn(newAPIClient(h, "", ""), user, "not-the-password-3", fromAddr(attackerAddr))
	took = time.Since(start)
	h.assert(t, "F9b-3: after the user's sign-in the attacker's network still waits its own delay (1 s from its fifth refusal)",
		c == 401 && took >= time.Second, "answered %d after %v", c, took)
}

// studioTask waits for a task a Quarry session started, through its own
// route.
func (a *apiClient) studioTask(id int64, timeout time.Duration) string {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(time.Second) {
		_, b := a.get(fmt.Sprintf("/api/quarry/tasks/%d", id))
		if st := jsonField(b, "status"); st == "succeeded" || st == "failed" {
			return st
		}
	}
	return "timeout"
}

// studioPassword is a fresh password for a database user.
func studioPassword() string {
	raw := make([]byte, 18)
	_, _ = rand.Read(raw)
	return "Svc-" + strings.NewReplacer("/", "x", "+", "y", "=", "").Replace(b64(raw))
}

// dbStudioImportIgnoresOptionFiles imports into wdb (of the site at
// siteRoot), as the read user ro signed in
// as, a .sql file that creates a table and records CURRENT_USER(), while an
// option file under /etc/mysql/conf.d names root for every client group.
// The read user may not create tables: the import must fail and leave
// nothing. A build whose client also read the server's option files signed
// in as root there (unix_socket) and made the table.
func (h *harness) dbStudioImportIgnoresOptionFiles(t *testing.T, ro *apiClient, siteRoot, wdb, tag string) {
	conf := "/etc/mysql/conf.d/zz-cg-e2e-root-client.cnf"
	name := tag + "_who.sql"
	who := "`" + wdb + "`.cg_e2e_svc_who"
	undo := h.undo("remove the root client option file and the import's leftovers", func() {
		_ = os.Remove(conf)
		_ = os.Remove(siteRoot + "/shared/imports/" + name)
		h.run("mariadb", "-N", "-e", "DROP TABLE IF EXISTS "+who)
	})
	defer undo()
	err := os.WriteFile(conf, []byte("[client]\nuser=root\n[mysql]\nuser=root\n[mariadb-client]\nuser=root\n[mysqldump]\nuser=root\n"), 0o644)
	h.assert(t, "F9b-3 control: a root client option file is in place", err == nil && strings.Contains(h.run("mariadb", "--print-defaults").Trim(), "--user=root"),
		"write %s: %v", conf, err)
	path := filepath.Join(t.TempDir(), name)
	_ = os.WriteFile(path, []byte("CREATE TABLE cg_e2e_svc_who (u VARCHAR(200));\nINSERT INTO cg_e2e_svc_who SELECT CURRENT_USER();\n"), 0o644)
	h.assert(t, "F9b-3 control: the read user uploads a .sql file", ro.upload("/api/quarry", name, path), "upload of %s refused", name)
	_, b := ro.post("/api/quarry/db/"+wdb+"/import", map[string]string{"name": name, "format": "sql", "confirm": wdb})
	status := ro.studioTask(taskID(b), 3*time.Minute)
	made := h.run("mariadb", "-N", "-e", "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = '"+wdb+"' AND TABLE_NAME = 'cg_e2e_svc_who'").Trim()
	h.assert(t, "F9b-3: a root [client] user= in the server's option files does not make a database user's import root's",
		status == "failed" && made == "0", "import %s, table made: %s (%s)", status, made, h.run("mariadb", "-N", "-e", "SELECT u FROM "+who).Trim())
}
