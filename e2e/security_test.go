//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// security: privilege boundaries, checked as real site users. Each check
// asserts on what the product does to a request or a file, not on its
// configuration text: a setting that is present but ineffective must fail.
func (h *harness) security(t *testing.T) {
	sa, sb := h.dom("seca"), h.dom("secb")
	aid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, sa, h.PHPV))
	bid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, sb, h.PHPV))
	defer func() {
		for _, id := range []int64{aid, bid} {
			if id != 0 {
				h.api.deleteSite(id)
				h.api.teardownGone(id)
			}
		}
	}()
	if aid == 0 || bid == 0 || h.api.waitActive(aid, 120*time.Second) != "active" || h.api.waitActive(bid, 120*time.Second) != "active" {
		h.fail(t, "security fixtures did not provision", "security fixtures did not provision")
		return
	}
	au, bu := h.api.site(aid).SystemUser, h.api.site(bid).SystemUser
	ar, br := "/srv/sites/"+sa, "/srv/sites/"+sb
	pid := h.pid
	defer h.remove(br+"/current/secret.txt", ar+"/current/leak.txt", ar+"/shared/etc-link")

	// R-2.5 and R-2.2: who may talk to the agent, and what of its errors
	// reaches a browser.
	h.agentPeerCredentials(t, au)
	h.agentErrorsAreSafe(t, aid, sa)
	h.webhookRefusedOnSave(t)

	// S-C1: a deploy may not copy anything from outside the site's own tree.
	h.check(t, "deploy from outside the site is refused",
		code(h.api.post(fmt.Sprintf("/api/sites/%d/deployments", aid), `{"source_dir":"/var/lib/cloudground"}`)), "400")
	// Planted the way a site would: by the site user, in its own shared/.
	// (Not `ln` + `chown -h` as root: uutils' chown -h, Ubuntu 26.04's
	// default, follows the link and re-owned /etc.)
	h.run("runuser", "-u", au, "--", "ln", "-s", "/etc", ar+"/shared/etc-link")
	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/deployments", aid), fmt.Sprintf(`{"source_dir":%q}`, ar+"/shared/etc-link"))
	if dtask := taskID(b); dtask != 0 {
		h.check(t, "deploy through a planted symlink fails", h.api.waitTask(dtask, 60*time.Second), "failed")
	} else {
		t.Logf("deploy through a planted symlink: no task (refused up front: %s); the bash suite records no check here either", head(b, 200))
	}
	shadows, _ := filepath.Glob(ar + "/releases/*/shadow")
	h.assert(t, "no host file copied into a release", len(shadows) == 0, "a host file was copied into a release")

	// S-C8: nginx must not serve another tenant's file through a symlink.
	secret := fmt.Sprintf("b-secret-%d", pid)
	h.mustWriteAs(t, br+"/current/secret.txt", secret+"\n", bu)
	h.as(au, "ln", "-s", br+"/current/secret.txt", ar+"/current/leak.txt")
	// Positive controls: the file is servable from its own site and the link
	// exists, so a refusal below means nginx refused, not that nothing was there.
	h.assert(t, "control: the tenant's own file is served", contains(h.get(sb, "/secret.txt").Body, secret), "control: the tenant's own file is not served")
	fi, err := os.Lstat(ar + "/current/leak.txt")
	h.assert(t, "control: the cross-tenant link exists", err == nil && fi.Mode()&os.ModeSymlink != 0, "control: could not create the cross-tenant link")
	h.assert(t, "cross-tenant symlink is not served", !contains(h.get(sa, "/leak.txt").Body, secret), "nginx served another tenant's file through a symlink")
	sshdT := h.run("sshd", "-T", "-C", "user="+au+",host=localhost,addr=127.0.0.1").Out
	h.assert(t, "SFTP refuses symlink and hardlink requests",
		regexp.MustCompile(`(?im)forcecommand internal-sftp.*-P symlink,hardlink@openssh\.com`).MatchString(sshdT),
		"SFTP still allows symlink/hardlink requests")

	// The file editor opens text only; binary files are download-only.
	h.filesEditor(t, aid, ar, au)
	h.filesUploadThroughNginx(t, aid, ar)

	// S-H4: a cross-site form post (text/plain, foreign Origin) is refused
	// even with a valid session cookie.
	c, _ := h.api.do("POST", "/api/users", `{"email":"csrf@example.com","password":"csrf-password-123","role":"admin"}`,
		"Content-Type: text/plain", "Origin: https://"+sa, "Sec-Fetch-Site: same-site")
	h.assert(t, "cross-site write refused", c >= 400 && c != 404, "cross-site write accepted (%d)", c)
	if c < 400 { // never leave an admin account behind
		h.deleteUserByEmail("csrf@example.com")
	}

	// R-0.13: an address with a display name must be refused, and not exist after.
	h.check(t, "a user address with a display name is refused",
		code(h.api.post("/api/users", `{"email":"E2E Name <e2e-name@example.com>","password":"e2e-long-password-1","role":"readonly"}`)), "400")
	_, ul := h.api.get("/api/users")
	stored := contains(ul, "e2e-name@example.com")
	h.assert(t, "no account was stored for the display-name address", !stored, "a user with a display-name address was stored")
	if stored {
		h.deleteUserByEmail("e2e-name@example.com")
	}

	// R-0.14: server-wide data is not an operator's. A real operator session,
	// assigned to site A, is refused the server's capacity and logs, while its
	// own site's log stays readable (the control: the session works).
	ope, opw := fmt.Sprintf("e2e-op-%d@example.com", pid), fmt.Sprintf("e2e-operator-pw-%d", pid)
	_, ub := h.api.post("/api/users", fmt.Sprintf(`{"email":%q,"password":%q,"role":"operator","site_ids":[%d]}`, ope, opw, aid))
	opid := jsonField(ub, "id")
	delOp := h.undo("delete the operator account", func() {
		if opid != "" {
			h.api.relogin()
			h.api.del("/api/users/" + opid)
		} else {
			h.deleteUserByEmail(ope)
		}
	})
	op := newAPIClient(h, ope, opw)
	op.login()
	h.check(t, "control: the operator reads its own site's log", code(op.get(fmt.Sprintf("/api/sites/%d/logs?source=access", aid))), "200")
	h.check(t, "control: an admin reads the server status", code(h.api.get("/api/system/status")), "200")
	h.check(t, "an operator is refused the server status", code(op.get("/api/system/status")), "403")
	h.check(t, "an operator is refused the server metrics", code(op.get("/api/system/metrics")), "403")
	h.check(t, "an operator is refused a server-wide log", code(op.get("/api/logs?source=nginx-access")), "403")
	h.check(t, "an operator is refused /api/logs even for its own site", code(op.get("/api/logs?source=site:"+sa+":access&site="+sa)), "403")
	h.check(t, "an operator cannot read another site's log", code(op.get(fmt.Sprintf("/api/sites/%d/logs?source=access", bid))), "404")

	// R-4.1: the whole route table, for no session and for this operator.
	_, cb := h.api.post(fmt.Sprintf("/api/sites/%d/cron", bid), map[string]string{"schedule": "0 3 * * *", "command": "true"})
	cron := jsonField(cb, "id")
	h.assert(t, "R-4.1 control: a cron job of the other site exists", cron != "", "creating a cron job on site B failed: %s", head(cb, 200))
	if cron != "" {
		delCron := h.undo("delete the R-4.1 cron job", func() { h.api.relogin(); h.api.del("/api/cron/" + cron) })
		h.routeMatrix(t, op, aid, bid, cron)
		delCron()
	}
	delOp()

	// S-H3: a site's PHP user cannot reach the API at all.
	h.assert(t, "site user cannot open the API socket",
		!h.as(au, "curl", "-s", "-o", "/dev/null", "--max-time", "3", "--unix-socket", "/run/cloudground/api.sock", "http://localhost/healthz").OK(),
		"site user can open the API socket")
	h.assert(t, "API is not on loopback TCP",
		!h.as(au, "curl", "-sk", "-o", "/dev/null", "--max-time", "3", "https://127.0.0.1:5252/healthz").OK(),
		"API still listens on loopback TCP")

	// S-C7 / Phase 4.3: each site has its own PHP master in its own
	// namespace. Site B's PHP must not see site A's APCu entries or files.
	h.mustWriteAs(t, ar+"/current/cg-a.php", fmt.Sprintf(`<?php apcu_store('cg-e2e-a-%d', 'from-a'); echo 'stored';`, pid), au)
	h.mustWriteAs(t, br+"/current/cg-b.php",
		`<?php $k=[]; foreach (new APCUIterator('/^cg-e2e-a-/') as $i) { $k[]=$i['key']; } echo 'keys=', count($k), ' sites=', json_encode(@scandir('/srv/sites'));`, bu)
	h.get(sa, fmt.Sprintf("/cg-a.php?x=%d", pid))
	seen := string(h.get(sb, fmt.Sprintf("/cg-b.php?x=%d", pid)).Body)
	h.assert(t, "a site cannot see another site's APCu entries", strings.Contains(seen, "keys=0"), "cross-site APCu visible: %s", seen)
	switch {
	case strings.Contains(seen, sa):
		h.fail(t, "a site's PHP sees no other site's files", "a site's PHP can see another site's directory: %s", seen)
	case strings.Contains(seen, "sites="):
		h.assert(t, "a site's PHP sees no other site's files", true, "")
	default:
		h.fail(t, "a site's PHP sees no other site's files", "isolation probe failed: %s", seen)
	}
	h.remove(ar+"/current/cg-a.php", br+"/current/cg-b.php")

	// S-C5: the agent's scratch space is root's alone.
	o, m := statOwnerMode("/var/lib/cloudground-agent/work")
	h.check(t, "agent work dir is root-only", strings.TrimSpace(o+" "+m), "root 700")

	// S-H5: releases/ belongs to root, so a site user cannot plant a release.
	o, _ = statOwnerMode(ar + "/releases")
	h.check(t, "releases/ is root-owned", o, "root")
	h.assert(t, "site user cannot plant a release", !h.as(au, "ln", "-s", "/", ar+"/releases/20260101-000000").OK(), "site user planted a release")
}

// deleteUserByEmail removes an account the suite may have caused to exist.
func (h *harness) deleteUserByEmail(email string) {
	var users []struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
	}
	_, b := h.api.get("/api/users")
	if json.Unmarshal(b, &users) != nil {
		return
	}
	for _, u := range users {
		if strings.Contains(u.Email, email) {
			h.api.del(fmt.Sprintf("/api/users/%d", u.ID))
		}
	}
}

// wooSecurity: S-C2 and S-C8 on the WooCommerce site, which has a database.
func (h *harness) wooSecurity(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	wid, woo, wroot := w.id, w.dom, w.root
	query := func(body string) []byte {
		_, b := h.api.post(fmt.Sprintf("/api/sites/%d/database/query", wid), body)
		return b
	}
	h.assert(t, "SQL console has no client commands", !contains(query(`{"sql":"system id","allow_writes":true}`), "uid="), "SQL console ran a shell command")
	h.assert(t, "SQL console refuses \\!", !contains(query(`{"sql":"\\! id","allow_writes":true}`), "uid="), "SQL console ran \\! id")
	query(`{"sql":"SELECT 1 INTO OUTFILE \"/tmp/cg-outfile\"","allow_writes":false}`)
	wrote := exists("/tmp/cg-outfile")
	h.assert(t, "read-only console cannot write files", !wrote, "read-only console wrote a file")
	if wrote {
		h.remove("/tmp/cg-outfile")
	}
	h.assert(t, "console is confined to the site database",
		!contains(query(`{"sql":"SELECT COUNT(*) AS n FROM mysql.user","allow_writes":false}`), `"rows":[[`),
		"console read another database (mysql.user)")
	q := query(`{"sql":"SELECT option_value FROM wp_options WHERE option_name=\"siteurl\"","allow_writes":false}`)
	h.assert(t, "console still answers ordinary queries", contains(q, "https://"+woo), "console broke ordinary queries: %s", q)
	_, info := h.api.get(fmt.Sprintf("/api/sites/%d/database", wid))
	h.assert(t, "R-3.6: the database page lists the site's tables", contains(info, "_options"), "the database page lists no tables: %.300s", info)
	// R-3.6: a statement that outlives the console (60 s) is stopped on the
	// server. Dropping the scoped account does not end it: before, the
	// SLEEP kept running for its whole length after the console gave up.
	query(`{"sql":"SELECT SLEEP(90)","allow_writes":false}`)
	running := h.run("mariadb", "-N", "-e", `SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER LIKE 'cgq\_%'`).Trim()
	h.check(t, "R-3.6: a console statement that ran out of time is not left running on the server", running, "0")
	h.assert(t, "wp-config.php is unreadable to nginx's user", !h.as("www-data", "cat", wroot+"/shared/wp-config.php").OK(),
		"nginx's user can read wp-config.php")

	// An imported SQL file comes from the site: it must not run client
	// commands as root, nor reach another database. The marker goes where
	// the agent itself may write: its sandbox keeps it out of /root, which
	// made an earlier version of this check pass on a vulnerable build.
	const pwn = "/var/lib/cloudground-agent/work/cg-e2e-pwn"
	wsu, _ := statOwnerMode(wroot + "/shared")
	h.remove(pwn)
	cfg, _ := os.ReadFile(wroot + "/shared/wp-config.php")
	wdbn := WPConfigDBName(string(cfg))
	cleanImport := h.undo("drop the SQL import fixtures", func() {
		h.run("mariadb", "-e", "DROP TABLE IF EXISTS `"+wdbn+"`.cg_e2e_import")
		files, _ := filepath.Glob(wroot + "/shared/cg-*.sql")
		for _, f := range files {
			_ = os.Remove(f)
		}
		_ = os.Remove(pwn)
	})
	imp := func(file string) int64 {
		_, b := h.api.post(fmt.Sprintf("/api/sites/%d/database/import", wid), fmt.Sprintf(`{"path":"shared/%s","confirm":%q}`, file, woo))
		return taskID(b)
	}
	put := func(file, content string) {
		h.runIn(content, "runuser", "-u", wsu, "--", "tee", wroot+"/shared/"+file)
	}
	put("cg-evil.sql", "CREATE TABLE IF NOT EXISTS cg_e2e_import (id INT);\nsystem touch "+pwn+"\n")
	put("cg-cross.sql", "CREATE TABLE mysql.cg_e2e_pwn (id INT);\n")
	put("cg-good.sql", "CREATE TABLE IF NOT EXISTS cg_e2e_import (id INT);\nINSERT INTO cg_e2e_import VALUES (7);\n")
	if tid := imp("cg-evil.sql"); tid != 0 {
		h.api.waitTask(tid, 300*time.Second)
	}
	ran := exists(pwn)
	h.assert(t, "an imported SQL file cannot run shell commands", !ran, "an imported SQL file ran a shell command as root")
	// R-3.6: the failed import is rolled back exactly. cg-evil.sql created
	// its table before the refused command; importing the rollback point on
	// top of that, as before, left the table there.
	h.check(t, "R-3.6: a failed import leaves no table behind (exact rollback)",
		h.run("mariadb", "-N", "-e", "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='"+wdbn+"' AND TABLE_NAME='cg_e2e_import'").Trim(), "0")
	if ran {
		h.remove(pwn)
	}
	if tid := imp("cg-cross.sql"); tid != 0 {
		h.api.waitTask(tid, 300*time.Second)
	}
	crossed := strings.Contains(h.run("mariadb", "-N", "-e", "SHOW TABLES FROM mysql LIKE 'cg_e2e_pwn'").Out, "cg_e2e_pwn")
	h.assert(t, "an imported SQL file is confined to the site database", !crossed, "an imported SQL file wrote to another database")
	if crossed {
		h.run("mariadb", "-e", "DROP TABLE mysql.cg_e2e_pwn")
	}
	got := ""
	if tid := imp("cg-good.sql"); tid != 0 {
		got = h.api.waitTask(tid, 300*time.Second)
	}
	h.check(t, "an ordinary SQL file still imports", got, "succeeded")
	first := strings.SplitN(h.run("mariadb", "-N", "-e", "SELECT id FROM `"+wdbn+"`.cg_e2e_import").Out, "\n", 2)[0]
	h.assert(t, "the imported rows are in the site database", first == "7", "the ordinary import did not land")
	cleanImport()
}

// webhookRefusedOnSave (W3): the alert webhook is checked when saved with
// the send-time dialer's rule. A private or local URL used to be saved, and
// every alert was then refused at send time with only a journal line.
func (h *harness) webhookRefusedOnSave(t *testing.T) {
	_, before := h.api.get("/api/settings")
	hook := jsonField(before, "notify_webhook")
	for _, inward := range []string{"https://127.0.0.1:8443/cg-e2e", "https://10.0.0.1/cg-e2e", "https://localhost/cg-e2e"} {
		c, b := h.api.put("/api/settings", map[string]string{"notify_webhook": inward})
		if c == 200 { // the old build saved it: put back what was there
			h.api.put("/api/settings", map[string]string{"notify_webhook": hook})
		}
		h.assert(t, "W3: saving the alert webhook "+inward+" is refused with the reason",
			c == 400 && strings.Contains(string(b), "public address"), "HTTP %d: %s", c, head(b, 300))
	}
	_, after := h.api.get("/api/settings")
	h.check(t, "W3: the refused webhooks left the setting as it was", jsonField(after, "notify_webhook"), hook)
}
