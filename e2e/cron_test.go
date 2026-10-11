//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cronJobs (R-3.11): the agent renders the crontab from typed jobs, refuses
// what cron would refuse, takes disabled jobs out, and runs a job now in a
// transient unit whose whole cgroup goes when it ends or overruns.
func (f *feat) cronJobs(t *testing.T) {
	h := f.h
	add := func(schedule, command string) (int, int64) {
		c, b := h.api.post(f.api("/cron"), map[string]string{"schedule": schedule, "command": command})
		id, _ := strconv.ParseInt(jsonField(b, "id"), 10, 64)
		if id != 0 {
			h.undo("delete cron job", func() { h.api.del(fmt.Sprintf("/api/cron/%d", id)) })
		}
		return c, id
	}
	// The site's schedule: its cron.d file (R-4.5: root's lines that start the
	// sandboxed runner; no per-user crontab any more).
	crontab := func() string { b, _ := os.ReadFile("/etc/cron.d/cloudground-" + f.fsu); return string(b) }

	// Schedules and commands crontab(1) refuses are a 400, before anything
	// is stored. The old API accepted "5/10" and "*/0" and over-long commands,
	// crontab then refused the whole table, and the create answered 502.
	for _, bad := range []struct{ name, schedule, command string }{
		{"a step on a single value", "5/10 * * * *", "true"},
		{"a zero step", "*/0 * * * *", "true"},
		{"an out-of-range minute", "60 * * * *", "true"},
		{"a newline in the schedule", "* * * * *\n* * * * * id", "true"},
		{"a command longer than cron's line", "* * * * *", strings.Repeat("x", 990)},
	} {
		c, _ := add(bad.schedule, bad.command)
		h.check(t, "R-3.11: "+bad.name+" is refused with 400", strconv.Itoa(c), "400")
	}
	h.assert(t, "R-3.11: the refused jobs left the schedule alone", strings.Contains(crontab(), " root /usr/local/bin/cg-site-cron "+f.fsu+" wp\n"),
		"R-3.11: the site schedule lost its WordPress line")

	// A job with "\%" and "%" runs as typed. The old renderer escaped "\%"
	// into "\\%", which cron reads as the end of the command: the job was cut
	// mid-quote and never ran.
	marker := fmt.Sprintf("cg-e2e-cron-%d", h.pid)
	c, id := add("* * * * *", `echo "\%" 50% `+marker)
	h.check(t, "R-3.11: a job with % is accepted", strconv.Itoa(c), "201")
	if id != 0 {
		logPath := fmt.Sprintf("/srv/sites/%s/logs/cron-%d.log", f.fs, id)
		var log []byte
		for deadline := time.Now().Add(130 * time.Second); time.Now().Before(deadline); time.Sleep(pollEvery) {
			if log, _ = os.ReadFile(logPath); bytes.Contains(log, []byte("[cloudground end")) {
				break
			}
		}
		h.assert(t, "R-3.11: cron runs a job with \\% and % verbatim", bytes.Contains(log, []byte(`\% 50% `+marker+"\n")) &&
			bytes.Contains(log, []byte("status=0]")), "R-3.11: %s after two minutes: %q", logPath, head(log, 400))

		// W3: the scheduled run is the job's last run in the API (it only
		// knew of runs on demand: last_run stayed empty).
		var job struct {
			LastRun    string `json:"last_run"`
			LastStatus string `json:"last_status"`
		}
		var lb []byte
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && job.LastRun == ""; time.Sleep(pollEvery) {
			_, lb = h.api.get(f.api("/cron"))
			var jobs []struct {
				ID         int64  `json:"id"`
				LastRun    string `json:"last_run"`
				LastStatus string `json:"last_status"`
			}
			_ = json.Unmarshal(lb, &jobs)
			for _, j := range jobs {
				if j.ID == id {
					job.LastRun, job.LastStatus = j.LastRun, j.LastStatus
				}
			}
		}
		h.assert(t, "W3: a scheduled run is the job's last run", job.LastRun != "" && job.LastStatus == "succeeded", "cron: %s", head(lb, 400))

		// Disabling takes it out of the crontab; enabling puts it back.
		line := " " + f.fsu + " " + strconv.FormatInt(id, 10) + "\n"
		h.assert(t, "R-3.11: control: the job is in the crontab", strings.Contains(crontab(), line), "R-3.11: job %d is not in the crontab", id)
		c, _ := h.api.do("PATCH", fmt.Sprintf("/api/cron/%d", id), map[string]any{"enabled": false})
		h.check(t, "R-3.11: a job can be disabled", strconv.Itoa(c), "200")
		h.assert(t, "R-3.11: a disabled job leaves the crontab", !strings.Contains(crontab(), line), "R-3.11: disabled job %d is still in the crontab", id)
		c, _ = h.api.do("PATCH", fmt.Sprintf("/api/cron/%d", id), map[string]any{"enabled": true, "schedule": "0 4 * * *"})
		h.check(t, "R-3.11: a job can be edited and re-enabled", strconv.Itoa(c), "200")
		ct := crontab()
		h.assert(t, "R-3.11: the edited job is back with its new schedule", strings.Contains(ct, "0 4 * * * ") && strings.Contains(ct, line),
			"R-3.11: crontab after the edit:\n%s", ct)
	}

	// R-4.5 review: site code the agent runs is in the site's sandbox, as its
	// PHP is: a scheduled job, a job run now and wp-cli see only their own
	// site under /srv/sites and run with no new privileges.
	probe := `printf 'sites=%s nnp=%s\n' "$(ls /srv/sites | tr '\n' ,)" "$(awk '/^NoNewPrivs/{print $2}' /proc/self/status)"`
	want := "sites=" + f.fs + ", nnp=1"
	seen := fmt.Sprintf("/srv/sites/%s/logs/cg-e2e-sandbox-%d", f.fs, h.pid)
	h.undo("remove the sandbox probe", func() { _ = os.Remove(seen) })
	if _, sid := add("* * * * *", probe+" > "+seen); sid != 0 {
		var got []byte
		for deadline := time.Now().Add(130 * time.Second); time.Now().Before(deadline); time.Sleep(pollEvery) {
			if got, _ = os.ReadFile(seen); len(got) > 0 {
				break
			}
		}
		h.check(t, "R-4.5: a scheduled job runs in the site's sandbox", strings.TrimSpace(string(got)), want)
		h.api.del(fmt.Sprintf("/api/cron/%d", sid))
	}
	if _, nid := add("0 5 * * *", probe); nid != 0 {
		_, b := h.api.post(fmt.Sprintf("/api/cron/%d/run", nid), map[string]string{})
		h.check(t, "R-4.5: a job run now runs in the site's sandbox", strings.TrimSpace(jsonField(b, "output")), want)
		h.api.del(fmt.Sprintf("/api/cron/%d", nid))
	}
	cliProbe := f.fp + "/wp-content/mu-plugins/cg-e2e-cli-sandbox.php"
	cliSeen := fmt.Sprintf("/srv/sites/%s/logs/cg-e2e-cli-sandbox-%d", f.fs, h.pid)
	h.mustWriteAs(t, cliProbe, fmt.Sprintf(`<?php if (PHP_SAPI === 'cli') { $s = array_values(array_diff(scandir('/srv/sites'), ['.', '..']));
preg_match('/^NoNewPrivs:\s*(\d)/m', file_get_contents('/proc/self/status'), $m);
file_put_contents(%q, 'sites=' . implode(',', $s) . ', nnp=' . $m[1]); }`, cliSeen), f.fsu)
	h.undo("remove the wp-cli sandbox probe", func() { _ = os.Remove(cliProbe); _ = os.Remove(cliSeen) })
	h.api.get(f.api("/wp/plugins")) // the agent runs wp-cli for it
	cli, _ := os.ReadFile(cliSeen)
	h.check(t, "R-4.5: wp-cli runs in the site's sandbox", string(cli), "sites="+f.fs+", nnp=1")
	_ = os.Remove(cliProbe)

	// Run now: a child that left the job's process group (setsid) goes when
	// the job ends. The old runner killed one process group, and only on
	// timeout: this child ran on as the site user.
	child := fmt.Sprintf("299.%d", h.pid)
	_, rid := add("0 5 * * *", "setsid sleep "+child+" </dev/null >/dev/null 2>&1 & echo started")
	if rid != 0 {
		h.undo("kill the run-now child", func() { h.run("pkill", "-f", "sleep "+child) })
		c, b := h.api.post(fmt.Sprintf("/api/cron/%d/run", rid), map[string]string{})
		h.assert(t, "R-3.11: run now reports the job's own output", c == 200 && jsonField(b, "status") == "succeeded" && strings.Contains(jsonField(b, "output"), "started"),
			"R-3.11: run now = %d %s", c, head(b, 300))
		h.check(t, "R-3.11: run now leaves no process behind", strconv.Itoa(procsMatching("sleep "+child)), "0")
	}

	// Run now, a job that fails: its status and exit code, from systemd-run's
	// result line (systemd 259 indents it; read as no line at all, this was
	// a 500 "Could not run the cron job").
	if _, fid := add("0 5 * * *", "echo failing; exit 4"); fid != 0 {
		c, b := h.api.post(fmt.Sprintf("/api/cron/%d/run", fid), map[string]string{})
		h.assert(t, "R-3.11: run now reports a failing job's exit code", c == 200 && jsonField(b, "status") == "failed" && jsonField(b, "exit_code") == "4" && strings.Contains(jsonField(b, "output"), "failing"),
			"R-3.11: run now = %d %s", c, head(b, 300))
	}

	// Run now with a time limit (the agent's max_seconds, which the API
	// leaves at 300 s): the job and its children are killed at the limit.
	var site json.RawMessage
	_, site = h.api.get(fmt.Sprintf("/api/sites/%d", f.fid))
	slow := fmt.Sprintf("298.%d", h.pid)
	body := fmt.Sprintf(`{"site":%s,"job":{"id":%d,"schedule":"* * * * *","command":"sleep %s & sleep %s","enabled":true},"max_seconds":3}`,
		site, 999000+h.pid%1000, slow, slow)
	h.undo("kill the timed-out job", func() { h.run("pkill", "-f", "sleep "+slow) })
	start := time.Now()
	code, b, err := h.agentCall("RunCron", body)
	took := time.Since(start)
	h.assert(t, "R-3.11: a job past its time limit is reported as timeout", err == nil && code == 200 && jsonField(b, "status") == "timeout" && took < 20*time.Second,
		"R-3.11: RunCron with max_seconds=3 = %d %s (%v) after %s", code, head(b, 300), err, took)
	h.check(t, "R-3.11: a job past its time limit leaves no process behind", strconv.Itoa(procsMatching("sleep "+slow)), "0")
}

// procsMatching counts the processes whose command line contains s.
func procsMatching(s string) int {
	ents, _ := os.ReadDir("/proc")
	n := 0
	self := strconv.Itoa(os.Getpid())
	for _, e := range ents {
		if e.Name() == self {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), s) {
			n++
		}
	}
	return n
}
