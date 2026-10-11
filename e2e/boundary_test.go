//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// boundary is the R-10.5 section: the root agent's boundary, as found by the
// final review. Two PHP sites of the section's own, A and B; the agent is
// called on its socket with a restic repository of the section's own, so
// the panel's backup setting is never touched.
func (h *harness) boundary(t *testing.T) {
	domA, domB := h.dom("r10a"), h.dom("r10b")
	idA := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, domA, h.PHPV))
	idB := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, domB, h.PHPV))
	defer func() {
		for _, id := range []int64{idA, idB} {
			if id != 0 {
				h.api.deleteSite(id)
				h.api.teardownGone(id)
			}
		}
	}()
	okA := idA != 0 && h.api.waitActive(idA, 4*time.Minute) == "active"
	okB := idB != 0 && h.api.waitActive(idB, 4*time.Minute) == "active"
	h.assert(t, "R-10.5 control: the boundary sites provision", okA && okB, "R-10.5: %s or %s did not become active", domA, domB)
	if !okA || !okB {
		return
	}
	rpc, closeRPC, err := newAgentRPC(h)
	if err != nil {
		h.fail(t, "R-10.5 fixture: the agent's token", "R-10.5 fixture: %v", err)
		return
	}
	defer closeRPC()
	b := &bnd{h: h, rpc: rpc, idA: idA, idB: idB, domA: domA, domB: domB,
		rootA: "/srv/sites/" + domA, rootB: "/srv/sites/" + domB,
		userA: h.api.site(idA).SystemUser, userB: h.api.site(idB).SystemUser}
	h.mustWriteAs(t, b.rootA+"/current/index.php", `<?php echo "r10-ok";`, b.userA)
	h.mustWriteAs(t, b.rootB+"/current/index.php", `<?php echo "r10-ok";`, b.userB)
	b.repo = fmt.Sprintf("/var/backups/cloudground/e2e-r10-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	b.pw = strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	defer h.undo("remove the R-10.5 repository", func() { _ = os.RemoveAll(b.repo) })()

	b.restoreKeepsRevokedKeysOut(t)
	b.fileManagerLeavesTheSkeleton(t)
	b.siteIndexTiesTheRecord(t)
	b.backupSeesOnlyTheSite(t)
	b.repositoryIsValidated(t)
	b.fpmTestIsSandboxed(t)
	b.connectorRefusesLinkedWPContent(t)
	b.shellLoginIsForced(t)
	b.runCommandIsNotExpanded(t)
	b.routingReachesOnlyItsOwn(t)
	b.stagingDBUserIsChecked(t)
	b.cronRunIsHardened(t)
	b.gateClientIsItsOwnBinary(t)
	b.phpBypassCookies(t)
	b.localSockets(t)
}

// localSockets (R-10.5/G, ops/sftp SPEC §5.8): what the box listens on
// locally is the documented set: the panel API is not on TCP (5252) but on
// its Unix socket, the connector's 9080 is loopback only, and no
// CloudGround program binds an abstract Unix socket.
func (b *bnd) localSockets(t *testing.T) {
	h := b.h
	tcp := h.run("ss", "-Hltnp").Out
	h.assert(t, "R-10.5/G: the panel API does not listen on TCP 5252", !strings.Contains(tcp, ":5252 "), "ss: %s", head([]byte(tcp), 400))
	for _, line := range strings.Split(tcp, "\n") {
		if strings.Contains(line, ":9080 ") {
			h.assert(t, "R-10.5/G: the connector's 9080 listens on loopback only", strings.Contains(line, "127.0.0.1:9080") || strings.Contains(line, "[::1]:9080"), "ss: %s", line)
		}
	}
	h.assert(t, "R-10.5/G: the panel API listens on its Unix socket", exists("/run/cloudground/api.sock"), "no /run/cloudground/api.sock")
	ux := h.run("ss", "-Hlxp").Out
	var abstract []string
	for _, line := range strings.Split(ux, "\n") {
		if strings.Contains(line, " @") && (strings.Contains(line, "panel-api") || strings.Contains(line, "panel-agent") || strings.Contains(line, "cg-site-shell")) {
			abstract = append(abstract, line)
		}
	}
	h.assert(t, "R-10.5/G: no CloudGround program binds an abstract Unix socket", len(abstract) == 0, "abstract sockets: %v", abstract)
}

// phpBypassCookies (R-10.5/E): a plain PHP site's cached page is not served
// to a visitor holding a PHP framework's session cookie (CodeIgniter's
// ci_session, CakePHP's CAKEPHP), nor one holding a cookie the site added
// to its bypass list (PATCH bypass_cookies). Before, only WordPress's
// cookies and PHPSESSID bypassed. Control: the anonymous page is a HIT.
func (b *bnd) phpBypassCookies(t *testing.T) {
	h := b.h
	b.h.cacheOf(b.domB, "/", "")
	h.check(t, "R-10.5/E control: an anonymous PHP page is cached", h.cacheOf(b.domB, "/", ""), "HIT")
	for _, cookie := range []string{"ci_session=e2e", "CAKEPHP=e2e"} {
		name, _, _ := strings.Cut(cookie, "=")
		h.check(t, "R-10.5/E: a PHP page is not served from the cache with "+name, h.cacheOf(b.domB, "/", "a=1; "+cookie), "BYPASS")
	}
	c, body := h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", b.idB), map[string]any{"bypass_cookies": []string{"r10_sid"}})
	h.check(t, "R-10.5/E: the site's own bypass cookie is accepted", fmt.Sprint(c), "202")
	if c == 202 {
		h.api.waitTask(taskID(body), 120*time.Second)
	}
	defer h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", b.idB), map[string]any{"bypass_cookies": []string{}})
	h.check(t, "R-10.5/E: a PHP page is not served from the cache with the site's own cookie", h.cacheOf(b.domB, "/", "r10_sid=1"), "BYPASS")
	h.cacheOf(b.domB, "/", "")
	h.check(t, "R-10.5/E control: the anonymous page is still cached", h.cacheOf(b.domB, "/", "r10_sidebar=1"), "HIT")
	c, _ = h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", b.idB), map[string]any{"bypass_cookies": []string{`a"b`}})
	h.check(t, "R-10.5/E: a cookie name nginx would read as syntax is refused", fmt.Sprint(c), "400")
}

// gateClientIsItsOwnBinary (R-10.5/D): the shell login's login shell is the
// small cg-site-shell binary, not a link to panel-agent (whose whole
// package initialisation ran before the client made itself non-dumpable),
// and a shell login is still served by it (control: the shell login's
// passwd entry names it).
func (b *bnd) gateClientIsItsOwnBinary(t *testing.T) {
	h := b.h
	const gate = "/usr/local/bin/cg-site-shell"
	fi, err := os.Lstat(gate)
	h.assert(t, "R-10.5/D: cg-site-shell is a binary of its own, not a link", err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0,
		"%s: %v %v", gate, fi, err)
	same := h.run("cmp", "-s", gate, "/usr/local/bin/panel-agent").OK()
	h.assert(t, "R-10.5/D: cg-site-shell is not panel-agent", !same, "%s is panel-agent's bytes", gate)
	if fi != nil && fi.Size() > 0 {
		agent, _ := os.Stat("/usr/local/bin/panel-agent")
		h.assert(t, "R-10.5/D: cg-site-shell is smaller than panel-agent", agent != nil && fi.Size() < agent.Size()/2,
			"cg-site-shell %d bytes, panel-agent %v", fi.Size(), agent)
	}
}

// cronRunIsHardened (R-10.5/C): a cron job runs with the shell's hardening
// beyond the PHP sandbox: it cannot make a setuid file (RestrictSUIDSGID)
// and has an IPC namespace of its own. Before, chmod u+s worked in a job.
func (b *bnd) cronRunIsHardened(t *testing.T) {
	h := b.h
	cmd := `f="$HOME/tmp/cg-e2e-suid"; cp /bin/true "$f"; chmod u+s "$f" 2>/dev/null; echo "mode=$(stat -c %a "$f")"; rm -f "$f"; echo "ipc=$(readlink /proc/self/ns/ipc)"`
	c, body := h.api.post(fmt.Sprintf("/api/sites/%d/cron", b.idA), map[string]string{"schedule": "0 5 * * *", "command": cmd})
	id := jsonField(body, "id")
	if id == "" {
		h.fail(t, "R-10.5/C control: a cron job is created", "create cron: %d %s", c, head(body, 200))
		return
	}
	defer h.api.del("/api/cron/" + id)
	c, body = h.api.post("/api/cron/"+id+"/run", map[string]string{})
	out := jsonField(body, "output")
	h.assert(t, "R-10.5/C control: the job ran", c == 200 && strings.Contains(out, "mode="), "run now: %d %q", c, out)
	h.assert(t, "R-10.5/C: a cron job cannot make a setuid file", strings.Contains(out, "mode=755"), "run now: %q", out)
	hostIPC := h.run("readlink", "/proc/self/ns/ipc").Trim()
	h.assert(t, "R-10.5/C: a cron job has an IPC namespace of its own", strings.Contains(out, "ipc=ipc:") && !strings.Contains(out, "ipc="+hostIPC),
		"job %q, host %s", out, hostIPC)
}

// stagingDBUserIsChecked (R-10.5, staging): a staging copy of a WordPress
// site whose database user is not a valid name (here an option) is refused before
// anything is made (it would reach wp-cli's argv). The agent is called with
// site A's record as a WordPress production and an option-like user.
func (b *bnd) stagingDBUserIsChecked(t *testing.T) {
	h := b.h
	var prod map[string]any
	_ = json.Unmarshal(b.siteJSON(b.idA), &prod)
	prod["type"] = "wordpress"
	prod["config"] = map[string]any{"database": map[string]any{"enabled": true, "name": fmt.Sprintf("site_%d", b.idA), "user": fmt.Sprintf("site_%d", b.idA)}}
	sid := int64(900000 + h.pid%1000)
	dom := "stg-r10." + b.domA
	stg := map[string]any{"id": sid, "domain": dom, "type": "wordpress", "php_version": h.PHPV, "system_user": fmt.Sprintf("cg-site-%d", sid),
		"root_path": "/srv/sites/" + dom, "staging_of": b.idA,
		"config": map[string]any{"database": map[string]any{"enabled": true, "name": fmt.Sprintf("site_%d", sid), "user": "--require=/tmp/x.php"}}}
	defer h.remove("/srv/sites/" + dom)
	status, body := b.rpc.call("CreateStaging", map[string]any{"production": prod, "staging": stg, "db_password": "r10-staging-pw-123"}, 300)
	// Fase 9b: the names are chosen now, within one shape (a lowercase
	// letter first), which the agent checks on every site record.
	h.assert(t, "R-10.5: a staging database user that is not a valid name (an option) is refused", status == "400" &&
		(strings.Contains(body, "not a valid name") || strings.Contains(body, "names of its own")),
		"CreateStaging: %s %s", status, head([]byte(body), 200))
	h.assert(t, "R-10.5: nothing of the refused staging copy was made", !exists("/srv/sites/"+dom), "/srv/sites/%s exists", dom)
}

// routingReachesOnlyItsOwn (R-10.5/A, /B): site A's routing may not proxy
// to a port site B's user listens on (before, any site user's listener was
// exempt), and a proxy_pass host must be an address (before, a name was
// checked at render and resolved again by nginx at every reload). Control:
// A's own listener is reachable.
func (b *bnd) routingReachesOnlyItsOwn(t *testing.T) {
	h := b.h
	portB, portA := 18000+h.pid%1000, 19000+h.pid%1000
	listen := func(user string, port int) *bg {
		return execBackground("runuser", "-u", user, "--", "timeout", "120", "python3", "-m", "http.server", fmt.Sprint(port), "--bind", "127.0.0.1")
	}
	lb, la := listen(b.userB, portB), listen(b.userA, portA)
	defer func() {
		lb.stop()
		la.stop()
		h.run("pkill", "-f", fmt.Sprintf("http.server %d", portB))
		h.run("pkill", "-f", fmt.Sprintf("http.server %d", portA))
	}()
	listening := func(port int) bool {
		return strings.TrimSpace(h.run("ss", "-Hltn", fmt.Sprintf("sport = :%d", port)).Out) != ""
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !(listening(portA) && listening(portB)) {
		time.Sleep(200 * time.Millisecond)
	}
	h.assert(t, "R-10.5/A control: both sites' listeners are up", listening(portA) && listening(portB), "listeners not up")
	defer h.setRouting(b.idA, "")
	c, body := h.setRouting(b.idA, fmt.Sprintf("location /x/ {\n    proxy_pass http://127.0.0.1:%d;\n}\n", portA))
	h.check(t, "R-10.5/A control: a proxy to the site's own listener is accepted", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("PUT: %s", body)
	}
	c, body = h.setRouting(b.idA, fmt.Sprintf("location /x/ {\n    proxy_pass http://127.0.0.1:%d;\n}\n", portB))
	h.assert(t, "R-10.5/A: a proxy to another site's listener is refused", c == 400 && strings.Contains(body, fmt.Sprintf("port %d", portB)),
		"PUT = %d %s", c, body)
	c, body = h.setRouting(b.idA, "location /x/ {\n    proxy_pass https://example.com/;\n}\n")
	h.assert(t, "R-10.5/B: a proxy_pass to a host name is refused", c == 400 && strings.Contains(body, "host name"), "PUT = %d %s", c, body)
}

// agentLimits is the exclusive R-10.5 section (R-10.5/8): the agent serves
// at most 64 requests at once and refuses the next with 503 at once. 64
// requests whose body never arrives hold every slot; a Ping then gets 503;
// once they are closed, Ping answers 200. Before, every request got a
// goroutine. Exclusive: while the slots are held every other caller of the
// agent is refused too.
func (h *harness) agentLimits(t *testing.T) {
	tok, err := os.ReadFile("/etc/cloudground/agent.token")
	if err != nil {
		h.fail(t, "R-10.5/8 fixture: the agent's token", "R-10.5/8 fixture: %v", err)
		return
	}
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 64; i++ {
		c, err := net.Dial("unix", agentSock)
		if err != nil {
			break
		}
		conns = append(conns, c)
		fmt.Fprintf(c, "POST /rpc/Ping HTTP/1.1\r\nHost: agent\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n",
			strings.TrimSpace(string(tok)))
	}
	time.Sleep(500 * time.Millisecond)
	code, body, err := h.agentCall("Ping", "{}")
	h.assert(t, "R-10.5/8: past 64 requests in flight the agent answers 503 at once", err == nil && code == 503,
		"Ping with %d requests held: %d %s %v", len(conns), code, head(body, 200), err)
	for _, c := range conns {
		c.Close()
	}
	conns = nil
	time.Sleep(500 * time.Millisecond)
	code, _, err = h.agentCall("Ping", "{}")
	h.check(t, "R-10.5/8: once they end, the agent serves again", fmt.Sprint(code, err), "200 <nil>")
}

// runCommandIsNotExpanded (R-10.5/8): a "$VAR" in a sandboxed run's command
// reaches the command as written; systemd does not expand it. Before,
// systemd-run's ExecStart expanded $HOME in a cron job's command into the
// site's root before the job's shell saw it.
func (b *bnd) runCommandIsNotExpanded(t *testing.T) {
	h := b.h
	c, body := h.api.post(fmt.Sprintf("/api/sites/%d/cron", b.idA), map[string]string{"schedule": "0 5 * * *", "command": `printf 'v=%s\n' '$HOME'`})
	id := jsonField(body, "id")
	h.assert(t, "R-10.5/8 control: a cron job is created", c == 201 || c == 200 && id != "", "create cron: %d %s", c, head(body, 200))
	if id == "" {
		return
	}
	defer h.api.del("/api/cron/" + id)
	c, body = h.api.post("/api/cron/"+id+"/run", map[string]string{})
	out := jsonField(body, "output")
	h.assert(t, "R-10.5/8: a $VAR in a run's command is not expanded by systemd", c == 200 && strings.Contains(out, "v=$HOME"),
		"run now: %d %q", c, out)
}

// shellLoginIsForced (R-10.5/8): sshd's effective configuration for a
// site's shell login forces the gate (ForceCommand), so a global Subsystem
// or ForceCommand cannot give it an SFTP session without a chroot.
func (b *bnd) shellLoginIsForced(t *testing.T) {
	h := b.h
	api := fmt.Sprintf("/api/sites/%d", b.idA)
	h.check(t, "R-10.5/8 control: the shell is turned on", code(h.api.post(api+"/ssh-shell", `{"enable":true}`)), "200")
	defer h.api.post(api+"/ssh-shell", `{"enable":false}`)
	shellUser := strings.Replace(b.userA, "cg-site-", "cg-shell-", 1)
	out := h.run("sshd", "-T", "-C", "user="+shellUser+",host=localhost,addr=127.0.0.1").Out
	h.assert(t, "R-10.5/8: the shell login's sessions are forced to the gate", strings.Contains(strings.ToLower(out), "forcecommand cloudground-site-shell"),
		"sshd -T for %s: no forcecommand", shellUser)
}

// connectorRefusesLinkedWPContent (R-10.5/7): a deployed WordPress tree
// whose wp-content is a link (here to the site's .ssh) does not get the
// connector written through it: the deploy fails and .ssh is untouched.
// Before, the agent made wp-content/mu-plugins through the link, in .ssh,
// as root. The deploy goes to the agent directly with site A's record as a
// WordPress site, the only type the connector is installed for.
func (b *bnd) connectorRefusesLinkedWPContent(t *testing.T) {
	h := b.h
	src := b.rootA + "/shared/cg-e2e-r10-wp"
	defer h.remove(src)
	h.run("runuser", "-u", b.userA, "--", "mkdir", "-p", src)
	h.mustWriteAs(t, src+"/index.php", "<?php echo 'r10';", b.userA)
	h.run("runuser", "-u", b.userA, "--", "ln", "-s", "../../.ssh", src+"/wp-content")
	var rec map[string]any
	_ = json.Unmarshal(b.siteJSON(b.idA), &rec)
	rec["type"] = "wordpress"
	id := time.Now().UTC().Format("20060102-150405.000") + "-r10a5b"
	status, body := b.rpc.call("DeployRelease", map[string]any{"site": rec, "source_dir": src, "release_id": id}, 600)
	defer h.remove(b.rootA + "/releases/" + id)
	h.assert(t, "R-10.5/7: a deploy whose wp-content is a link is refused", status != "200", "DeployRelease: %s %s", status, head([]byte(body), 200))
	h.assert(t, "R-10.5/7: nothing was written into .ssh", !exists(b.rootA+"/.ssh/mu-plugins"), ".ssh/mu-plugins exists")
	h.assert(t, "R-10.5/7: the site still serves", contains(h.get(b.domA, "/").Body, "r10-ok"), "the site does not serve")
}

// fpmTestIsSandboxed (R-10.5/6): the php-fpm configuration test of a
// render runs as a transient service in the site's sandbox, not by runuser
// in the agent's own mount namespace: the journal shows its unit.
func (b *bnd) fpmTestIsSandboxed(t *testing.T) {
	h := b.h
	since := fmt.Sprintf("@%d", time.Now().Unix()-1)
	status, body := b.rpc.call("ApplySiteConfig", map[string]any{"site": b.siteJSON(b.idA)}, 300)
	h.check(t, "R-10.5/6 control: a full apply of site A succeeds", status, "200")
	if status != "200" {
		t.Logf("ApplySiteConfig: %s", head([]byte(body), 300))
	}
	j := h.run("journalctl", "--no-pager", "--since", since, "-o", "cat", "-u", "cg-fpm-test-"+b.userA+"-*").Out
	h.assert(t, "R-10.5/6: the php-fpm configuration test ran as a transient service of the site", strings.Contains(j, "php-fpm configuration test for "+b.domA),
		"journal of cg-fpm-test-%s-*: %q", b.userA, head([]byte(j), 300))
	h.assert(t, "R-10.5/6: the site still serves after the apply", contains(h.get(b.domA, "/").Body, "r10-ok"), "the site does not serve")
}

// repositoryIsValidated (R-10.5/5): the agent refuses a local repository
// outside /var/backups/cloudground and /mnt before restic runs (before,
// it made one wherever it was told), and the API refuses a loopback rest:
// endpoint by the rule, not because the test against it failed.
func (b *bnd) repositoryIsValidated(t *testing.T) {
	h := b.h
	outside := fmt.Sprintf("/var/backups/cg-e2e-r10-outside-%d", h.pid)
	defer h.remove(outside)
	status, body := b.rpc.call("RunBackup", map[string]any{"site": b.siteJSON(b.idA), "repository": outside, "password": b.pw, "kind": "files"}, 600)
	h.assert(t, "R-10.5/5: the agent refuses a local repository outside the backup roots", status == "400" && !exists(outside),
		"RunBackup into %s: %s %s (exists %v)", outside, status, head([]byte(body), 200), exists(outside))
	c, rb := h.api.put("/api/settings", `{"backup_repository":"rest:https://127.0.0.1:1/","backup_password":"r10-not-saved-password"}`)
	h.assert(t, "R-10.5/5: the API refuses a loopback rest: endpoint by the rule", c == 400 && contains(rb, "private or local address"),
		"PUT settings: %d %s", c, head(rb, 200))
	_, sb := h.api.get("/api/settings")
	h.assert(t, "R-10.5/5: the refused repository is not stored", !contains(sb, "127.0.0.1:1"), "settings: %s", head(sb, 300))
}

// backupSeesOnlyTheSite (R-10.5/4): the restic that walks a site's tree for
// its files backup runs where only that tree (and restic's own paths) is
// visible. A stand-in ahead of restic on the agent's PATH records, for this
// section's repository only, what the backup's restic can see, then execs
// the real one. Before, it ran as the agent's root with the whole machine
// in view. A link the tenant planted to a root-only file is stored as a
// link.
func (b *bnd) backupSeesOnlyTheSite(t *testing.T) {
	h := b.h
	h.clearStandIns(t, "restic")
	real, err := exec.LookPath("restic")
	if err != nil {
		h.fail(t, "R-10.5/4 fixture: restic is on PATH", "R-10.5/4 fixture: %v", err)
		return
	}
	const standIn = "/usr/local/sbin/restic"
	probe := b.repo + "/cg-e2e-probe"
	script := fmt.Sprintf(`#!/bin/sh
if [ "$RESTIC_REPOSITORY" = %q ] && [ "$1" = backup ] && [ "$2" = --json ]; then
  { echo "sites: $(ls /srv/sites | tr '\n' ' ')"; [ -e /etc/shadow ] && echo "system-files-visible"; [ -e /var/lib/cloudground-agent ] && echo "agent-state-visible"; echo done; } > %q 2>&1
fi
exec %q "$@"
`, b.repo, probe, real)
	defer h.undo("remove the R-10.5/4 restic stand-in", func() { _ = os.Remove(standIn) })()
	if err := os.WriteFile(standIn, []byte(script), 0o755); err != nil {
		h.fail(t, "R-10.5/4 fixture: the restic stand-in", "R-10.5/4 fixture: %v", err)
		return
	}
	link := b.rootA + "/shared/cg-e2e-r10-root-file"
	defer h.remove(link)
	h.run("runuser", "-u", b.userA, "--", "ln", "-s", "/var/lib/cloudground-agent/sites.json", link)
	snap := b.backup(t, b.idA)
	h.assert(t, "R-10.5/4 control: the backup succeeds", snap != "", "no snapshot")
	got := readTrim(probe)
	h.assert(t, "R-10.5/4 control: the stand-in saw the files backup", strings.HasSuffix(got, "done"), "probe: %q", got)
	h.assert(t, "R-10.5/4: the backup's restic sees only its own site under /srv/sites",
		strings.Contains(got, "sites: "+b.domA+" ") && !strings.Contains(got, b.domB), "probe: %q", got)
	h.assert(t, "R-10.5/4: the backup's restic sees no system or agent files",
		!strings.Contains(got, "system-files-visible") && !strings.Contains(got, "agent-state-visible"), "probe: %q", got)
	if snap != "" {
		ls := b.restic("ls", "--json", snap, link)
		out := ls.Out
		h.assert(t, "R-10.5/4: a planted link to a root-only file is stored as a link",
			strings.Contains(out, `"type":"symlink"`) && !strings.Contains(out, `"type":"file"`), "restic ls (exit %d): %s %s", ls.Code, head([]byte(out), 300), head([]byte(ls.Err), 300))
	}
}

// siteIndexTiesTheRecord (R-10.5/3): the agent refuses a request whose site
// pairs site B's id and user with site A's domain and tree; the agent's own
// index of what it provisioned is root's, 0600. Before, every field was
// checked on its own and the request ran on A's tree as B's user.
func (b *bnd) siteIndexTiesTheRecord(t *testing.T) {
	h := b.h
	var rec map[string]any
	_ = json.Unmarshal(b.siteJSON(b.idA), &rec)
	status, body := b.rpc.call("CurrentRelease", map[string]any{"site": rec}, 60)
	h.check(t, "R-10.5/3 control: the agent serves site A's own record", status, "200")
	rec["id"], rec["system_user"] = b.idB, b.userB
	// Fase 9b: A's database is named site_<A>, which the record check
	// already refuses to site B's id. Neutral names that any site may claim
	// leave the site index as the only thing that can refuse the record.
	if cfg, ok := rec["config"].(map[string]any); ok {
		if d, ok := cfg["database"].(map[string]any); ok && d["enabled"] == true {
			n := fmt.Sprintf("rten_tie_%d", h.pid)
			d["name"], d["user"] = n, n
		}
	}
	status, body = b.rpc.call("CurrentRelease", map[string]any{"site": rec}, 60)
	h.assert(t, "R-10.5/3: a record pairing B's id and user with A's domain is refused", status == "400" && strings.Contains(body, "does not match"),
		"CurrentRelease with B's id on A's domain: %s %s", status, head([]byte(body), 200))
	fi, err := os.Stat("/var/lib/cloudground-agent/sites.json")
	h.assert(t, "R-10.5/3: the agent's site index is root's, 0600", err == nil && fi.Mode().Perm() == 0o600 && ownerOf("/var/lib/cloudground-agent/sites.json") == "0:0",
		"sites.json: %v %v", fi, err)
	idx := readTrim("/var/lib/cloudground-agent/sites.json")
	h.assert(t, "R-10.5/3: the index records both sites", strings.Contains(idx, fmt.Sprintf("%q: %q", fmt.Sprint(b.idA), b.domA)) &&
		strings.Contains(idx, fmt.Sprintf("%q: %q", fmt.Sprint(b.idB), b.domB)), "sites.json: %s", idx)
}

// fileManagerLeavesTheSkeleton (R-10.5/2): the file manager, which runs as
// root, cannot write .ssh/shell_keys (directly, or through a link the
// tenant planted in its shared/), nor delete current or shared. Before, it
// could: the skeleton is root's, and root may write it.
func (b *bnd) fileManagerLeavesTheSkeleton(t *testing.T) {
	h := b.h
	api := func(op string) string { return fmt.Sprintf("/api/sites/%d/files/%s", b.idA, op) }
	shell := b.rootA + "/.ssh/shell_keys"
	link := b.rootA + "/shared/cg-e2e-r10-ssh"
	defer h.remove(link)
	before := readTrim(shell)
	h.run("runuser", "-u", b.userA, "--", "ln", "-s", "../.ssh", link)
	fi, err := os.Lstat(link)
	h.assert(t, "R-10.5/2 control: the tenant's link into .ssh exists", err == nil && fi.Mode()&os.ModeSymlink != 0, "no link: %v", err)
	for _, p := range []string{".ssh/shell_keys", "shared/cg-e2e-r10-ssh/shell_keys"} {
		body, _ := json.Marshal(map[string]string{"path": p, "content": "ssh-ed25519 AAAA planted"})
		c, rb := h.api.post(api("write"), body)
		h.assert(t, "R-10.5/2: the file manager cannot write "+p, c == 400, "write %s: %d %s", p, c, head(rb, 200))
	}
	h.assert(t, "R-10.5/2: shell_keys is unchanged", readTrim(shell) == before && !strings.Contains(readTrim(shell), "planted"),
		"shell_keys: %q", readTrim(shell))
	for _, p := range []string{"current", "shared", ".ssh"} {
		body, _ := json.Marshal(map[string]string{"operation": "delete", "path": p})
		c, rb := h.api.post(api("manage"), body)
		h.assert(t, "R-10.5/2: the file manager cannot delete "+p, c == 400 && exists(b.rootA+"/"+p), "delete %s: %d %s", p, c, head(rb, 200))
	}
	// Control: inside shared/ the file manager still works.
	c, rb := h.api.post(api("write"), `{"path":"shared/cg-e2e-r10.txt","content":"ok"}`)
	defer h.remove(b.rootA + "/shared/cg-e2e-r10.txt")
	h.check(t, "R-10.5/2 control: the file manager writes inside shared/", fmt.Sprint(c), "200")
	if c != 200 {
		t.Logf("write: %s", head(rb, 200))
	}
	h.assert(t, "R-10.5/2: the site still serves", contains(h.get(b.domA, "/").Body, "r10-ok"), "the site does not serve its index.php")
}

// bnd is what the R-10.5 checks share.
type bnd struct {
	h                        *harness
	rpc                      *agentRPC
	idA, idB                 int64
	domA, domB, rootA, rootB string
	userA, userB             string
	repo, pw                 string
}

// siteJSON is the panel's record of a site, as the API sends it to the
// agent.
func (b *bnd) siteJSON(id int64) json.RawMessage {
	_, body := b.h.api.get(fmt.Sprintf("/api/sites/%d", id))
	return json.RawMessage(body)
}

// backup takes a files backup of site id into the section's repository and
// returns its snapshot id ("" when it failed).
func (b *bnd) backup(t *testing.T, id int64) string {
	status, body := b.rpc.call("RunBackup", map[string]any{"site": b.siteJSON(id), "repository": b.repo, "password": b.pw, "kind": "files"}, 600)
	if status != "200" {
		t.Logf("RunBackup: %s %s", status, head([]byte(body), 300))
		return ""
	}
	return jsonField([]byte(body), "snapshot_id")
}

// restic runs the real restic on the section's repository.
func (b *bnd) restic(args ...string) result {
	return b.h.resticOn("restic", b.repo, b.pw)(args...)
}

// ownerOf is "uid:gid" of a path, not followed.
func ownerOf(p string) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return "missing"
	}
	st := fi.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
}

// restoreKeepsRevokedKeysOut (R-10.5/1): a restore does not bring back a key
// revoked after the snapshot, and every file of the restored .ssh is
// root's. Before, the snapshot's .ssh came back as the site user's, its
// shell_keys included, and with the revoked keys in it.
func (b *bnd) restoreKeepsRevokedKeysOut(t *testing.T) {
	h := b.h
	kdir, err := os.MkdirTemp("", "cg-e2e-r10-keys-")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	defer os.RemoveAll(kdir)
	key := filepath.Join(kdir, "k")
	h.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "cg-e2e-r10", "-f", key)
	pub := readTrim(key + ".pub")
	data := strings.Fields(pub)
	api := fmt.Sprintf("/api/sites/%d/ssh-keys", b.idA)
	body, _ := json.Marshal(map[string]string{"public_key": pub, "access": "shell"})
	h.check(t, "R-10.5/1 control: a shell key is added", code(h.api.post(api, body)), "201")
	keys, shell := b.rootA+"/.ssh/authorized_keys", b.rootA+"/.ssh/shell_keys"
	h.assert(t, "R-10.5/1 control: the key is in shell_keys", len(data) > 1 && strings.Contains(readTrim(shell), data[1]), "shell_keys: %q", readTrim(shell))

	snap := b.backup(t, b.idA)
	h.assert(t, "R-10.5/1 control: the site with the key is backed up", snap != "", "no snapshot")
	if snap == "" {
		return
	}
	var list struct {
		Keys []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"keys"`
	}
	_, lb := h.api.get(api)
	_ = json.Unmarshal(lb, &list)
	// A fingerprint is standard base64 and has a "/" about one key in two:
	// unescaped, the DELETE missed the route and the key stayed.
	dels := []string{}
	for _, k := range list.Keys {
		c, db := h.api.del(api + "/" + QuoteAll(k.Fingerprint))
		if c != 200 {
			dels = append(dels, fmt.Sprintf("%s: %d %s", k.Fingerprint, c, head(db, 120)))
		}
	}
	h.assert(t, "R-10.5/1 control: the key is revoked", len(list.Keys) > 0 && len(dels) == 0 && !strings.Contains(readTrim(keys), data[1]),
		"listed %d, deletes %v, authorized_keys still has it: %v", len(list.Keys), dels, strings.Contains(readTrim(keys), data[1]))

	status, rb := b.rpc.call("RestoreSnapshot", map[string]any{"site": b.siteJSON(b.idA), "repository": b.repo, "password": b.pw,
		"snapshot_id": snap, "mode": "files"}, 900)
	h.check(t, "R-10.5/1 control: the files restore succeeds", status, "200")
	if status != "200" {
		t.Logf("RestoreSnapshot: %s", head([]byte(rb), 300))
		return
	}
	h.assert(t, "R-10.5/1: a restore does not bring back a key revoked after the snapshot",
		!strings.Contains(readTrim(keys), data[1]) && !strings.Contains(readTrim(shell), data[1]),
		"after the restore: authorized_keys %q, shell_keys %q", readTrim(keys), readTrim(shell))
	_, gid, _ := lookupIDs(b.userA)
	want := fmt.Sprintf("0:%d", gid)
	ents, _ := os.ReadDir(b.rootA + "/.ssh")
	bad := []string{}
	for _, e := range ents {
		if o := ownerOf(b.rootA + "/.ssh/" + e.Name()); o != want {
			bad = append(bad, e.Name()+"="+o)
		}
	}
	h.assert(t, "R-10.5/1: every file of the restored .ssh is root's", len(bad) == 0 && ownerOf(b.rootA+"/.ssh") == want,
		"want %s: .ssh=%s %v", want, ownerOf(b.rootA+"/.ssh"), bad)
}
