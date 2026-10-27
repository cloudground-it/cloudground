//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// fnView is the part of a function's view (api SPEC §17.1) the checks read.
type fnView struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Release  string   `json:"release"`
	Releases []string `json:"releases"`
	URLs     []string `json:"urls"`
	Token    string   `json:"token"`
	Task     struct {
		ID int64 `json:"id"`
	} `json:"task"`
}

func decodeFn(b []byte) fnView {
	var v fnView
	_ = json.Unmarshal(b, &v)
	return v
}

// functionsSection (#143 v0.9.0, internal/functions SPEC, internal/engine
// SPEC §8, api SPEC §17): standalone functions. A function with no domain
// answers a test run and its schedule but nobody outside; the functions
// domain answers it at /<name> with the path and the query, an own domain at
// / with the whole path; a token is checked by nginx and a new one replaces
// the old; CORS headers come from its origins; a package.json is installed
// per release and a rollback brings the old dependency back; systemd's
// limits and nginx's bounds hold; it runs as its own user, its code is no
// site's to read and nothing runs between requests; a site's domain is
// refused; a deletion removes everything; and a v0.8.x site function,
// written into the state before a start, is moved out of its site with its
// code and secrets while the site stops answering /.functions/.
func (h *harness) functionsSection(t *testing.T) {
	// Node.js 22 (installed once; the managed apps section may have done it).
	c, b := h.api.post("/api/runtimes/install", map[string]string{"runtime": "node", "version": "22"})
	if c == 202 {
		h.check(t, "#143: Node.js 22 is installed", h.api.waitTask(taskID(b), 15*time.Minute), "succeeded")
	}
	fnDomain, ownDomain := h.dom("fn"), h.dom("api-fn")
	// A site: its user stands for any site's, and it is the v0.8.x site of
	// the migration below.
	siteDom := h.dom("fnsite")
	sid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, siteDom))
	ok := h.api.waitActive(sid, 4*time.Minute) == "active"
	h.assert(t, "#143: the site provisions", ok, "%s (id %d) did not become active", siteDom, sid)
	if !ok {
		return
	}
	siteUser := h.api.site(sid).SystemUser
	created := []string{}
	defer func() {
		for _, n := range created {
			h.api.del("/api/functions/" + n)
		}
		h.api.put("/api/functions/domain", map[string]string{"domain": ""})
		h.api.deleteSite(sid)
		h.api.teardownGone(sid)
	}()

	call := func(host, path string, hdr ...string) (int, string, time.Duration) {
		args := []string{"-sk", "-m", "30", "-w", "\n%{http_code}", "--resolve", host + ":443:127.0.0.1"}
		for _, x := range hdr {
			args = append(args, "-H", x)
		}
		start := time.Now()
		r := h.run("curl", append(args, "https://"+host+path)...)
		out := strings.TrimRight(r.Out, "\n")
		i := strings.LastIndexByte(out, '\n')
		if i < 0 {
			n, _ := strconv.Atoi(out)
			return n, "", time.Since(start)
		}
		n, _ := strconv.Atoi(out[i+1:])
		return n, out[:i], time.Since(start)
	}
	create := func(body map[string]any) (int, fnView, []byte) {
		c, b := h.api.post("/api/functions", body)
		if c == 201 {
			created = append(created, body["name"].(string))
		}
		return c, decodeFn(b), b
	}
	running := func(id int64) string {
		return strings.TrimSpace(h.run("systemctl", "list-units", "--no-legend", "--state=active", fmt.Sprintf("cloudground-fn-%d@*", id)).Out)
	}
	code := func(marker string) string {
		return fmt.Sprintf(`exports.handler = async (event) => ({ statusCode: 200, headers: {"Content-Type": "application/json"},
  body: { marker: %q, path: event.path, query: event.query, source: event.source, key: (process.env.API_KEY || "").length, uid: process.getuid(), pid: process.pid } });
`, marker)
	}

	// No domain: a test run answers, nobody outside does.
	c, hello, b := create(map[string]any{"name": "hello", "runtime": "node", "version": "22", "files": map[string]string{"index.js": code("cg143-v1")},
		"secrets": map[string]string{"API_KEY": "cg143-secret"}})
	h.check(t, "#143: a function is created", fmt.Sprint(c), "201")
	if c != 201 {
		return
	}
	h.assert(t, "#143: the answer never shows the secret", !strings.Contains(string(b), "cg143-secret"), "%s", head(b, 300))
	h.assert(t, "#143: with no domain the function has no public address", len(hello.URLs) == 0, "%v", hello.URLs)
	_, ib := h.api.post("/api/functions/hello/invoke", map[string]any{"method": "GET", "path": "/t?q=1"})
	h.assert(t, "#143: a test run answers without any domain, with the path, the query and the secret",
		strings.Contains(string(ib), `"status":200`) && strings.Contains(string(ib), `\"path\":\"/t\"`) && strings.Contains(string(ib), `\"query\":\"q=1\"`) && strings.Contains(string(ib), `\"key\":12`),
		"%s", head(ib, 400))
	st, _, _ := call(fnDomain, "/hello")
	h.check(t, "#143: an unknown host on 443 is closed (444): the function is not reachable from outside", fmt.Sprint(st), "0")

	// Its own user, its own tree, nothing left running.
	uid := strings.TrimSpace(h.run("id", "-u", fmt.Sprintf("cg-fn-%d", hello.ID)).Out)
	h.assert(t, "#143: the function's user exists", uid != "", "no user cg-fn-%d", hello.ID)
	h.assert(t, "#143: the function runs as its own user", strings.Contains(string(ib), `\"uid\":`+uid), "uid %s: %s", uid, head(ib, 300))
	h.check(t, "#143: /srv/functions lists nothing to others", strings.TrimSpace(h.run("stat", "-c", "%U %a", "/srv/functions").Out), "root 711")
	h.check(t, "#143: a function's tree is root's with its group, 0750",
		strings.TrimSpace(h.run("stat", "-c", "%U %G %a", "/srv/functions/hello").Out), fmt.Sprintf("root cg-fn-%d 750", hello.ID))
	r := h.run("runuser", "-u", siteUser, "--", "cat", "/srv/functions/hello/current/index.js")
	h.assert(t, "#143: a site's user cannot read the function's code", !r.OK() && !strings.Contains(r.Out, "cg143"), "%d %s", r.Code, r.Out)
	sock := strings.TrimSpace(h.run("stat", "-c", "%U %G %a", fmt.Sprintf("/run/cloudground/fn/%d.sock", hello.ID)).Out)
	h.check(t, "#143: the socket is nginx's and root's alone", sock, "root www-data 660")
	var left string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if left = running(hello.ID); left == "" {
			break
		}
	}
	h.check(t, "#143: nothing of the function runs between invocations", left, "")
	unit := h.run("cat", fmt.Sprintf("/etc/systemd/system/cloudground-fn-%d@.service", hello.ID)).Out
	h.assert(t, "#143: the unit holds the limits, its user, its read-only code and no secret",
		strings.Contains(unit, "RuntimeMaxSec=10") && strings.Contains(unit, "MemoryMax=128M") && strings.Contains(unit, fmt.Sprintf("User=cg-fn-%d", hello.ID)) &&
			strings.Contains(unit, "BindReadOnlyPaths=/srv/functions/hello") && !strings.Contains(unit, "cg143-secret"), "%s", head([]byte(unit), 900))
	h.check(t, "#143: the environment file is root's 0600", strings.TrimSpace(h.run("stat", "-c", "%U %a", fmt.Sprintf("/etc/cloudground-fn/%d.env", hello.ID)).Out), "root 600")

	// The functions domain: /<name> with the path and the query.
	c, b = h.api.put("/api/functions/domain", map[string]string{"domain": fnDomain})
	h.check(t, "#143: the functions domain is saved", fmt.Sprint(c), "200")
	if task := taskID(b); task != 0 {
		h.check(t, "#143: the functions domain's certificate is issued", h.api.waitTask(task, 5*time.Minute), "succeeded")
		_, sb := h.api.get("/api/functions/domain/status")
		h.assert(t, "#143: the functions domain has its certificate", strings.Contains(string(sb), `"exists":true`), "%s", head(sb, 400))
		vr := h.run("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--resolve", fnDomain+":443:127.0.0.1", "https://"+fnDomain+"/hello")
		h.check(t, "#143: and it verifies (no -k)", vr.Trim(), "200")
	} else {
		t.Logf("#143: no ACME e-mail on this server: the functions domain serves the fallback certificate")
	}
	st, body, _ := call(fnDomain, "/hello/sub/x?y=2")
	h.assert(t, "#143: the functions domain answers /<name> with the path after it and the query",
		st == 200 && strings.Contains(body, `"marker":"cg143-v1"`) && strings.Contains(body, `"path":"/sub/x"`) && strings.Contains(body, `"query":"y=2"`) && strings.Contains(body, `"source":"http"`),
		"%d %s", st, body)
	_, body2, _ := call(fnDomain, "/hello")
	h.assert(t, "#143: each invocation is a process of its own", body != "" && body2 != "" && jsonField([]byte(body), "pid") != jsonField([]byte(body2), "pid"), "%s / %s", body, body2)
	st, body, _ = call(fnDomain, "/nothing-here")
	h.assert(t, "#143: a path naming no function is a JSON 404", st == 404 && strings.Contains(body, "no such function"), "%d %s", st, body)

	// An own domain: / with the whole path.
	c, b = h.api.put("/api/functions/hello/domains", map[string]any{"domains": []string{ownDomain}})
	h.check(t, "#143: an own domain is saved", fmt.Sprint(c), "200")
	if task := taskID(b); task != 0 {
		h.check(t, "#143: the own domain's certificate is issued", h.api.waitTask(task, 5*time.Minute), "succeeded")
	}
	st, body, _ = call(ownDomain, "/a/b?c=3")
	h.assert(t, "#143: the own domain answers at / with the whole path",
		st == 200 && strings.Contains(body, `"path":"/a/b"`) && strings.Contains(body, `"query":"c=3"`), "%d %s", st, body)
	c, _ = h.api.put("/api/functions/hello/domains", map[string]any{"domains": []string{siteDom}})
	h.check(t, "#143: a site's domain is refused", fmt.Sprint(c), "409")

	// CORS.
	c, _ = h.api.put("/api/functions/hello", map[string]any{"cors_origins": []string{"https://app.example.com"}})
	h.check(t, "#143: origins are saved", fmt.Sprint(c), "200")
	hdr := h.run("curl", "-sk", "-D", "-", "-o", "/dev/null", "-H", "Origin: https://app.example.com", "--resolve", fnDomain+":443:127.0.0.1", "https://"+fnDomain+"/hello").Out
	h.assert(t, "#143: an allowed origin gets its CORS header", strings.Contains(strings.ToLower(hdr), "access-control-allow-origin: https://app.example.com"), "%s", hdr)
	hdr = h.run("curl", "-sk", "-D", "-", "-o", "/dev/null", "-H", "Origin: https://evil.example", "--resolve", fnDomain+":443:127.0.0.1", "https://"+fnDomain+"/hello").Out
	h.assert(t, "#143: another origin gets none", !strings.Contains(strings.ToLower(hdr), "access-control-allow-origin"), "%s", hdr)
	pre := h.run("curl", "-sk", "-X", "OPTIONS", "-o", "/dev/null", "-w", "%{http_code}", "-H", "Origin: https://app.example.com", "-H", "Access-Control-Request-Method: POST",
		"--resolve", fnDomain+":443:127.0.0.1", "https://"+fnDomain+"/hello").Out
	h.check(t, "#143: a preflight is answered by nginx", strings.TrimSpace(pre), "204")

	// A token: checked by nginx, replaced by a new one.
	c, sec, _ := create(map[string]any{"name": "secure", "runtime": "node", "auth": "token"})
	h.check(t, "#143: a function behind a token is created", fmt.Sprint(c), "201")
	st, _, _ = call(fnDomain, "/secure")
	h.check(t, "#143: without the token: 401", fmt.Sprint(st), "401")
	st, _, _ = call(fnDomain, "/secure", "Authorization: Bearer "+sec.Token)
	h.check(t, "#143: with the token: 200", fmt.Sprint(st), "200")
	_, tb := h.api.post("/api/functions/secure/token", map[string]any{})
	newTok := jsonField(tb, "token")
	st, _, _ = call(fnDomain, "/secure", "Authorization: Bearer "+sec.Token)
	h.check(t, "#143: the old token after a new one: 401", fmt.Sprint(st), "401")
	st, _, _ = call(fnDomain, "/secure", "Authorization: Bearer "+newTok)
	h.check(t, "#143: the new token: 200", fmt.Sprint(st), "200")
	conf := strings.TrimSpace(h.run("stat", "-c", "%U %a", "/etc/nginx/sites-enabled/cloudground-functions.conf").Out)
	h.check(t, "#143: the nginx file holding the token is root's 0600", conf, "root 600")

	// Dependencies per release; a rollback brings the old one back.
	deps := func(v string) map[string]string {
		return map[string]string{
			"index.js":     `exports.handler = async () => ({ ms: require("ms/package.json").version, oneMinute: require("ms")("1m") });` + "\n",
			"package.json": fmt.Sprintf(`{"name":"cg143-deps","private":true,"dependencies":{"ms":%q}}`, v),
		}
	}
	c, _, db := create(map[string]any{"name": "deps", "runtime": "node", "files": deps("2.1.2")})
	h.check(t, "#143: a function with a package.json is created (its dependency installed)", fmt.Sprint(c), "201")
	if c != 201 {
		t.Logf("deps: %s", head(db, 600))
	}
	_, body, _ = call(fnDomain, "/deps")
	h.assert(t, "#143: the dependency answers", strings.Contains(body, `"ms":"2.1.2"`) && strings.Contains(body, `"oneMinute":60000`), "%s", body)
	c, _ = h.api.put("/api/functions/deps", map[string]any{"files": deps("2.1.3")})
	h.check(t, "#143: a new release with another version deploys", fmt.Sprint(c), "200")
	_, body, _ = call(fnDomain, "/deps")
	h.assert(t, "#143: the new release has its own dependency", strings.Contains(body, `"ms":"2.1.3"`), "%s", body)
	c, _ = h.api.post("/api/functions/deps/rollback", map[string]any{})
	h.check(t, "#143: a rollback is accepted", fmt.Sprint(c), "200")
	_, body, _ = call(fnDomain, "/deps")
	h.assert(t, "#143: after the rollback the old dependency answers", strings.Contains(body, `"ms":"2.1.2"`), "%s", body)

	// systemd's limits and nginx's bounds.
	c, spin, _ := create(map[string]any{"name": "spin", "runtime": "node", "timeout_sec": 2, "files": map[string]string{"index.js": "exports.handler = () => { for (;;) {} };\n"}})
	h.check(t, "#143: a function that never ends is created", fmt.Sprint(c), "201")
	st, _, took := call(fnDomain, "/spin")
	h.assert(t, "#143: a busy loop is killed at the timeout", st >= 500 && took < 10*time.Second, "%d after %s", st, took)
	c, hog, _ := create(map[string]any{"name": "hog", "runtime": "node", "memory_mb": 64, "files": map[string]string{"index.js": "exports.handler = () => { const a = []; for (;;) a.push(Buffer.alloc(16 << 20, 1)); };\n"}})
	h.check(t, "#143: a function that eats memory is created", fmt.Sprint(c), "201")
	st, _, _ = call(fnDomain, "/hog")
	h.assert(t, "#143: an invocation past its memory is killed", st >= 500, "%d", st)
	time.Sleep(2 * time.Second)
	h.check(t, "#143: nothing is left running after the limits", running(spin.ID)+running(hog.ID), "")
	c, slow, _ := create(map[string]any{"name": "slow", "runtime": "node", "files": map[string]string{"index.js": "exports.handler = () => new Promise(r => setTimeout(() => r('slow done'), 3000));\n"}})
	h.check(t, "#143: a slow function is created", fmt.Sprint(c), "201")
	var peak int
	var mu sync.Mutex
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			n := 0
			if out := running(slow.ID); out != "" {
				n = len(strings.Split(out, "\n"))
			}
			mu.Lock()
			peak = max(peak, n)
			mu.Unlock()
		}
	}()
	burst := h.run("sh", "-c", fmt.Sprintf(`seq 80 | xargs -P 80 -I{} curl -sk -m 30 -o /dev/null -w '%%{http_code}\n' --resolve %s:443:127.0.0.1 https://%s/slow`, fnDomain, fnDomain)).Out
	close(stop)
	<-sampled
	codes := map[string]int{}
	for _, l := range strings.Fields(burst) {
		codes[l]++
	}
	h.assert(t, "#143: a burst of 80 gets only 200, 429 and 503, some refused", codes["429"]+codes["503"] > 0 && codes["429"]+codes["503"]+codes["200"] == 80, "codes %v", codes)
	h.assert(t, "#143: at most 8 processes at once for one client", peak <= 8, "peak %d processes", peak)

	// The schedule runs it on its own.
	c, _ = h.api.put("/api/functions/hello", map[string]any{"schedule": "* * * * *"})
	h.check(t, "#143: a schedule is saved", fmt.Sprint(c), "200")
	var sched bool
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline) && !sched; time.Sleep(10 * time.Second) {
		_, lb := h.api.get("/api/functions/hello/logs")
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
	h.api.put("/api/functions/hello", map[string]any{"schedule": ""})

	// A deletion removes everything.
	c, _ = h.api.del("/api/functions/hello")
	h.check(t, "#143: the function is deleted", fmt.Sprint(c), "204")
	created = created[1:]
	st, body, _ = call(fnDomain, "/hello")
	h.assert(t, "#143: a deleted function no longer answers", st == 404, "%d %s", st, body)
	st, _, _ = call(ownDomain, "/")
	h.check(t, "#143: nor at its own domain (closed, 444)", fmt.Sprint(st), "0")
	for _, p := range []string{
		fmt.Sprintf("/etc/systemd/system/cloudground-fn-%d.socket", hello.ID), fmt.Sprintf("/etc/systemd/system/cloudground-fn-%d@.service", hello.ID),
		fmt.Sprintf("/etc/cloudground-fn/%d.env", hello.ID), "/srv/functions/hello", fmt.Sprintf("/etc/nginx/sites-enabled/cloudground-fn-%d.conf", hello.ID),
		fmt.Sprintf("/etc/letsencrypt/live/cloudground-fn-%d", hello.ID), "/var/log/cloudground/functions/hello.access.log",
	} {
		h.assert(t, "#143: deleted with the function: "+p, !h.run("test", "-e", p).OK(), "%s is still there", p)
	}
	h.assert(t, "#143: its user is gone", !h.run("id", fmt.Sprintf("cg-fn-%d", hello.ID)).OK(), "cg-fn-%d still exists", hello.ID)

	h.functionsMigration(t, sid, siteDom, siteUser, call)
}

// functionsMigration (api SPEC §17.8): a v0.8.x site function, written into
// the state and the site's tree while the API is stopped, is moved out of
// its site at the next start, with its code and its secret, and the site no
// longer answers /.functions/.
func (h *harness) functionsMigration(t *testing.T, sid int64, siteDom, siteUser string, call func(host, path string, hdr ...string) (int, string, time.Duration)) {
	site := h.api.site(sid)
	root := filepath.Join("/srv/sites", siteDom)
	rel := "01JC0000000000000000000143"
	dir := filepath.Join(root, "functions", "legacy", "releases", rel)
	code := `exports.handler = async () => ({ marker: "cg143-legacy", key: process.env.LEGACY_KEY || "" });` + "\n"
	ok := h.run("mkdir", "-p", dir).OK() &&
		os.WriteFile(filepath.Join(dir, "index.js"), []byte(code), 0o640) == nil &&
		h.run("ln", "-sfn", "releases/"+rel, filepath.Join(root, "functions", "legacy", "current")).OK() &&
		h.run("gnuchown", "-R", siteUser+":"+siteUser, filepath.Join(root, "functions")).OK()
	h.assert(t, "#143 migration: control: a v0.8.x function's code is in the site's tree", ok, "could not write %s", dir)
	_ = site

	// The API stopped, the function is written into the site's settings,
	// its secret sealed, and the migration's marker cleared.
	h.check(t, "#143 migration: control: the API stops", fmt.Sprint(h.run("systemctl", "stop", "cloudground-api.service").Code), "0")
	dbPath := "/var/lib/cloudground/state.db"
	owners := map[string][2]int{}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			st := fi.Sys().(*syscall.Stat_t)
			owners[p] = [2]int{int(st.Uid), int(st.Gid)}
		}
	}
	err := func() error {
		s, err := state.Open(dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		if _, err := s.MutateSite(sid, func(x *state.Site) error {
			x.Config.Functions = []state.SiteFunction{{Name: "legacy", Runtime: "node", Version: "22", MemoryMB: 128, TimeoutSec: 10, Secrets: []string{"LEGACY_KEY"}, Release: rel}}
			return nil
		}); err != nil {
			return err
		}
		if err := s.SetSecret(state.SiteSecret(sid, "fn.legacy.LEGACY_KEY"), "cg143-kept"); err != nil {
			return err
		}
		_, err = s.UpdatePanelSettings(func(p *state.PanelSettings) error { p.FunctionsMigration = ""; return nil })
		return err
	}()
	uid, gid := 0, 0
	if o, ok := owners[dbPath]; ok {
		uid, gid = o[0], o[1]
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Chown(p, uid, gid)
	}
	h.assert(t, "#143 migration: control: the fixture is written into the state", err == nil, "%v", err)
	h.check(t, "#143 migration: control: the API starts", fmt.Sprint(h.run("systemctl", "start", "cloudground-api.service").Code), "0")
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if c, _ := h.api.get("/api/session"); c == 200 {
			break
		}
	}
	h.api.relogin()
	defer h.api.del("/api/functions/legacy")

	var fv fnView
	for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(2 * time.Second) {
		if c, b := h.api.get("/api/functions/legacy"); c == 200 {
			if fv = decodeFn(b); fv.Release != "" {
				break
			}
		}
	}
	h.check(t, "#143 migration: the site's function is a standalone one, with its release", fv.Release, rel)
	_, ib := h.api.post("/api/functions/legacy/invoke", map[string]any{})
	h.assert(t, "#143 migration: its code and its secret are kept", strings.Contains(string(ib), "cg143-legacy") && strings.Contains(string(ib), "cg143-kept"), "%s", head(ib, 300))
	st, body, _ := call(siteDom, "/.functions/legacy")
	h.assert(t, "#143 migration: the site no longer answers /.functions/", !strings.Contains(body, "cg143-legacy"), "%d %s", st, body)
	h.assert(t, "#143 migration: the old code left the site's tree", !h.run("test", "-e", filepath.Join(root, "functions", "legacy")).OK(), "still there")
	_, sb := h.api.get(fmt.Sprintf("/api/sites/%d", sid))
	h.assert(t, "#143 migration: the site's settings no longer hold it", !strings.Contains(string(sb), `"legacy"`), "%s", head(sb, 300))
}
