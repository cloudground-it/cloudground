//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// managedApps (#28): a Node.js site and a Python site, each provisioned with
// its runtime (Node.js from nodejs.org, Python's venv from the distro), run
// their app as a unit of their own in the site's slice, as the site user,
// behind nginx on a unix socket. A tiny Node app is deployed twice and
// rolled back, its environment and a sealed secret reach it, it is started
// again when it dies; a tiny WSGI app with a dependency from PyPI answers on
// the Python site. Responses are checked through nginx.
func (h *harness) managedApps(t *testing.T) {
	get := func(dom, path string) (int, string) {
		r := h.run("curl", "-sk", "-m", "15", "-w", "\n%{http_code}", "--resolve", dom+":443:127.0.0.1", "https://"+dom+path)
		out := strings.TrimRight(r.Out, "\n")
		i := strings.LastIndexByte(out, '\n')
		if i < 0 {
			n, _ := strconv.Atoi(out)
			return n, ""
		}
		n, _ := strconv.Atoi(out[i+1:])
		return n, out[:i]
	}
	// waitBody polls until the site answers 200 with want in the body.
	waitBody := func(dom, want string) string {
		var body string
		for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			code, b := get(dom, "/")
			body = fmt.Sprintf("%d %s", code, b)
			if code == 200 && strings.Contains(b, want) {
				return ""
			}
		}
		return body
	}
	// deploy builds a release from dir and promotes it.
	deploy := func(id int64, dir string) (string, string) {
		_, b := h.api.post(fmt.Sprintf("/api/sites/%d/deployments", id), map[string]string{"source_dir": dir})
		rid := jsonField(b, "release_id")
		if st := h.api.waitTask(taskID(b), 10*time.Minute); st != "succeeded" {
			return rid, "build " + st + ": " + string(head(b, 300))
		}
		var deps []struct {
			ID        int64  `json:"id"`
			ReleaseID string `json:"release_id"`
		}
		_, lb := h.api.get(fmt.Sprintf("/api/sites/%d/deployments", id))
		_ = json.Unmarshal(lb, &deps)
		for _, d := range deps {
			if d.ReleaseID == rid {
				_, pb := h.api.post(fmt.Sprintf("/api/deployments/%d/promote", d.ID), `{}`)
				if st := h.api.waitTask(taskID(pb), 3*time.Minute); st != "succeeded" {
					return rid, "promote " + st
				}
				return rid, ""
			}
		}
		return rid, "no deployment for " + rid
	}
	create := func(typ, dom string) (int64, bool) {
		id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":%q}`, dom, typ))
		ok := h.api.waitActive(id, 15*time.Minute) == "active"
		h.assert(t, "#28: the "+typ+" site provisions with its runtime", ok, "%s (id %d) did not become active", dom, id)
		return id, ok
	}

	// ---- Node.js ----
	ndom := h.dom("nodeapp")
	nid, ok := create("node", ndom)
	if ok {
		func() {
			defer func() {
				h.api.deleteSite(nid)
				h.api.teardownGone(nid)
			}()
			user := h.api.site(nid).SystemUser
			unit := "cloudground-app-" + user + ".service"
			h.check(t, "#28: the starter app answers through nginx", waitBody(ndom, "It runs Node.js v22"), "")
			show := func(prop string) string {
				return strings.TrimSpace(h.run("systemctl", "show", "--value", "-p", prop, unit).Out)
			}
			h.check(t, "#28: the app runs as the site user", show("User"), user)
			h.check(t, "#28: in the site's slice", show("Slice"), user+".slice")
			h.check(t, "#28: in the site's sandbox", show("ProtectSystem"), "strict")
			h.check(t, "#28: with no start limit, so restarts in a row never leave it failed", show("StartLimitIntervalUSec"), "0")
			pid := show("MainPID")
			h.check(t, "#28: the app process is the site user's", strings.TrimSpace(h.run("ps", "-o", "uid=", "-p", pid).Out), strings.TrimSpace(h.run("id", "-u", user).Out))
			sock := "/run/cloudground/app/" + user + "/app.sock"
			h.check(t, "#28: nginx reaches it on a unix socket of the site's group only", strings.TrimSpace(h.run("stat", "-c", "%A", sock).Out), "srwxrwx---")

			root := "/srv/sites/" + ndom
			release := func(n int) string {
				dir := fmt.Sprintf("%s/shared/cg-e2e-app-v%d", root, n)
				h.as(user, "mkdir", "-p", dir)
				h.mustWriteAs(t, dir+"/package.json", `{"name":"cg-e2e","version":"1.0.0","private":true}`+"\n", user)
				h.mustWriteAs(t, dir+"/server.js", fmt.Sprintf(`const http = require("http");
http.createServer((req, res) => {
  res.end("cg28-node-v%d greeting=" + (process.env.GREETING || "") + " token=" + (process.env.API_TOKEN ? process.env.API_TOKEN.length : 0) + "\n");
}).listen(process.env.PORT);
`, n), user)
				return dir
			}
			defer h.run("rm", "-rf", root+"/shared/cg-e2e-app-v1", root+"/shared/cg-e2e-app-v2")
			r1, err := deploy(nid, release(1))
			h.check(t, "#28: a Node app deploys (npm install as the site user) and is promoted", err, "")
			h.assert(t, "#28: the release has its dependencies installed inside it", h.run("test", "-e", root+"/releases/"+r1+"/package.json").OK(), "%s", r1)
			h.check(t, "#28: the new release answers", waitBody(ndom, "cg28-node-v1"), "")
			_, err = deploy(nid, release(2))
			h.check(t, "#28: a second release deploys", err, "")
			h.check(t, "#28: the second release answers", waitBody(ndom, "cg28-node-v2"), "")
			_, b := h.api.post(fmt.Sprintf("/api/sites/%d/rollback", nid), `{}`)
			h.check(t, "#28: a rollback is accepted", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
			h.check(t, "#28: after the rollback the first release answers again", waitBody(ndom, "cg28-node-v1"), "")

			// The environment, a plain variable and a sealed secret.
			c, ab := h.api.put(fmt.Sprintf("/api/sites/%d/app", nid), map[string]any{
				"env": map[string]string{"GREETING": "ciao"}, "secrets": map[string]string{"API_TOKEN": "cg28-secret-value"},
			})
			h.check(t, "#28: variables and a secret are saved", fmt.Sprint(c), "200")
			if c != 200 {
				t.Logf("PUT app: %s", head(ab, 300))
			}
			h.assert(t, "#28: the answer never shows the secret", !strings.Contains(string(ab), "cg28-secret-value"), "%s", head(ab, 300))
			h.check(t, "#28: the app gets the variable and the secret", waitBody(ndom, "greeting=ciao token=17"), "")
			envFile := "/etc/cloudground-app/" + user + ".env"
			h.check(t, "#28: the environment file is root's alone", strings.TrimSpace(h.run("stat", "-c", "%U %a", envFile).Out), "root 600")
			unitText := h.run("cat", "/etc/systemd/system/"+unit).Out
			h.assert(t, "#28: the unit holds no secret", unitText != "" && !strings.Contains(unitText, "cg28-secret-value"), "%s", head([]byte(unitText), 400))

			// F-6-2: in TCP-port mode the app binds its own port through the
			// sandbox's SocketBindDeny, and nginx reaches it there; then back
			// to the socket.
			const appPort = "19281"
			// The installer leaves the ports below 32768 to services; one the
			// kernel hands out itself is refused.
			h.check(t, "F-6-2: the kernel hands out ports from 32768 only", strings.Join(strings.Fields(h.run("cat", "/proc/sys/net/ipv4/ip_local_port_range").Out), " "), "32768 65535")
			c, ab = h.api.put(fmt.Sprintf("/api/sites/%d/app", nid), map[string]any{"port": 40000})
			h.assert(t, "F-6-2: a port the kernel hands out is refused", c == 400 && strings.Contains(string(ab), "choose a port from 1024 to 32767"), "%d %s", c, head(ab, 300))
			c, ab = h.api.put(fmt.Sprintf("/api/sites/%d/app", nid), map[string]any{"port": 19281})
			h.check(t, "F-6-2: the app moves to a TCP port", fmt.Sprint(c), "200")
			if c != 200 {
				t.Logf("PUT app port: %s", head(ab, 300))
			}
			h.check(t, "F-6-2: the app on its own port answers through nginx", waitBody(ndom, "cg28-node-v1"), "")
			h.assert(t, "F-6-2: the unit allows exactly its own port",
				strings.Contains(h.run("cat", "/etc/systemd/system/"+unit).Out, "\nSocketBindAllow=tcp:"+appPort+"\n"), "%s", unit)
			h.assert(t, "F-6-2: the app listens on its port", strings.TrimSpace(h.run("ss", "-ltnH", "sport = :"+appPort).Out) != "", "nothing on %s", appPort)
			c, _ = h.api.put(fmt.Sprintf("/api/sites/%d/app", nid), map[string]any{"port": 0})
			h.check(t, "F-6-2: the app moves back to its socket", fmt.Sprint(c), "200")
			h.check(t, "F-6-2: the app on its socket answers again", waitBody(ndom, "cg28-node-v1"), "")

			// It dies: systemd starts it again.
			before := show("NRestarts")
			// A unit that is not running has MainPID 0, and kill -9 0 would
			// kill the e2e's own process group: fail the check instead.
			mainPID, _ := strconv.Atoi(show("MainPID"))
			h.assert(t, "#28: the app has a main process to kill", mainPID > 0, "MainPID %d", mainPID)
			if mainPID > 0 {
				h.run("kill", "-9", strconv.Itoa(mainPID))
			}
			h.check(t, "#28: a killed app is started again", waitBody(ndom, "cg28-node-v1"), "")
			// systemd starts it again (Restart=always); a reconcile of the
			// agent running meanwhile may start it first, which NRestarts
			// does not count: the new process is what proves it came back.
			h.check(t, "#28: systemd restarts the app whenever it exits", show("Restart"), "always")
			newPID, _ := strconv.Atoi(show("MainPID"))
			h.assert(t, "#28: the app runs again in a new process", newPID > 0 && newPID != mainPID, "MainPID %d → %d (NRestarts %s → %s)", mainPID, newPID, before, show("NRestarts"))
			c, sb := h.api.get(fmt.Sprintf("/api/sites/%d/app/status", nid))
			h.assert(t, "#28: the panel shows the app healthy", c == 200 && strings.Contains(string(sb), `"active":true`) && strings.Contains(string(sb), `"status":200`), "%d %s", c, head(sb, 300))
			h.assert(t, "#28: the app's output is in the site's app log", h.run("test", "-f", "/var/log/cloudground/"+ndom+"/app.log").OK(), "no app.log")
		}()
		h.assert(t, "#28: the app's unit goes with the site", !h.run("test", "-e", "/etc/systemd/system/cloudground-app-cg-site-"+strconv.FormatInt(nid, 10)+".service").OK(), "unit left")
	}

	// ---- Python ----
	pdom := h.dom("pyapp")
	pid, ok := create("python", pdom)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(pid)
		h.api.teardownGone(pid)
	}()
	user := h.api.site(pid).SystemUser
	h.check(t, "#28: the Python starter answers through gunicorn and nginx", waitBody(pdom, "It runs Python 3."), "")
	root := "/srv/sites/" + pdom
	dir := root + "/shared/cg-e2e-wsgi"
	defer h.run("rm", "-rf", dir)
	h.as(user, "mkdir", "-p", dir)
	h.mustWriteAs(t, dir+"/requirements.txt", "gunicorn\n", user)
	h.mustWriteAs(t, dir+"/app.py", `def app(environ, start_response):
    body = ("cg28-wsgi path=" + environ.get("PATH_INFO", "") + "\n").encode()
    start_response("200 OK", [("Content-Type", "text/plain"), ("Content-Length", str(len(body)))])
    return [body]
`, user)
	rid, err := deploy(pid, dir)
	h.check(t, "#28: a WSGI app deploys (a venv and pip install as the site user)", err, "")
	h.assert(t, "#28: the release has its own virtual environment", h.run("test", "-x", root+"/releases/"+rid+"/.venv/bin/python").OK(), "%s", rid)
	h.check(t, "#28: the WSGI app answers through nginx", waitBody(pdom, "cg28-wsgi path=/"), "")
	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/rollback", pid), `{}`)
	h.check(t, "#28: the Python site rolls back", h.api.waitTask(taskID(b), 3*time.Minute), "succeeded")
	h.check(t, "#28: the starter answers again after the rollback", waitBody(pdom, "It runs Python 3."), "")

	// #213: moved to a TCP port, the default start follows (gunicorn binds
	// 127.0.0.1:$PORT) and nginx still gets an answer, not a 502.
	code, ab := h.api.put(fmt.Sprintf("/api/sites/%d/app", pid), map[string]any{"port": 8000})
	h.assert(t, "#213: a Python site on its default start moves to port 8000", code == 200 && jsonField(ab, "start") != "" && strings.Contains(jsonField(ab, "start"), "127.0.0.1:$PORT"), "%d %s", code, head(ab, 300))
	h.check(t, "#213: the Python site answers 200 through nginx on a TCP port", waitBody(pdom, "It runs Python 3."), "")
	h.assert(t, "#213: gunicorn listens on 127.0.0.1:8000", strings.Contains(h.run("ss", "-ltnH", "sport = :8000").Out, "127.0.0.1:8000"), "not listening")
}
