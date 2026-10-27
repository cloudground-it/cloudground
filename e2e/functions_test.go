//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// functionsSection (#143): a static site's functions answer at
// /.functions/<name> through nginx, one process per invocation that is
// gone afterwards; systemd kills an invocation past its timeout or its
// memory; a secret reaches the handler and never the unit; new code is a
// release that can be rolled back; a Python function answers; a schedule
// runs the function on its own; a deleted function no longer answers.
func (h *harness) functionsSection(t *testing.T) {
	// Node.js 22 for the functions (installed once; the managed apps section may have done it).
	c, b := h.api.post("/api/runtimes/install", map[string]string{"runtime": "node", "version": "22"})
	if c == 202 {
		h.check(t, "#143: Node.js 22 is installed", h.api.waitTask(taskID(b), 15*time.Minute), "succeeded")
	}
	dom := h.dom("functions")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#143: the site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	user := h.api.site(id).SystemUser
	base := fmt.Sprintf("/api/sites/%d/functions", id)
	call := func(path string) (int, string, time.Duration) {
		start := time.Now()
		r := h.run("curl", "-sk", "-m", "30", "-w", "\n%{http_code}", "--resolve", dom+":443:127.0.0.1", "https://"+dom+path)
		out := strings.TrimRight(r.Out, "\n")
		i := strings.LastIndexByte(out, '\n')
		if i < 0 {
			n, _ := strconv.Atoi(out)
			return n, "", time.Since(start)
		}
		n, _ := strconv.Atoi(out[i+1:])
		return n, out[:i], time.Since(start)
	}
	running := func(name string) string {
		return strings.TrimSpace(h.run("systemctl", "list-units", "--no-legend", "--state=active", "cloudground-fn-"+user+"-"+name+"@*").Out)
	}

	code := func(marker string) string {
		return fmt.Sprintf(`exports.handler = async (event) => ({ statusCode: 200, headers: {"Content-Type": "application/json"},
  body: { marker: %q, path: event.path, query: event.query, token: (process.env.API_TOKEN || "").length, pid: process.pid } });
`, marker)
	}
	c, b = h.api.post(base, map[string]any{"name": "hello", "runtime": "node", "version": "22", "code": code("cg143-v1"),
		"secrets": map[string]string{"API_TOKEN": "cg143-secret"}})
	h.check(t, "#143: a Node.js function is created", fmt.Sprint(c), "201")
	h.assert(t, "#143: the answer never shows the secret", !strings.Contains(string(b), "cg143-secret"), "%s", head(b, 300))
	st, body, _ := call("/.functions/hello/sub?x=1")
	h.assert(t, "#143: the function answers through nginx with the path, the query and the secret",
		st == 200 && strings.Contains(body, `"marker":"cg143-v1"`) && strings.Contains(body, `"path":"/sub"`) && strings.Contains(body, `"query":"x=1"`) && strings.Contains(body, `"token":12`),
		"%d %s", st, body)
	_, body2, _ := call("/.functions/hello")
	h.assert(t, "#143: each invocation is a process of its own", body != "" && body2 != "" && jsonField([]byte(body), "pid") != jsonField([]byte(body2), "pid"), "%s / %s", body, body2)
	var left string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if left = running("hello"); left == "" {
			break
		}
	}
	h.check(t, "#143: nothing of the function runs between invocations", left, "")
	unit := h.run("cat", "/etc/systemd/system/cloudground-fn-"+user+"-hello@.service").Out
	h.assert(t, "#143: the unit holds the limits and no secret", strings.Contains(unit, "RuntimeMaxSec=10") && strings.Contains(unit, "MemoryMax=128M") && !strings.Contains(unit, "cg143-secret"), "%s", head([]byte(unit), 600))

	// New code is a release; a rollback puts the old one back.
	c, _ = h.api.put(base+"/hello", map[string]any{"code": code("cg143-v2")})
	h.check(t, "#143: new code deploys", fmt.Sprint(c), "200")
	_, body, _ = call("/.functions/hello")
	h.assert(t, "#143: the new code answers", strings.Contains(body, "cg143-v2"), "%s", body)
	c, _ = h.api.post(base+"/hello/rollback", map[string]any{})
	h.check(t, "#143: a rollback is accepted", fmt.Sprint(c), "200")
	_, body, _ = call("/.functions/hello")
	h.assert(t, "#143: after the rollback the first code answers", strings.Contains(body, "cg143-v1"), "%s", body)

	// systemd's limits.
	c, _ = h.api.post(base, map[string]any{"name": "spin", "runtime": "node", "timeout_sec": 2, "code": "exports.handler = () => { for (;;) {} };\n"})
	h.check(t, "#143: a function that never ends is created", fmt.Sprint(c), "201")
	st, _, took := call("/.functions/spin")
	h.assert(t, "#143: a busy loop is killed at the timeout", st >= 500 && took < 10*time.Second, "%d after %s", st, took)
	c, _ = h.api.post(base, map[string]any{"name": "hog", "runtime": "node", "memory_mb": 64, "code": "exports.handler = () => { const a = []; for (;;) a.push(Buffer.alloc(16 << 20, 1)); };\n"})
	h.check(t, "#143: a function that eats memory is created", fmt.Sprint(c), "201")
	st, _, _ = call("/.functions/hog")
	h.assert(t, "#143: an invocation past its memory is killed", st >= 500, "%d", st)
	h.check(t, "#143: nothing is left running after the limits", running("spin")+running("hog"), "")

	// A burst over the limits gets 429 or 503, never a pile of processes
	// (internal/functions SPEC §3, internal/engine SPEC §4.13).
	c, _ = h.api.post(base, map[string]any{"name": "slow", "runtime": "node", "code": "exports.handler = () => new Promise(r => setTimeout(() => r('slow done'), 3000));\n"})
	h.check(t, "#143: a slow function is created", fmt.Sprint(c), "201")
	var peak int
	var mu sync.Mutex
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			n := 0
			if out := running("slow"); out != "" {
				n = len(strings.Split(out, "\n"))
			}
			mu.Lock()
			peak = max(peak, n)
			mu.Unlock()
		}
	}()
	burst := h.run("sh", "-c", fmt.Sprintf(`seq 80 | xargs -P 80 -I{} curl -sk -m 30 -o /dev/null -w '%%{http_code}\n' --resolve %s:443:127.0.0.1 https://%s/.functions/slow`, dom, dom)).Out
	close(stop)
	<-sampled
	codes := map[string]int{}
	for _, l := range strings.Fields(burst) {
		codes[l]++
	}
	h.assert(t, "#143: a burst of 80 is turned away with 429 or 503", codes["429"]+codes["503"] > 0 && codes["429"]+codes["503"]+codes["200"] == 80, "codes %v", codes)
	h.assert(t, "#143: the burst never gets 502 (no pile of processes refused by the socket)", codes["502"] == 0, "codes %v", codes)
	h.assert(t, "#143: at most 8 processes at once for one client", peak <= 8, "peak %d processes", peak)

	// A Python function, when the server has a Python with venv (the managed apps section installs it).
	c, _ = h.api.post(base, map[string]any{"name": "py", "runtime": "python", "version": "3.12",
		"code": "def handler(event):\n    return \"cg143-python \" + event[\"method\"]\n"})
	if c == 201 {
		st, body, _ = call("/.functions/py")
		h.assert(t, "#143: a Python function answers", st == 200 && strings.Contains(body, "cg143-python GET"), "%d %s", st, body)
	} else {
		t.Logf("python function: %d (no Python 3.12 venv on this server)", c)
	}

	// A schedule runs it on its own, through the same nginx.
	c, _ = h.api.put(base+"/hello", map[string]any{"schedule": "* * * * *"})
	h.check(t, "#143: a schedule is saved", fmt.Sprint(c), "200")
	var sched bool
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline) && !sched; time.Sleep(10 * time.Second) {
		_, lb := h.api.get(base + "/hello/logs")
		var logs struct {
			Invocations []struct {
				Source string `json:"source"`
				Status int    `json:"status"`
			} `json:"invocations"`
		}
		_ = json.Unmarshal(lb, &logs)
		for _, i := range logs.Invocations {
			sched = sched || (i.Source == "schedule" && i.Status == 200)
		}
	}
	h.assert(t, "#143: the schedule invoked the function", sched, "no scheduled invocation within 3 minutes")
	_, ib := h.api.post(base+"/hello/invoke", map[string]any{})
	h.assert(t, "#143: a test run answers", strings.Contains(string(ib), `"status":200`), "%s", head(ib, 300))
	// The agent invokes through nginx on a unix socket, in plain HTTP, in a
	// directory only root can enter.
	h.assert(t, "#143: the invocation socket is there", h.run("test", "-S", "/var/lib/cloudground-agent/nginx-functions.sock").OK(), "no socket")
	h.check(t, "#143: and only root can reach it", strings.TrimSpace(h.run("stat", "-c", "%U %a", "/var/lib/cloudground-agent").Out), "root 700")

	c, _ = h.api.do("DELETE", base+"/hello", nil)
	h.check(t, "#143: the function is deleted", fmt.Sprint(c), "204")
	st, body, _ = call("/.functions/hello")
	h.assert(t, "#143: a deleted function no longer answers", st != 200 || !strings.Contains(body, "cg143"), "%d %s", st, body)
	h.assert(t, "#143: its units are gone", !h.run("test", "-e", "/etc/systemd/system/cloudground-fn-"+user+"-hello.socket").OK(), "socket unit left")
}
