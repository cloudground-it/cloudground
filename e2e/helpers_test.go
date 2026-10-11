//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperUnits is the #66 section: the agent unit carries its memory limit,
// the heavy helpers (restic, mariadb-dump) run in units of their own in
// cg-helper.slice and never in the agent's cgroup, and a helper killed for
// memory fails its backup while the agent runs on, the same process. It
// calls the agent on its socket with a repository of its own, so the
// panel's backup setting is never touched.
func (h *harness) helperUnits(t *testing.T) {
	h.clearStandIns(t, "restic", "mariadb-dump")
	h.withFeature(t, func(f *feat) { f.helperUnits(t) })
}

// agentUnit reads properties of cloudground-agent.service.
func (h *harness) agentUnit(props ...string) map[string]string {
	args := []string{"show", "cloudground-agent.service"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out := map[string]string{}
	for _, line := range strings.Split(h.run("systemctl", args...).Out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			out[k] = v
		}
	}
	return out
}

func (f *feat) helperUnits(t *testing.T) {
	h := f.h
	u := h.agentUnit("MemoryHigh", "MemoryMax", "OOMPolicy")
	h.assert(t, "#66: the agent unit has its memory limit (MemoryHigh 768M, MemoryMax 1G, OOMPolicy continue)",
		u["MemoryHigh"] == strconv.Itoa(768<<20) && u["MemoryMax"] == strconv.Itoa(1<<30) && u["OOMPolicy"] == "continue",
		"systemctl show cloudground-agent: %v", u)

	rpc, closeRPC, err := newAgentRPC(h)
	if err != nil {
		h.fail(t, "#66 fixture: the agent's token", "#66 fixture: %v", err)
		return
	}
	defer closeRPC()
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-helpers-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	defer h.undo("remove the #66 repository", func() { _ = os.RemoveAll(brepo) })()
	_, siteJSON := h.api.get(f.api(""))
	backup := map[string]any{"site": json.RawMessage(siteJSON), "repository": brepo, "password": bpw, "kind": "database"}

	f.helpersInTheirUnits(t, rpc, backup)
	f.helperKilledForMemory(t, rpc, backup)
}

// helpersInTheirUnits: stand-ins ahead of restic and mariadb-dump on the
// agent's PATH (/usr/local/sbin) record the cgroup they were started in,
// then exec the real tools; a database backup runs both.
func (f *feat) helpersInTheirUnits(t *testing.T, rpc *agentRPC, backup map[string]any) {
	h := f.h
	const record = "/run/cg-e2e-helper-cgroups"
	tools := []string{"restic", "mariadb-dump"}
	var standIns []string
	cleanup := h.undo("remove the #66 cgroup stand-ins", func() {
		for _, s := range standIns {
			_ = os.Remove(s)
		}
		_ = os.Remove(record)
	})
	defer cleanup()
	_ = os.Remove(record)
	for _, tool := range tools {
		real, err := exec.LookPath(tool)
		if err != nil {
			h.fail(t, "#66 fixture: "+tool+" is on PATH", "#66 fixture: %v", err)
			return
		}
		s := "/usr/local/sbin/" + tool
		script := fmt.Sprintf("#!/bin/sh\necho \"%s $(cat /proc/self/cgroup)\" >> %s\nexec %q \"$@\"\n", tool, record, real)
		h.step("install " + s)
		if err := os.WriteFile(s, []byte(script), 0o755); err != nil {
			h.fail(t, "#66 fixture: the "+tool+" stand-in", "#66 fixture: %v", err)
			return
		}
		standIns = append(standIns, s)
	}
	status, body := rpc.call("RunBackup", backup, 600)
	h.assert(t, "#66 control: a database backup through the stand-ins succeeds", status == "200",
		"RunBackup %s: %s", status, head([]byte(body), 300))
	lines := strings.Split(readTrim(record), "\n")
	for _, tool := range tools {
		var seen []string
		for _, l := range lines {
			if strings.HasPrefix(l, tool+" ") {
				seen = append(seen, strings.TrimPrefix(l, tool+" "))
			}
		}
		inUnit := len(seen) > 0
		for _, cg := range seen {
			if !strings.Contains(cg, "/cg.slice/cg-helper.slice/cg-helper-"+tool+"-") || strings.Contains(cg, "cloudground-agent.service") {
				inUnit = false
			}
		}
		h.assert(t, "#66: "+tool+" runs in a unit of its own in cg-helper.slice, not in the agent's cgroup", inUnit,
			"%s ran in: %q (record: %q)", tool, seen, lines)
	}
	cleanup()
}

// helperKilledForMemory: with cg-helper.slice capped low for the test (a
// runtime drop-in), a mariadb-dump stand-in that grows without end is
// killed by the kernel inside its unit; the backup fails, the agent is the
// same process, not restarted, and serves the next backup.
func (f *feat) helperKilledForMemory(t *testing.T, rpc *agentRPC, backup map[string]any) {
	h := f.h
	const standIn = "/usr/local/sbin/mariadb-dump"
	const dropDir = "/run/systemd/system/cg-helper.slice.d"
	const drop = dropDir + "/50-cg-e2e-oom.conf"
	cleanup := h.undo("remove the #66 memory hog and the slice cap", func() {
		_ = os.Remove(standIn)
		_ = os.Remove(drop)
		_ = os.Remove(dropDir)
		h.run("systemctl", "daemon-reload")
	})
	defer cleanup()
	h.step("install " + standIn + " (grows until it is killed)")
	if err := os.WriteFile(standIn, []byte("#!/bin/sh\nx=a\nwhile :; do x=\"$x$x\"; done\n"), 0o755); err != nil {
		h.fail(t, "#66 fixture: the memory hog", "#66 fixture: %v", err)
		return
	}
	_ = os.MkdirAll(dropDir, 0o755)
	if err := os.WriteFile(drop, []byte("[Slice]\nMemoryMax=64M\nMemorySwapMax=0\n"), 0o644); err != nil {
		h.fail(t, "#66 fixture: the slice cap", "#66 fixture: %v", err)
		return
	}
	h.run("systemctl", "daemon-reload")

	before := h.agentUnit("MainPID", "NRestarts")
	since := strconv.FormatInt(time.Now().Unix()-1, 10)
	status, body := rpc.call("RunBackup", backup, 300)
	h.assert(t, "#66: a backup whose mariadb-dump is killed for memory fails", status != "200" && status != "",
		"RunBackup %s: %s", status, head([]byte(body), 300))
	j := h.run("journalctl", "-q", "--no-pager", "-o", "cat", "--since", "@"+since, "-u", "cg-helper-mariadb-dump-*").Out
	h.assert(t, "#66: the kernel killed the helper inside its own unit (result oom-kill)", strings.Contains(j, "Failed with result 'oom-kill'"),
		"journal of cg-helper-mariadb-dump-*: %s", head([]byte(j), 600))
	after := h.agentUnit("MainPID", "NRestarts", "ActiveState")
	h.assert(t, "#66: the agent survives the helper's kill (same MainPID, no restart, active)",
		after["MainPID"] == before["MainPID"] && after["MainPID"] != "0" && after["NRestarts"] == before["NRestarts"] && after["ActiveState"] == "active",
		"before %v, after %v", before, after)
	cleanup()

	status, body = rpc.call("RunBackup", backup, 600)
	h.assert(t, "#66: the agent then serves the next backup", status == "200", "RunBackup %s: %s", status, head([]byte(body), 300))
}
