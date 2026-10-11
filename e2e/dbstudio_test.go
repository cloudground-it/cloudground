//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// dbStudio: F4b, Quarry on the WooCommerce site's database, opened from
// the panel (F9b: Quarry is a service of its own, entered through a
// handoff). Who may write, what a scoped account can reach, the audit of
// every write, and a 1 GB export and import that the agent streams without
// its memory growing.
func (h *harness) dbStudio(t *testing.T, wid int64) {
	woo := h.dom("shop")
	wroot := "/srv/sites/" + woo
	cfg, _ := os.ReadFile(wroot + "/shared/wp-config.php")
	wdbn := WPConfigDBName(string(cfg))
	base := "/api/quarry/db/" + wdbn
	mysql := func(q string) string { return h.run("mariadb", "-N", "-e", q).Trim() }

	// Fixtures: a table of the site's own to edit, another tenant's
	// database to fail to reach, a read-only account.
	mysql("CREATE TABLE IF NOT EXISTS `" + wdbn + "`.cg_e2e_studio (id INT PRIMARY KEY, v TEXT)")
	mysql("CREATE DATABASE IF NOT EXISTS cg_e2e_tenant")
	mysql("CREATE TABLE IF NOT EXISTS cg_e2e_tenant.secret (v TEXT)")
	mysql("INSERT INTO cg_e2e_tenant.secret VALUES ('tenant-secret-e2e')")
	pid := os.Getpid()
	roe, rop := fmt.Sprintf("e2e-ro-%d@example.com", pid), fmt.Sprintf("e2e-readonly-pw-%d", pid)
	_, ub := h.api.post("/api/users", fmt.Sprintf(`{"email":%q,"password":%q,"role":"readonly"}`, roe, rop))
	roid := jsonField(ub, "id")
	cleanup := h.undo("drop the Quarry fixtures", func() {
		mysql("DROP TABLE IF EXISTS `" + wdbn + "`.cg_e2e_studio")
		mysql("DROP TABLE IF EXISTS `" + wdbn + "`.cg_e2e_big")
		mysql("DROP DATABASE IF EXISTS cg_e2e_tenant")
		for _, g := range []string{wroot + "/shared/exports/*cg_e2e*", wroot + "/shared/exports/" + wdbn + "-*.sql", wroot + "/shared/imports/cg-e2e-*"} {
			files, _ := filepath.Glob(g)
			for _, f := range files {
				_ = os.Remove(f)
			}
		}
		h.api.relogin()
		if roid != "" {
			h.api.del("/api/users/" + roid)
		} else {
			h.deleteUserByEmail(roe)
		}
	})
	defer cleanup()
	ro := newAPIClient(h, roe, rop)
	h.assert(t, "F4b control: the read-only account signs in", ro.login(), "the read-only account could not sign in")
	h.assert(t, "F4b control: Quarry opens from the panel", h.api.openStudio(wid, wdbn) && ro.openStudio(wid, wdbn),
		"a handoff from the panel did not open Quarry")

	// A read-only account sees the tables and their structure...
	c, b := ro.get(base + "/tables")
	h.assert(t, "F4b: a read-only account lists the tables", c == 200 && contains(b, "cg_e2e_studio") && contains(b, "_options"),
		"tables answered %d: %.300s", c, b)
	c, b = ro.get(base + "/tables/cg_e2e_studio")
	h.assert(t, "SR-T4: a read-only account reads a table's structure", c == 200 && contains(b, `"id"`), "structure answered %d: %.300s", c, b)
	// ...but not the rows nor SQL (SR-T4): the database holds what the file
	// manager hides from it (wp-config.php's keys, the users' hashes).
	c, b = ro.post(base+"/tables/"+urlPathEscape(h.wooUsersTable(wdbn))+"/rows", `{"page":0}`)
	h.assert(t, "SR-T4: a read-only account cannot read rows", c == 403 && !contains(b, "user_pass"), "rows answered %d: %.300s", c, b)
	c, b = ro.post(base+"/query", `{"sql":"SELECT user_login, user_pass FROM `+h.wooUsersTable(wdbn)+`"}`)
	h.assert(t, "SR-T4: a read-only account cannot run SQL", c == 403 && !contains(b, `"rows"`), "the query answered %d: %.300s", c, b)
	c, b = h.api.post(base+"/tables/cg_e2e_studio/rows", `{"page":0}`)
	h.assert(t, "SR-T4 control: an administrator reads rows", c == 200 && contains(b, `"primary_key":["id"]`), "rows answered %d: %.300s", c, b)
	// ...and cannot write, neither through the panel nor in SQL.
	h.check(t, "F4b: a read-only account cannot edit rows", code(ro.post(base+"/tables/cg_e2e_studio/edit",
		`{"changes":[{"op":"insert","values":{"id":"66","v":"ro"}}]}`)), "403")
	h.check(t, "F4b: a read-only account cannot alter a table", code(ro.post(base+"/tables/cg_e2e_studio/alter",
		`{"kind":"add_column","column":{"name":"ro","type":"INT","nullable":true}}`)), "403")
	h.check(t, "F4b: a read-only account cannot allow writes in SQL", code(ro.post(base+"/query",
		`{"sql":"INSERT INTO cg_e2e_studio VALUES (67, 'ro')","allow_writes":true}`)), "403")
	h.check(t, "F4b: a read-only account's INSERT is refused", code(ro.post(base+"/query", `{"sql":"INSERT INTO cg_e2e_studio VALUES (68, 'ro')"}`)), "403")
	h.check(t, "F4b: nothing a read-only account sent was written",
		mysql("SELECT COUNT(*) FROM `"+wdbn+"`.cg_e2e_studio"), "0")

	// Nothing outside the site's database, in any form.
	for _, q := range []string{"SELECT COUNT(*) FROM mysql.user", "SELECT v FROM cg_e2e_tenant.secret"} {
		_, b = h.api.post(base+"/query", map[string]any{"sql": q, "allow_writes": true})
		h.assert(t, "F4b: the SQL editor cannot read "+strings.Fields(q)[3], !contains(b, `"rows":[[`) && !contains(b, "tenant-secret"),
			"%s answered %.300s", q, b)
	}
	for _, tbl := range []string{"cg_e2e_tenant`.`secret", "mysql`.`user"} {
		c, _ := h.api.post(base+"/tables/"+urlPathEscape(tbl)+"/rows", `{}`)
		h.check(t, "F4b: Quarry cannot browse "+tbl, strconv.Itoa(c), "404")
	}
	c, b = h.api.post(base+"/tables/cg_e2e_studio/rows", `{"sort":[{"column":"id`+"`"+`; DROP TABLE cg_e2e_studio; -- "}]}`)
	h.assert(t, "F4b: an injected sort column is refused and the table stays", c == 404 && mysql("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='"+wdbn+"' AND TABLE_NAME='cg_e2e_studio'") == "1",
		"answered %d: %.200s", c, b)
	// A query longer than the 30 s ReadTimeout the panel's server had (#43):
	// past it the request's context was cancelled and the query failed.
	start := time.Now()
	c, b = h.api.post(base+"/query", map[string]any{"sql": "SELECT SLEEP(45) AS slept"})
	took := time.Since(start)
	h.assert(t, "#43: a Quarry query that sleeps 45 s completes", c == 200 && contains(b, `"rows"`) && took >= 44*time.Second,
		"answered %d after %s: %.200s", c, took, b)
	// The client commands of the mariadb CLI do not exist here.
	for _, q := range []string{"system id", `\! id`} {
		_, b = h.api.post(base+"/query", map[string]any{"sql": q, "allow_writes": true})
		h.assert(t, "F4b: the SQL editor refuses "+strings.Fields(q)[0], !contains(b, "uid="), "%q ran: %.200s", q, b)
	}

	// Writes: the preview is what runs, and the audit has each one with its
	// table and key, never the row's contents.
	const secretValue = "e2e-row-content-never-audited"
	edit := map[string]any{"changes": []map[string]any{{"op": "insert", "values": map[string]any{"id": "1", "v": secretValue}}}, "dry_run": true}
	_, dry := h.api.post(base+"/tables/cg_e2e_studio/edit", edit)
	h.check(t, "F4b: a dry run writes nothing", mysql("SELECT COUNT(*) FROM `"+wdbn+"`.cg_e2e_studio"), "0")
	edit["dry_run"] = false
	_, run := h.api.post(base+"/tables/cg_e2e_studio/edit", edit)
	var dr, rr struct {
		SQL      []string `json:"sql"`
		Executed bool     `json:"executed"`
	}
	json.Unmarshal(dry, &dr)
	json.Unmarshal(run, &rr)
	h.assert(t, "F4b: the preview is the SQL that ran", len(dr.SQL) > 0 && rr.Executed && strings.Join(dr.SQL, ";") == strings.Join(rr.SQL, ";"),
		"preview %q, run %.300s", dr.SQL, run)
	h.check(t, "F4b: the insert landed", mysql("SELECT v FROM `"+wdbn+"`.cg_e2e_studio WHERE id = 1"), secretValue)
	h.api.post(base+"/tables/cg_e2e_studio/edit", `{"changes":[{"op":"update","key":{"id":"1"},"values":{"v":"changed"}}]}`)
	h.check(t, "F4b: the update landed", mysql("SELECT v FROM `"+wdbn+"`.cg_e2e_studio WHERE id = 1"), "changed")
	c, b = h.api.post(base+"/tables/cg_e2e_studio/edit", `{"changes":[{"op":"update","key":{"id":"1"},"values":{"v":"must roll back"}},{"op":"insert","values":{"id":"1","v":"dup"}}]}`)
	h.assert(t, "F4b: a failing batch is refused and rolled back", c == 400 && mysql("SELECT v FROM `"+wdbn+"`.cg_e2e_studio WHERE id = 1") == "changed",
		"answered %d: %.200s", c, b)
	h.api.post(base+"/tables/cg_e2e_studio/edit", `{"changes":[{"op":"delete","key":{"id":"1"}}]}`)
	h.check(t, "F4b: the delete landed", mysql("SELECT COUNT(*) FROM `"+wdbn+"`.cg_e2e_studio"), "0")
	c, b = h.api.post(base+"/tables/cg_e2e_studio/alter", `{"kind":"add_index","index":{"name":"cg_e2e_v","columns":[{"name":"v","length":10}]}}`)
	h.assert(t, "F4b: a guided ALTER runs", c == 200 && contains(b, `"executed":true`), "answered %d: %.200s", c, b)
	_, audit := h.api.get("/api/audit")
	var events []struct{ Action, Subject, Detail string }
	json.Unmarshal(audit, &events)
	seen := map[string]bool{}
	for _, e := range events {
		if e.Subject == woo && strings.Contains(e.Detail, " table=cg_e2e_studio") {
			seen[e.Action+" "+e.Detail] = true
		}
	}
	for _, want := range []string{"quarry.insert table=cg_e2e_studio", "quarry.update table=cg_e2e_studio key=id=1",
		"quarry.delete table=cg_e2e_studio key=id=1", "quarry.alter table=cg_e2e_studio add_index=cg_e2e_v"} {
		action, rest, _ := strings.Cut(want, " ")
		want = action + " db=" + wdbn + " " + rest
		h.assert(t, "F4b: the audit records "+action, seen[want], "no %q in %v", want, seen)
	}
	h.assert(t, "F4b: the audit never holds row contents", !bytes.Contains(audit, []byte(secretValue)), "a row's value is in the audit log")

	h.dbStudioBigTransfer(t, wid, wroot, wdbn, base, mysql)
}

// dbStudioBigTransfer: a 1 GB table exported, downloaded, uploaded and
// imported back through Quarry while the agent's resident memory is
// sampled from /proc. The agent streams each leg (dump to file, file to
// client, chunks to the API), so its RSS must not grow with the data.
func (h *harness) dbStudioBigTransfer(t *testing.T, wid int64, wroot, wdbn, base string, mysql func(string) string) {
	mb := 1024
	if v, err := strconv.Atoi(os.Getenv("CG_E2E_DBSTUDIO_MB")); err == nil && v > 0 {
		mb = v
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(wroot, &fs) != nil || int64(fs.Bavail)*int64(fs.Bsize) < int64(mb)*5<<20 {
		h.fail(t, "F4b control: room for the large transfer", "less than %d MB free under %s", mb*5, wroot)
		return
	}
	mysql("DROP TABLE IF EXISTS `" + wdbn + "`.cg_e2e_big")
	mysql("CREATE TABLE `" + wdbn + "`.cg_e2e_big (id INT PRIMARY KEY, v LONGTEXT)")
	// 1 MiB rows; MariaDB's sequence engine makes the ids.
	for done := 0; done < mb; done += 64 {
		mysql(fmt.Sprintf("INSERT INTO `%s`.cg_e2e_big SELECT seq, REPEAT(MD5(seq), 32768) FROM `%s`.seq_%d_to_%d", wdbn, wdbn, done+1, min(done+64, mb)))
	}
	sum := func() string {
		return mysql("SELECT CONCAT(COUNT(*), ':', SUM(LENGTH(v)), ':', SUM(CRC32(v))) FROM `" + wdbn + "`.cg_e2e_big")
	}
	before := sum()
	h.assert(t, "F4b control: the large table is there", strings.HasPrefix(before, strconv.Itoa(mb)+":"), "table %s", before)

	agentPID := h.run("systemctl", "show", "-p", "MainPID", "--value", "cloudground-agent").Trim()
	rss := newRSSWatch(agentPID)
	defer rss.stop()

	// Export.
	_, b := h.api.post(base+"/export", `{"format":"sql","tables":["cg_e2e_big"]}`)
	h.check(t, "F4b: the large export succeeds", h.api.waitTask(taskID(b), 30*time.Minute), "succeeded")
	_, b = h.api.get("/api/quarry/files")
	var files struct {
		Exports []struct {
			Name string
			Size int64
		}
	}
	json.Unmarshal(b, &files)
	name := ""
	for _, f := range files.Exports {
		if f.Size > int64(mb)<<20 {
			name = f.Name
			break
		}
	}
	h.assert(t, "F4b: the export is listed with its size", name != "", "exports: %.300s", b)
	if name == "" {
		return
	}
	path := wroot + "/shared/exports/" + name
	disk, _ := fileSHA(path)

	// Download through the API, streamed.
	got, n := h.api.stream("/api/quarry/exports/" + name)
	h.assert(t, "F4b: the download is the export, byte for byte", got == disk && n > int64(mb)<<20, "downloaded %d bytes, sha %s vs %s", n, got, disk)

	// Upload it back in 8 MiB chunks, then import it over the table.
	mysql("DROP TABLE `" + wdbn + "`.cg_e2e_big")
	up := "cg-e2e-big.sql"
	ok := h.api.upload("/api/quarry", up, path)
	h.assert(t, "F4b: the large upload is stored", ok, "the chunked upload failed")
	updisk, _ := fileSHA(wroot + "/shared/imports/" + up)
	h.check(t, "F4b: the upload is the file, byte for byte", updisk, disk)
	_, b = h.api.post(base+"/import", map[string]string{"name": up, "format": "sql", "confirm": wdbn})
	h.check(t, "F4b: the large import succeeds", h.api.waitTask(taskID(b), 60*time.Minute), "succeeded")
	h.check(t, "F4b: the imported table is the exported one", sum(), before)

	peak, baseRSS := rss.stop()
	t.Logf("agent RSS: %d MiB before, %d MiB peak, over %d MiB moved four times", baseRSS>>20, peak>>20, mb)
	// The bound is far below what one leg moves: a buffered copy of the
	// data anywhere in the agent would pass it at once.
	h.assert(t, "F4b: the agent's memory does not grow with a 1 GB export and import", peak-baseRSS < 192<<20 && baseRSS > 0,
		"agent RSS %d MiB -> peak %d MiB", baseRSS>>20, peak>>20)
}

// rssWatch samples a process's VmRSS every 100 ms.
type rssWatch struct {
	pid        string
	mu         sync.Mutex
	base, peak int64
	done       chan struct{}
	once       sync.Once
}

func readRSS(pid string) int64 {
	b, err := os.ReadFile("/proc/" + pid + "/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func newRSSWatch(pid string) *rssWatch {
	w := &rssWatch{pid: pid, done: make(chan struct{}), base: readRSS(pid)}
	w.peak = w.base
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-tick.C:
				if r := readRSS(w.pid); r > 0 {
					w.mu.Lock()
					w.peak = max(w.peak, r)
					w.mu.Unlock()
				}
			}
		}
	}()
	return w
}

func (w *rssWatch) stop() (peak, base int64) {
	w.once.Do(func() { close(w.done) })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak, w.base
}

func fileSHA(path string) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	hs := sha256.New()
	n, _ := io.Copy(hs, f)
	return hex.EncodeToString(hs.Sum(nil)), n
}

// openStudio opens Quarry from the client's panel session (F9b): a
// handoff for the site and database, exchanged by the same client, whose
// jar then holds both cookies.
func (a *apiClient) openStudio(site int64, database string) bool {
	_, b := a.post(fmt.Sprintf("/api/sites/%d/quarry/handoff", site), map[string]string{"database": database})
	tok := jsonField(b, "token")
	if tok == "" {
		return false
	}
	c, _ := a.post("/api/quarry/handoff", map[string]string{"token": tok})
	return c == 200
}

// stream GETs path and hashes the body as it arrives.
func (a *apiClient) stream(path string) (string, int64) {
	a.h.step("GET " + path + " (streamed)")
	resp, err := a.hc.Get(a.h.Panel + path)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "status " + resp.Status, 0
	}
	hs := sha256.New()
	n, _ := io.Copy(hs, resp.Body)
	return hex.EncodeToString(hs.Sum(nil)), n
}

// upload sends the file at path to Quarry's chunked upload, as the
// browser does: 8 MiB per request.
func (a *apiClient) upload(base, name, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8<<20)
	var off int64
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 || off == 0 {
			req, err := http.NewRequest("POST", a.h.Panel+fmt.Sprintf("%s/import/upload?name=%s&offset=%d", base, name, off), bytes.NewReader(buf[:n]))
			if err != nil {
				return false
			}
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := a.hc.Do(req)
			if err != nil {
				return false
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				a.h.root.Logf("upload chunk at %d: %s", off, resp.Status)
				return false
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return true
		}
		if rerr != nil {
			return false
		}
	}
}

// urlPathEscape escapes a path segment (a table name with backticks).
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// wooUsersTable is the WordPress users table of database (its prefix is the
// site's), as root reads it.
func (h *harness) wooUsersTable(database string) string {
	name := h.run("mariadb", "-N", "-e", "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = '"+database+"' AND TABLE_NAME LIKE '%\\_users' ORDER BY LENGTH(TABLE_NAME) LIMIT 1").Trim()
	if name == "" {
		return "wp_users"
	}
	return name
}
