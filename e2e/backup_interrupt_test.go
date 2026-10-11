//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clearStandIns removes the tool stand-ins a run cut short may have left
// ahead of the real tools on the agent's PATH (/usr/local/sbin), before a
// section relies on the real ones. Each removal is logged.
func (h *harness) clearStandIns(t *testing.T, names ...string) {
	for _, n := range names {
		p := "/usr/local/sbin/" + n
		if _, err := os.Lstat(p); err == nil {
			t.Logf("(removing %s, a stand-in an earlier run left on the agent's PATH)", p)
			h.remove(p)
		}
	}
}

// backupInterruption is the R-4.6 section: restic stopped mid-backup
// removes its repository lock, files vanishing under a site's tmp/ do not
// fail its backup, and restic is a version with typed exit codes. It calls
// the agent on its socket with a repository of its own, so the panel's
// backup setting is never touched.
func (h *harness) backupInterruption(t *testing.T) {
	h.clearStandIns(t, "restic", "mariadb-dump")
	h.withFeature(t, func(f *feat) { f.backupInterruption(t) })
}

// resticVersion parses `restic version` ("restic 0.19.1 compiled with …")
// into its major and minor numbers.
func resticVersion(out string) (major, minor int, ok bool) {
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "restic" {
		return 0, 0, false
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	var err1, err2 error
	major, err1 = strconv.Atoi(parts[0])
	minor, err2 = strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

// agentRPC is one call to the agent's socket as root, the way the API makes
// it: the token and the body travel in root-only files, never in argv.
type agentRPC struct {
	h         *harness
	cfg, work string
}

func newAgentRPC(h *harness) (*agentRPC, func(), error) {
	work, err := os.MkdirTemp("", "cg-e2e-rpc-")
	if err != nil {
		return nil, nil, err
	}
	tok, err := os.ReadFile("/etc/cloudground/agent.token")
	if err != nil {
		os.RemoveAll(work)
		return nil, nil, err
	}
	cfg := filepath.Join(work, "curl.cfg")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("header = \"Authorization: Bearer %s\"\n", strings.TrimSpace(string(tok)))), 0o600); err != nil {
		os.RemoveAll(work)
		return nil, nil, err
	}
	return &agentRPC{h: h, cfg: cfg, work: work}, func() { os.RemoveAll(work) }, nil
}

func (a *agentRPC) args(method string, params any, maxTime int) []string {
	body, _ := json.Marshal(params)
	p := filepath.Join(a.work, method+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".json")
	_ = os.WriteFile(p, body, 0o600)
	return []string{"-sS", "--max-time", strconv.Itoa(maxTime), "--unix-socket", agentSock, "-K", a.cfg,
		"-X", "POST", "-H", "Content-Type: application/json", "--data-binary", "@" + p, "-w", "\n%{http_code}", "http://agent/rpc/" + method}
}

// call runs method to its end and returns the HTTP status and the body.
func (a *agentRPC) call(method string, params any, maxTime int) (string, string) {
	a.h.step("agent " + method)
	r := runEnv(nil, "curl", a.args(method, params, maxTime)...)
	out := strings.TrimSpace(r.Out)
	i := strings.LastIndex(out, "\n")
	if i < 0 {
		return out, r.Err
	}
	return out[i+1:], out[:i]
}

// start runs method in the background; stopping it cuts the connection.
func (a *agentRPC) start(method string, params any, maxTime int) *bg {
	a.h.step("agent " + method + " (in the background)")
	return execBackground("curl", a.args(method, params, maxTime)...)
}

func (f *feat) backupInterruption(t *testing.T) {
	h := f.h
	// The installer pins restic (0.19.1): 24.04's 0.16 has no typed exit
	// codes. What the agent runs is the first restic on its PATH.
	realRestic, lerr := exec.LookPath("restic")
	vout := h.run("restic", "version").Out
	major, minor, ok := resticVersion(vout)
	h.assert(t, "R-4.6: restic on the server has typed exit codes (0.17 or later)", ok && (major > 0 || minor >= 17),
		"restic version: %q (%s)", strings.TrimSpace(vout), realRestic)
	if lerr != nil {
		h.fail(t, "R-4.6 fixture: restic is on PATH", "R-4.6 fixture: %v", lerr)
		return
	}
	rpc, closeRPC, err := newAgentRPC(h)
	if err != nil {
		h.fail(t, "R-4.6 fixture: the agent's token", "R-4.6 fixture: %v", err)
		return
	}
	defer closeRPC()

	// The agent's Capabilities reports the restic it runs.
	code, body := rpc.call("Capabilities", struct{}{}, 30)
	var caps struct {
		ResticVersion   string `json:"restic_version"`
		ResticSupported bool   `json:"restic_supported"`
	}
	_ = json.Unmarshal([]byte(body), &caps)
	firstLine, _, _ := strings.Cut(strings.TrimSpace(vout), "\n")
	h.assert(t, "R-4.6: Capabilities reports restic's version, supported", code == "200" &&
		caps.ResticVersion == firstLine && caps.ResticSupported == (ok && (major > 0 || minor >= 17)),
		"status %s, %q supported %v; restic version says %q", code, caps.ResticVersion, caps.ResticSupported, firstLine)

	// A throwaway repository of the section's own.
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-int-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	removeRepo := h.undo("remove the R-4.6 repository", func() { _ = os.RemoveAll(brepo) })
	defer removeRepo()
	restic := h.resticOn(realRestic, brepo, bpw)
	// The repository's locks; -1 when restic cannot list them, which is
	// neither "a lock is held" nor "no lock is left".
	locks := func() int {
		r := restic("list", "locks", "--no-lock")
		if !r.OK() {
			t.Logf("restic list locks: %s", r)
			return -1
		}
		return CountNonEmptyLines(r.Out)
	}
	_, siteJSON := h.api.get(f.api(""))
	backup := func(kind string) map[string]any {
		return map[string]any{"site": json.RawMessage(siteJSON), "repository": brepo, "password": bpw, "kind": kind}
	}
	status, body := rpc.call("RunBackup", backup("files"), 600)
	h.check(t, "R-4.6 control: a first backup makes the repository", status, "200")
	if status != "200" {
		t.Logf("RunBackup: %s", head([]byte(body), 300))
		return
	}

	f.backupStoppedMidRun(t, rpc, realRestic, brepo, backup("files"), locks)

	// The next backup, and its restore test.
	status, body = rpc.call("RunBackup", backup("full"), 600)
	h.check(t, "R-4.6: the backup after a stopped one succeeds", status, "200")
	snap := jsonField([]byte(body), "snapshot_id")
	status, body = rpc.call("VerifyBackup", map[string]any{"site": json.RawMessage(siteJSON), "repository": brepo, "password": bpw, "snapshot_id": snap}, 900)
	h.assert(t, "R-4.6: the backup after a stopped one passes its restore test", status == "200" && strings.Contains(body, `"passed":true`),
		"VerifyBackup %s: %s", status, head([]byte(body), 300))

	f.backupWhileTmpChurns(t, rpc, backup("files"), restic)
}

// backupStoppedMidRun cuts a running backup's call to the agent and checks
// restic left no lock. Cutting the connection cancels the request's context,
// the path a job cancellation or a deadline takes to restic. A stand-in
// ahead of restic on the agent's PATH (/usr/local/sbin) execs the real one
// with --limit-upload, so the backup of a fresh 48 MiB file is still running
// when it is cut; the restic stopped is the real one, holding a real lock.
func (f *feat) backupStoppedMidRun(t *testing.T, rpc *agentRPC, realRestic, brepo string, params map[string]any, locks func() int) {
	h := f.h
	const standIn = "/usr/local/sbin/restic"
	bulk := filepath.Join("/srv/sites", f.fs, "shared/uploads/cg-e2e-r46-bulk.bin")
	var call *bg
	cleanup := h.undo("remove the R-4.6 restic stand-in and bulk file", func() {
		if call != nil {
			call.stop()
		}
		_ = os.Remove(standIn)
		h.run("pkill", "-f", "--", "--limit-upload 256")
		_ = os.Remove(bulk)
	})
	defer cleanup()

	data := make([]byte, 48<<20)
	_, _ = rand.Read(data)
	if err := h.writeAs(bulk, string(data), f.fsu, 0o644); err != nil {
		t.Logf("fixture: bulk file: %v", err)
	}
	h.step("install " + standIn)
	if err := os.WriteFile(standIn, []byte("#!/bin/sh\nexec "+realRestic+" --limit-upload 256 \"$@\"\n"), 0o755); err != nil {
		t.Logf("fixture: %v", err)
	}
	call = rpc.start("RunBackup", params, 120)

	held := false
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end) && !held; time.Sleep(500 * time.Millisecond) {
		held = locks() > 0
	}
	h.assert(t, "R-4.6 control: the backup holds a lock in the repository while it runs", held,
		"no lock appeared in %s while the backup ran (did it start?)", brepo)
	time.Sleep(3 * time.Second)
	running := h.run("pgrep", "-f", "--", "--limit-upload 256").OK()
	h.assert(t, "R-4.6 control: the backup is still running when its call is cut", running,
		"the throttled restic had already finished: nothing was stopped mid-run")

	// Cut the call: curl goes, the connection closes, the agent cancels.
	call.stop()
	stopped := false
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end) && !stopped; time.Sleep(500 * time.Millisecond) {
		stopped = !h.run("pgrep", "-f", "--", "--limit-upload 256").OK()
	}
	h.assert(t, "R-4.6 control: restic stops once the call is cut", stopped, "restic was still running 60 s after its call was cut")
	n := locks()
	h.assert(t, "R-4.6: a backup stopped mid-run leaves no lock in the repository (restic list locks is empty)", n == 0,
		"%d lock(s) left in %s: restic was killed before it could remove its lock", n, brepo)
	cleanup()
}

// backupWhileTmpChurns runs a backup while the site user keeps creating and
// deleting session-like files under tmp/sessions, as PHP's session GC does.
// One vanishing between restic listing the directory and reading it failed
// the backup (exit 3); tmp/ is now left out, so the snapshot holds nothing
// of it.
func (f *feat) backupWhileTmpChurns(t *testing.T, rpc *agentRPC, params map[string]any, restic func(...string) result) {
	h := f.h
	sessions := filepath.Join("/srv/sites", f.fs, "tmp/sessions")
	churn := fmt.Sprintf(`i=0; while :; do i=$((i+1)); j=0; while [ $j -lt 200 ]; do j=$((j+1)); : > '%[1]s/sess_cge2e_'$i'_'$j; done; sleep 0.05; rm -f '%[1]s'/sess_cge2e_*; done`, sessions)
	h.step("start the tmp/sessions churn as " + f.fsu)
	loop := execBackground("sudo", "-u", f.fsu, "sh", "-c", churn)
	stop := h.undo("stop the tmp/sessions churn", func() {
		loop.stop()
		h.run("pkill", "-u", f.fsu, "-f", "sess_cge2e_")
		if ents, err := os.ReadDir(sessions); err == nil {
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "sess_cge2e_") {
					_ = os.Remove(filepath.Join(sessions, e.Name()))
				}
			}
		}
	})
	defer stop()
	seen := false
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && !seen; time.Sleep(100 * time.Millisecond) {
		ents, _ := os.ReadDir(sessions)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "sess_cge2e_") {
				seen = true
			}
		}
	}
	h.assert(t, "R-4.6 control: files come and go under tmp/sessions during the backup", seen, "the churn loop made no files in %s", sessions)
	status, body := rpc.call("RunBackup", params, 600)
	stop()
	h.check(t, "R-4.6: a backup while files vanish under tmp/ succeeds", status, "200")
	snap := jsonField([]byte(body), "snapshot_id")
	if snap == "" {
		h.fail(t, "R-4.6: the backup holds nothing of tmp/", "R-4.6: no snapshot to inspect (%s)", head([]byte(body), 200))
		return
	}
	ls := restic("ls", "--no-lock", snap)
	under := 0
	prefix := filepath.Join("/srv/sites", f.fs, "tmp")
	for _, line := range strings.Split(ls.Out, "\n") {
		line = strings.TrimSpace(line)
		if line == prefix || strings.HasPrefix(line, prefix+"/") {
			under++
		}
	}
	h.assert(t, "R-4.6: the backup holds nothing of tmp/", ls.OK() && under == 0,
		"snapshot %s lists %d entries under %s (restic ls exit %d)", snap, under, prefix, ls.Code)
}
