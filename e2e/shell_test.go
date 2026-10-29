//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// siteShell (R-4.5): a site's SSH shell. Two sites of the section's own: A,
// a PHP site that gets the shell, and B, a static neighbour. A "shell" key
// opens bash in A as A's uid, as a sandboxed service in A's slice (the PHP
// master's sandbox), with git, composer, wp and php; an "SFTP only" key
// opens no shell; B, root's files, sudo, other users' processes and the
// agent are out of reach; the shell's processes go with the session, when
// the shell is turned off and when the site is deleted; a checkout made in
// the shell becomes a release.
func (h *harness) siteShell(t *testing.T) {
	domA, domB := h.dom("r45a"), h.dom("r45b")
	idA := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, domA, h.PHPV))
	idB := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, domB))
	okA := h.api.waitActive(idA, 4*time.Minute) == "active"
	okB := h.api.waitActive(idB, 4*time.Minute) == "active"
	h.assert(t, "R-4.5: the shell site provisions", okA, "R-4.5: %s (id %d) did not become active", domA, idA)
	h.assert(t, "R-4.5: the neighbour site provisions", okB, "R-4.5: %s (id %d) did not become active", domB, idB)
	if !okA || !okB {
		return
	}
	userA := h.api.site(idA).SystemUser
	rootA, rootB := "/srv/sites/"+domA, "/srv/sites/"+domB
	uidA := h.run("id", "-u", userA).Trim()
	api := func(format string, a ...any) string {
		return fmt.Sprintf("/api/sites/%d", idA) + fmt.Sprintf(format, a...)
	}

	kdir, err := os.MkdirTemp("", "cg-e2e-shell-")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	h.undo("remove the shell keys", func() { _ = os.RemoveAll(kdir) })
	shellKey, sftpKey := kdir+"/shell", kdir+"/sftp"
	for name, path := range map[string]string{"cg-e2e-shell": shellKey, "cg-e2e-sftp": sftpKey} {
		h.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path)
	}
	addKey := func(path, access string) string {
		body, _ := json.Marshal(map[string]string{"public_key": readTrim(path + ".pub"), "access": access})
		return code(h.api.post(api("/ssh-keys"), body))
	}
	h.check(t, "R-4.5: a key of type shell is added", addKey(shellKey, "shell"), "201")
	h.check(t, "R-4.5: a key of type SFTP only is added", addKey(sftpKey, "sftp"), "201")
	var list struct {
		Keys []struct {
			Label  string `json:"label"`
			Access string `json:"access"`
		} `json:"keys"`
		ShellUser string `json:"shell_user"`
	}
	_, lb := h.api.get(api("/ssh-keys"))
	_ = json.Unmarshal(lb, &list)
	access := map[string]string{}
	for _, k := range list.Keys {
		access[k.Label] = k.Access
	}
	h.check(t, "R-4.5: the key list carries each key's type", access["cg-e2e-shell"]+" "+access["cg-e2e-sftp"], "shell sftp")
	shellUser := list.ShellUser
	h.check(t, "R-4.5: the shell login is named after the site user", shellUser, strings.Replace(userA, "cg-site-", "cg-shell-", 1))
	h.check(t, "R-4.5: SFTP access is turned on", code(h.api.post(api("/sftp"), `{"enable":true,"password":"cg-e2e-shell-pass-1"}`)), "200")

	port := h.sshPort()
	sshOpts := func(key string) []string {
		return []string{"60", "ssh", "-i", key, "-p", port, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR"}
	}
	sh := func(key, login, command string) result {
		return h.run("timeout", append(sshOpts(key), "-n", login+"@127.0.0.1", command)...)
	}
	const probe = "id -u"

	// Before the shell is on, a shell key opens nothing.
	h.assert(t, "R-4.5 control: no shell before the shell is turned on", !sh(shellKey, shellUser, probe).OK(),
		"a shell opened for %s before the shell was turned on", shellUser)

	h.check(t, "R-4.5: the shell is turned on", code(h.api.post(api("/ssh-shell"), `{"enable":true}`)), "200")
	_, sb := h.api.get(fmt.Sprintf("/api/sites/%d", idA))
	h.check(t, "R-4.5: the shell switch is stored", fmt.Sprint(strings.Contains(string(sb), `"shell_enabled":true`)), "true")

	// A shell key: bash, as the site's uid, in a service of the site's slice.
	r := sh(shellKey, shellUser, "id -u; id -un; cat /proc/self/cgroup")
	lines := strings.Split(r.Trim(), "\n")
	h.assert(t, "R-4.5: a shell key opens a shell", r.OK() && len(lines) >= 3, "R-4.5: ssh %s: %s", shellUser, r)
	if len(lines) >= 3 {
		h.check(t, "R-4.5: the shell runs as the site's uid", lines[0], uidA)
		h.check(t, "R-4.5: the shell's uid is the site user", lines[1], userA)
		h.assert(t, "R-4.5: the shell runs as a service in the site's slice",
			strings.Contains(r.Out, "/cg-site.slice/"+userA+".slice/"+shellUser+"-") && strings.Contains(r.Out, ".service"),
			"R-4.5: cgroup %q", strings.Join(lines[2:], " "))
	}
	h.check(t, "R-4.5: a command's exit status comes back through ssh", strconv.Itoa(sh(shellKey, shellUser, "exit 7").Code), "7")

	// F-6-2: a site's code binds no TCP or UDP port: not the connector's
	// 9080 (held or not), not a free one. Port 0 (an ephemeral port the
	// kernel picks) still works. Asserted wherever this host's systemd can
	// enforce SocketBindDeny (249 or later, built with the BPF framework:
	// Ubuntu 26.04); skipped, with the reason, only where it says it cannot
	// (Ubuntu 24.04's 255 is built without it: phpfpm SPEC §4.1).
	bound := sh(shellKey, shellUser, "python3 -c '"+bindProbe+"' 9080 "+freePort+" 0")
	if ok, why := h.socketBindSupported(); ok {
		h.check(t, "F-6-2: the site shell cannot bind the connector's port, a free port or UDP; port 0 still works",
			bound.Trim(), "tcp 9080 1\ntcp "+freePort+" 1\nudp "+freePort+" 1\ntcp 0 ok")
	} else {
		t.Logf("F-6-2: skipped, %s; the shell's binds: %q", why, bound.Trim())
	}
	// A tool opens one session per command (a deploy script, Ansible): a
	// run of them, back to back, is not refused. The gate allowed 20 a
	// minute, and the 21st command of this section was refused.
	burst, refused := 0, ""
	for i := 0; i < 30; i++ {
		if r := sh(shellKey, shellUser, "true"); r.OK() {
			burst++
		} else if refused == "" {
			refused = r.String()
		}
	}
	h.assert(t, "R-4.5: 30 commands in a row each get a shell", burst == 30, "R-4.5: %d of 30 ran; the first refusal: %s", burst, refused)
	// An SFTP-only key opens no shell, on either login.
	h.assert(t, "R-4.5: an SFTP-only key opens no shell", !sh(sftpKey, shellUser, probe).OK(),
		"R-4.5: the SFTP-only key opened a shell")
	sftpLogin := sh(shellKey, userA, probe)
	h.assert(t, "R-4.5: the SFTP login gives a shell key no shell either", !strings.Contains(sftpLogin.Out, uidA),
		"R-4.5: ssh %s ran a command: %s", userA, sftpLogin)
	sftpPwd := h.runIn("pwd\n", "timeout", append(append([]string{"60", "sftp", "-q", "-b", "-"}, sftpOpts(sftpKey, port)...), userA+"@127.0.0.1")...)
	h.assert(t, "R-4.5: an SFTP-only key still signs in to SFTP, chrooted", sftpPwd.OK() && strings.Contains(sftpPwd.Out, "Remote working directory: /\n"),
		"R-4.5: sftp: %s", sftpPwd)

	// The tools.
	for _, tool := range []struct{ name, cmd, want string }{
		{"git", "git --version", "git version"},
		{"composer", "composer --version --no-ansi", "Composer"},
		{"wp", "wp --version", "WP-CLI"},
		{"php", "php -v", "PHP "},
	} {
		r := sh(shellKey, shellUser, tool.cmd)
		h.assert(t, "R-4.5: "+tool.name+" starts in the shell", r.OK() && strings.Contains(r.Out, tool.want),
			"R-4.5: %s: %s", tool.cmd, r)
	}

	// The sandbox: the PHP master's.
	sites := strings.Fields(sh(shellKey, shellUser, "ls /srv/sites").Trim())
	h.check(t, "R-4.5: /srv/sites in the shell holds only the site itself", strings.Join(sites, " "), domA)
	h.assert(t, "R-4.5: another site's tree is absent from the shell", !sh(shellKey, shellUser, "test -e "+rootB).OK(),
		"R-4.5: %s exists in the shell", rootB)
	for _, c := range []struct{ name, cmd string }{
		{"root's home cannot be listed", "ls /root"},
		{"the shadow file cannot be read", "cat /etc/shadow"},
		{"the panel's configuration is absent", "ls /etc/cloudground"},
		{"su gains nothing", "su -c true root </dev/null"},
		{"no namespace can be made (unshare -U)", "unshare -U true"},
	} {
		h.assert(t, "R-4.5: "+c.name, !sh(shellKey, shellUser, c.cmd).OK(), "R-4.5: %q succeeded in the shell", c.cmd)
	}
	sudo := sh(shellKey, shellUser, "sudo -n true")
	h.assert(t, "R-4.5: sudo is refused by no-new-privileges", !sudo.OK() && strings.Contains(strings.ToLower(sudo.Out+sudo.Err), "no new privileges"),
		"R-4.5: sudo -n true: %s", sudo)
	uids := strings.Fields(sh(shellKey, shellUser, "ps -e -o uid=").Trim())
	foreign := 0
	for _, u := range uids {
		if u != uidA {
			foreign++
		}
	}
	h.assert(t, "R-4.5: ps -e in the shell shows only the site's own processes", len(uids) > 0 && foreign == 0,
		"R-4.5: ps -e uids %v", uids)
	h.check(t, "R-4.5 control: root sees other users' processes on the host", fmt.Sprint(procsOfUID("0") > 0), "true")
	hostMark, shellMark := fmt.Sprintf("/tmp/cg-e2e-host-%d", h.pid), fmt.Sprintf("/tmp/cg-e2e-shell-%d", h.pid)
	_ = os.WriteFile(hostMark, []byte("host"), 0o644)
	h.undo("remove the /tmp marks", func() { _ = os.Remove(hostMark); _ = os.Remove(shellMark) })
	h.assert(t, "R-4.5: the shell's /tmp is private (the host's file is not there)", !sh(shellKey, shellUser, "test -e "+hostMark).OK(),
		"R-4.5: %s is visible in the shell", hostMark)
	wrote := sh(shellKey, shellUser, "echo shell > "+shellMark+" && test -e "+shellMark).OK()
	h.assert(t, "R-4.5: the shell's /tmp is private (its file stays out of the host's)", wrote && !exists(shellMark),
		"R-4.5: wrote=%v, on the host: %v", wrote, exists(shellMark))
	groups := strings.Fields(sh(shellKey, shellUser, "id -Gn").Trim())
	h.check(t, "R-4.5: the shell has only the site's group", strings.Join(groups, " "), userA)
	// Unix sockets: the agent's and the gate are refused; MariaDB's, which
	// anyone may reach, is the control that the probe can connect at all.
	sock := func(addr string) string {
		if strings.HasPrefix(addr, "/") {
			addr = "unix://" + addr
		}
		return sh(shellKey, shellUser, fmt.Sprintf(`php -r 'echo @stream_socket_client("%s") ? "CONNECTED" : "REFUSED";'`, addr)).Trim()
	}
	h.check(t, "R-4.5 control: the shell can connect to a socket open to all (MariaDB's)", sock("/run/mysqld/mysqld.sock"), "CONNECTED")
	h.check(t, "R-4.5: the shell cannot connect to the agent", sock("/run/cloudground/agent.sock"), "REFUSED")
	h.check(t, "R-4.5: the shell cannot connect to the shell gate", sock("/run/cloudground/shell.sock"), "REFUSED")
	h.check(t, "R-4.5: the shell cannot connect to the panel API's socket (nginx's only)", sock("/run/cloudground/api.sock"), "REFUSED")
	probePHP := readlink(rootA+"/current") + "/cg-e2e-gate.php"
	h.mustWriteAs(t, rootA+"/"+probePHP, `<?php echo @stream_socket_client("unix:///run/cloudground/shell.sock") ? "CONNECTED" : "REFUSED";`, userA)
	h.check(t, "R-4.5: the site's PHP cannot connect to the shell gate", strings.TrimSpace(string(h.get(domA, "/cg-e2e-gate.php").Body)), "REFUSED")
	h.remove(rootA + "/" + probePHP)
	// crontab cannot work in the shell: it is setgid crontab, and under
	// NoNewPrivileges the setgid gains nothing, so it cannot read or write
	// cron's spool. (The site's schedule is root's /etc/cron.d file.)
	h.assert(t, "R-4.5: crontab -l is refused in the shell (no new privileges)", !sh(shellKey, shellUser, "crontab -l").OK(), "R-4.5: crontab -l worked")
	h.assert(t, "R-4.5: crontab cannot be replaced from the shell (no new privileges)", !sh(shellKey, shellUser, "echo '* * * * * true' | crontab -").OK(),
		"R-4.5: crontab - worked")
	// Control: the panel's own cron jobs still install and fire with the
	// shell on.
	cronMark := fmt.Sprintf("logs/cg-e2e-shellcron-%d", h.pid)
	_, cb := h.api.post(api("/cron"), map[string]string{"schedule": "* * * * *", "command": "date > " + cronMark})
	cronID := jsonField(cb, "id")
	h.assert(t, "R-4.5 control: a panel cron job made with the shell on is scheduled",
		cronID != "" && strings.Contains(readTrim("/etc/cron.d/cloudground-"+userA)+"\n", " root /usr/local/bin/cg-site-cron "+userA+" "+cronID+"\n"),
		"R-4.5: job %q, cron file %q", cronID, readTrim("/etc/cron.d/cloudground-"+userA))
	fired := false
	for deadline := time.Now().Add(130 * time.Second); !fired && time.Now().Before(deadline); time.Sleep(pollEvery) {
		fired = exists(rootA + "/" + cronMark)
	}
	h.assert(t, "R-4.5 control: the panel cron job fires", fired, "R-4.5: %s/%s not written within two minutes", rootA, cronMark)
	if cronID != "" {
		h.api.del("/api/cron/" + cronID)
	}
	// Nothing else the shell could reach: Redis is not running, MariaDB's
	// root is refused, the agent's sockets are refused (above); the
	// connector's loopback listener needs a site's token (control: it
	// answers 401 without one).
	redis := sock("tcp://127.0.0.1:6379")
	h.assert(t, "R-4.5: nothing listens on Redis's port", redis == "REFUSED", "R-4.5: the probe of 127.0.0.1:6379 answered %q (CONNECTED: something listens; empty: the probe did not run)", redis)
	h.assert(t, "R-4.5: MariaDB's root is refused to the shell", !sh(shellKey, shellUser, "mysql -uroot -e 'select 1'").OK(), "R-4.5: mysql -uroot worked")
	h.check(t, "R-4.5: the connector's loopback listener needs a site's token",
		sh(shellKey, shellUser, `php -r '$c = stream_context_create(["http" => ["method" => "POST", "ignore_errors" => true]]); @file_get_contents("http://127.0.0.1:9080/api/connector/purge", false, $c); echo explode(" ", $http_response_header[0] ?? "x 000")[1];'`).Trim(), "401")
	// The user manager of the uid: masked, not running, after a login.
	h.check(t, "R-4.5: the uid's user manager is masked while the shell is on",
		h.run("systemctl", "show", "-p", "LoadState", "--value", "user@"+uidA+".service").Trim(), "masked")
	h.check(t, "R-4.5: no user manager runs for the uid after shell logins",
		h.run("systemctl", "show", "-p", "ActiveState", "--value", "user@"+uidA+".service").Trim(), "inactive")
	own := sh(shellKey, shellUser, "echo cg > src/cg-e2e-owner && stat -c %U src/cg-e2e-owner").Trim()
	h.check(t, "R-4.5: a file the shell writes belongs to the site user", own, userA)
	// An interactive login: on the session's terminal, with job control, in
	// the live release.
	// TERM is a real terminal's: the runner has none, and the session's TERM
	// would be "dumb", which systemd's terminal integration (W3 below)
	// skips, so the check passed on a build that still printed the errors.
	tty := h.runIn("echo CGPWD=$PWD; echo CGTTY=$(tty); echo CGTERM=$TERM; case $- in *m*) echo CGJOBS=on;; esac; exit\n",
		"env", append([]string{"TERM=xterm-256color", "timeout"}, append(sshOpts(shellKey), "-tt", shellUser+"@127.0.0.1")...)...)
	h.assert(t, "R-4.5: an interactive login lands in ~/current", strings.Contains(tty.Out, "CGPWD="+rootA+"/current"),
		"R-4.5: interactive session: %s", tty)
	h.assert(t, "R-4.5: an interactive login has its terminal", strings.Contains(tty.Out, "CGTTY=/dev/pts/"), "R-4.5: interactive session: %s", tty)
	h.assert(t, "R-4.5: an interactive login has job control", strings.Contains(tty.Out, "CGJOBS=on"), "R-4.5: interactive session: %s", tty)
	// W3: systemd's terminal integration
	// (/etc/profile.d/80-systemd-osc-context.sh, Ubuntu 26.04) read
	// /proc/sys/kernel/random/*, which the sandbox hides: two errors at
	// every prompt. It runs only when TERM is not "dumb" (control); with a
	// pty, the shell's stderr is the terminal, so the errors are in tty.Out.
	h.assert(t, "W3 control: the interactive login has the client's TERM", strings.Contains(tty.Out, "CGTERM=xterm-256color"),
		"W3: interactive session: %s", tty)
	h.assert(t, "W3: an interactive login prints no /proc/sys/kernel/random errors",
		!strings.Contains(tty.Out+tty.Err, "/proc/sys/kernel/random") && !strings.Contains(tty.Out+tty.Err, "No such file or directory"),
		"W3: interactive session: %s", tty)
	// The script itself is out of the session's reach (the sandbox's
	// InaccessiblePaths entry), on a system that has it.
	const oscContext = "/etc/profile.d/80-systemd-osc-context.sh"
	if exists(oscContext) {
		h.check(t, "W3: the shell cannot read systemd's terminal-integration login script",
			sh(shellKey, shellUser, "test -r "+oscContext+" && echo READABLE || echo HIDDEN").Trim(), "HIDDEN")
		h.check(t, "W3: a login shell's stderr is empty",
			sh(shellKey, shellUser, "TERM=xterm-256color bash -lic 'echo CGLOGIN' 2>&1 >/dev/null | grep -v 'no job control\\|cannot set terminal process group' | wc -c").Trim(), "0")
	}

	// A checkout made in the shell becomes a release, and rolls back.
	marker := fmt.Sprintf("cg-shell-release-%d", h.pid)
	repo := sh(shellKey, shellUser, fmt.Sprintf(
		`set -e; mkdir -p ~/src/app && cd ~/src/app && printf '<?php echo "%s";' > index.php && git init -q && git add index.php && git -c user.email=e2e@cloudground.invalid -c user.name=e2e commit -qm e2e && git log --oneline | wc -l`, marker))
	h.check(t, "R-4.5: a repository is committed in ~/src from the shell", repo.Trim(), "1")
	before := readlink(rootA + "/current")
	_, db := h.api.post(api("/deployments"), map[string]string{"source_dir": rootA + "/src/app"})
	rel := jsonField(db, "release_id")
	h.check(t, "R-4.5: the checkout is built as a release", h.api.waitTask(taskID(db), 300*time.Second), "succeeded")
	var deps []struct {
		ID        int64  `json:"id"`
		ReleaseID string `json:"release_id"`
	}
	_, lb = h.api.get(api("/deployments"))
	_ = json.Unmarshal(lb, &deps)
	var depID int64
	for _, d := range deps {
		if d.ReleaseID == rel {
			depID = d.ID
		}
	}
	_, pb := h.api.post(fmt.Sprintf("/api/deployments/%d/promote", depID), `{}`)
	h.check(t, "R-4.5: the checkout's release is promoted", h.api.waitTask(taskID(pb), 120*time.Second), "succeeded")
	time.Sleep(1500 * time.Millisecond) // open_file_cache_valid
	h.assert(t, "R-4.5: the site serves the checkout", strings.Contains(string(h.get(domA, "/").Body), marker),
		"R-4.5: %s does not serve %s", domA, marker)
	h.check(t, "R-4.5: the repository's .git is not served", h.get(domA, "/.git/config").CodeStr(), "403")
	_, rb := h.api.post(api("/rollback"), `{}`)
	h.check(t, "R-4.5: the checkout's release rolls back", h.api.waitTask(taskID(rb), 120*time.Second), "succeeded")
	h.check(t, "R-4.5: the rollback puts the previous release back", readlink(rootA+"/current"), before)

	// Processes: what a session leaves behind ends with it; an open
	// session ends when the shell is turned off and when the site goes.
	m0 := fmt.Sprintf("45%d0", h.pid%10000)
	sh(shellKey, shellUser, "nohup setsid sleep "+m0+" >/dev/null 2>&1 </dev/null &")
	h.check(t, "R-4.5: a process a session leaves behind ends with the session", strconv.Itoa(waitGone("sleep "+m0)), "0")
	open := func(tag string) (string, chan struct{}) {
		mark := fmt.Sprintf("45%d%s", h.pid%10000, tag)
		cmd := exec.Command("timeout", append(sshOpts(shellKey), "-n", shellUser+"@127.0.0.1", "sleep "+mark)...)
		done := make(chan struct{})
		if err := cmd.Start(); err != nil {
			close(done)
			return mark, done
		}
		go func() { _ = cmd.Wait(); close(done) }()
		return mark, done
	}
	ended := func(done chan struct{}) bool {
		select {
		case <-done:
			return true
		case <-time.After(20 * time.Second):
			return false
		}
	}
	m1, s1 := open("1")
	pid := waitPid("sleep " + m1)
	cg, _ := os.ReadFile("/proc/" + pid + "/cgroup")
	h.assert(t, "R-4.5: an open session's process is in the site's slice", pid != "" && strings.Contains(string(cg), "/"+userA+".slice/"+shellUser+"-"),
		"R-4.5: sleep %s (pid %q) cgroup %q", m1, pid, cg)
	// The gate client runs as the site's uid outside the sandbox for the
	// whole session: it is not dumpable, so its /proc/<pid>/root (the
	// host's mounts) is out of the shell's reach. Control: a dumpable
	// process of the same uid outside the sandbox is reachable that way, so
	// the check would see a dumpable client.
	cpid := waitGateClient("sleep " + m1)
	viaClient := sh(shellKey, shellUser, "ls /proc/"+cpid+"/root/srv/sites")
	h.assert(t, "R-4.5: the gate client's /proc/<pid>/root is out of the shell's reach", cpid != "" && !viaClient.OK(),
		"R-4.5: client pid %q, ls: %s", cpid, viaClient)
	ctlMark := fmt.Sprintf("45%d9", h.pid%10000)
	ctl := exec.Command("runuser", "-u", userA, "--", "sleep", ctlMark)
	if err := ctl.Start(); err == nil {
		go ctl.Wait()
		cp := waitPid("sleep " + ctlMark)
		viaCtl := sh(shellKey, shellUser, "ls /proc/"+cp+"/root/srv/sites")
		h.assert(t, "R-4.5 control: a dumpable process of the uid outside the sandbox exposes the host's /srv/sites",
			cp != "" && viaCtl.OK() && strings.Contains(viaCtl.Out, domB), "R-4.5: control pid %q, ls: %s", cp, viaCtl)
		_ = ctl.Process.Kill()
	}
	h.check(t, "R-4.5: the shell is turned off", code(h.api.post(api("/ssh-shell"), `{"enable":false}`)), "200")
	h.check(t, "R-4.5: turning the shell off ends the open session's processes", strconv.Itoa(waitGone("sleep "+m1)), "0")
	h.assert(t, "R-4.5: turning the shell off ends the open ssh session", ended(s1), "R-4.5: ssh still running after the shell was turned off")
	h.assert(t, "R-4.5: the shell login goes with the shell", !h.run("getent", "passwd", shellUser).OK(),
		"R-4.5: getent passwd %s still answers", shellUser)
	h.assert(t, "R-4.5: the uid's user manager is unmasked with the shell off",
		h.run("systemctl", "show", "-p", "LoadState", "--value", "user@"+uidA+".service").Trim() != "masked", "R-4.5: still masked")
	h.assert(t, "R-4.5: the shell key opens nothing once the shell is off", !sh(shellKey, shellUser, probe).OK(),
		"R-4.5: a shell opened after the shell was turned off")
	h.assert(t, "R-4.5: SFTP still works after the shell is off", h.runIn("pwd\n", "timeout", append(append([]string{"60", "sftp", "-q", "-b", "-"}, sftpOpts(shellKey, port)...), userA+"@127.0.0.1")...).OK(),
		"R-4.5: SFTP refused after the shell was turned off")

	// On again, then the site is deleted.
	h.check(t, "R-4.5: the shell is turned on again", code(h.api.post(api("/ssh-shell"), `{"enable":true}`)), "200")
	m2, s2 := open("2")
	h.assert(t, "R-4.5 control: an open session runs before the site is deleted", waitPid("sleep "+m2) != "", "R-4.5: sleep %s did not start", m2)
	mask := "/etc/systemd/system/user@" + uidA + ".service"
	h.check(t, "R-4.5: the uid's user manager is masked while the shell is on", readlink(mask), "/dev/null")
	h.check(t, "R-4.5: the shell site delete accepted", h.api.deleteSite(idA), "202")
	h.check(t, "R-4.5: the shell site teardown completes", h.api.teardownGone(idA), "yes")
	h.check(t, "R-4.5: deleting the site ends the open session's processes", strconv.Itoa(waitGone("sleep "+m2)), "0")
	h.assert(t, "R-4.5: deleting the site ends the open ssh session", ended(s2), "R-4.5: ssh still running after the site was deleted")
	h.assert(t, "R-4.5: the shell login goes with its site", !h.run("getent", "passwd", shellUser).OK(),
		"R-4.5: getent passwd %s still answers", shellUser)
	h.assert(t, "R-4.5: the site user goes too (its group was the shell login's)", !h.run("getent", "group", userA).OK(),
		"R-4.5: getent group %s still answers", userA)
	h.assert(t, "R-4.5: the user manager's mask goes with the site", !exists(mask), "R-4.5: %s outlived its site", mask)

	h.check(t, "R-4.5: the neighbour site delete accepted", h.api.deleteSite(idB), "202")
	h.check(t, "R-4.5: the neighbour site teardown completes", h.api.teardownGone(idB), "yes")
}

func sftpOpts(key, port string) []string {
	return []string{"-i", key, "-P", port, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR"}
}

// waitPid is the pid of the first process whose command line contains s,
// waiting up to 10 s for it ("" if none).
func waitPid(s string) string {
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			if _, err := strconv.Atoi(e.Name()); err != nil || e.Name() == strconv.Itoa(os.Getpid()) {
				continue
			}
			b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			// The local ssh client carries the command too: only the
			// remote sleep itself counts.
			if err == nil && strings.HasPrefix(strings.ReplaceAll(string(b), "\x00", " "), "sleep "+strings.TrimPrefix(s, "sleep ")) {
				return e.Name()
			}
		}
	}
	return ""
}

// waitGone waits up to 20 s for no process to match s and returns how many
// still do.
func waitGone(s string) int {
	n := sleepers(s)
	for end := time.Now().Add(20 * time.Second); n > 0 && time.Now().Before(end); n = sleepers(s) {
		time.Sleep(250 * time.Millisecond)
	}
	return n
}

// sleepers counts the processes whose command line starts with s (not the
// ssh clients that carry s as an argument).
func sleepers(s string) int {
	n := 0
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && strings.HasPrefix(strings.ReplaceAll(string(b), "\x00", " "), s) {
			n++
		}
	}
	return n
}

// waitGateClient is the pid of the shell gate's client running command,
// waiting up to 10 s for it ("" if none). sshd starts it with the login's
// ForceCommand ("cg-site-shell -c cloudground-site-shell") and the client's
// command in SSH_ORIGINAL_COMMAND (IsGateClient); root reads the environ of
// a process that is not dumpable.
func waitGateClient(command string) string {
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			if err != nil {
				continue
			}
			environ, _ := os.ReadFile("/proc/" + e.Name() + "/environ")
			if IsGateClient(cmdline, environ, command) {
				return e.Name()
			}
		}
	}
	return ""
}

// bindProbe tries, on 127.0.0.1, a TCP bind of its first port, a TCP and a
// UDP bind of its second, and a TCP bind of its third, and prints one line
// each: "<proto> <port> ok" or "<proto> <port> <errno>" (1 is EPERM: the
// sandbox's SocketBindDeny, F-6-2). No single quote and no $: it goes
// through bash -c and systemd-run as one argument.
const bindProbe = `import socket, sys
def b(kind, port):
    s = socket.socket(socket.AF_INET, kind)
    try:
        s.bind(("127.0.0.1", port))
        return "ok"
    except OSError as e:
        return str(e.errno)
    finally:
        s.close()
a = [int(x) for x in sys.argv[1:]]
print("tcp", a[0], b(socket.SOCK_STREAM, a[0]))
print("tcp", a[1], b(socket.SOCK_STREAM, a[1]))
print("udp", a[1], b(socket.SOCK_DGRAM, a[1]))
print("tcp", a[2], b(socket.SOCK_STREAM, a[2]))
`

// freePort is a port nothing on the test box listens on.
const freePort = "19279"

// socketBindSupported reports whether this host's systemd can enforce
// SocketBindDeny (phpfpm SPEC §4.1), as `systemctl --version` says: version
// 249 or later and +BPF_FRAMEWORK. When it cannot, the reason. Where it can,
// a bind that works is a failure, libbpf missing or PID 1 not re-executed
// after it was installed included.
func (h *harness) socketBindSupported() (bool, string) {
	r := h.run("systemctl", "--version")
	return socketBindFrom(r.Out)
}
