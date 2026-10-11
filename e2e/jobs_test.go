//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// jobTask is the part of a task (GET /api/tasks/{id}) the job checks read.
type jobTask struct {
	ID        int64   `json:"id"`
	Status    string  `json:"status"`
	JobStatus string  `json:"job_status"`
	Error     string  `json:"error"`
	Log       string  `json:"log"`
	StartedAt *string `json:"started_at"`
}

func (a *apiClient) task(id int64) jobTask {
	var tk jobTask
	if id == 0 {
		return tk
	}
	_, b := a.get(fmt.Sprintf("/api/tasks/%d", id))
	_ = json.Unmarshal(b, &tk)
	return tk
}

// runMaintenance is POST /api/system/maintenance, which answers once the
// tick's jobs have ended, or with 202 and their IDs when that takes longer:
// then their end is polled here (up to 10 minutes), and the answer is
// given as the 200 it stands for.
func (a *apiClient) runMaintenance() (int, []byte) {
	c, b := a.post("/api/system/maintenance", `{}`)
	if c != http.StatusAccepted {
		return c, b
	}
	var r struct {
		Jobs []int64 `json:"jobs"`
	}
	_ = json.Unmarshal(b, &r)
	for _, id := range r.Jobs {
		if st := a.waitJobState(id, 10*time.Minute, "succeeded", "failed", "canceled"); st == "timeout" {
			return 0, []byte(fmt.Sprintf("maintenance job %d did not end", id))
		}
	}
	return http.StatusOK, []byte(`{"ran":true}`)
}

// waitJobState polls a task until its job_status is one of want (or the
// timeout); it returns the last job_status read.
func (a *apiClient) waitJobState(id int64, timeout time.Duration, want ...string) string {
	last := ""
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		last = a.task(id).JobStatus
		for _, w := range want {
			if last == w {
				return last
			}
		}
	}
	if last == "" {
		return "timeout"
	}
	return last
}

// jobQueue (R-4.2): background work is a persistent job queue. With the
// agent frozen (SIGSTOP), a change to a site holds the site's lock while it
// waits on the agent, so the next change to the same site waits queued
// behind it, and a third can be canceled before it ever runs. The API is
// then restarted with the first job still running: it drains, and the next
// start runs the interrupted render again instead of leaving it "running"
// or failing it, then the queued one. The old build ran every task at once
// as a goroutine, had no cancel, and failed whatever a restart cut.
func (h *harness) jobQueue(t *testing.T) {
	dom := h.dom("r42")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	h.check(t, "R-4.2 control: the static site provisions", h.api.waitActive(id, 120*time.Second), "active")
	if id == 0 {
		return
	}
	defer h.undo("delete the R-4.2 site", func() {
		h.api.relogin()
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	})()
	resume := h.undo("resume the agent", func() { h.run("systemctl", "kill", "-s", "CONT", "cloudground-agent") })
	defer resume()

	ttl := func(n int) int { return 3000 + int(h.pid%500)*3 + n }
	put := func(n int) int64 {
		_, b := h.api.do("PATCH", fmt.Sprintf("/api/sites/%d", id), fmt.Sprintf(`{"cache_ttl_sec":%d}`, ttl(n)))
		return taskID(b)
	}

	stop := h.run("systemctl", "kill", "-s", "STOP", "cloudground-agent")
	h.assert(t, "R-4.2 control: the agent is frozen", stop.OK(), "systemctl kill -s STOP: %s", stop.Err)
	a := put(1)
	h.check(t, "R-4.2 control: the first change runs (waiting on the frozen agent)", h.api.waitJobState(a, 30*time.Second, "running"), "running")
	b := put(2)
	c := put(3)
	time.Sleep(3 * time.Second)
	tb := h.api.task(b)
	h.check(t, "R-4.2: a second change to the same site waits queued behind the first", tb.JobStatus+" "+tb.Status, "queued pending")
	h.assert(t, "R-4.2: the waiting change has not started", tb.StartedAt == nil && h.api.task(a).JobStatus == "running",
		"second task %d started at %v while the first was %s", b, tb.StartedAt, h.api.task(a).JobStatus)

	_, cb := h.api.post(fmt.Sprintf("/api/tasks/%d/cancel", c), `{}`)
	h.check(t, "R-4.2: a queued task is canceled at once", jsonField(cb, "job_status"), "canceled")
	var site struct {
		Config struct {
			CacheTTLSec int `json:"cache_ttl_sec"`
		} `json:"config"`
		Desired int64 `json:"desired_generation"`
		Applied int64 `json:"applied_generation"`
	}
	_, sb := h.api.get(fmt.Sprintf("/api/sites/%d", id))
	_ = json.Unmarshal(sb, &site)
	h.check(t, "R-4.2: a canceled change puts back the value it stored", fmt.Sprint(site.Config.CacheTTLSec), fmt.Sprint(ttl(2)))

	// Restart with the first job still waiting on the frozen agent: the
	// drain gives it its deadline (60 s), then the process exits.
	started := time.Now()
	rs := h.run("systemctl", "restart", "cloudground-api.service")
	h.assert(t, "R-4.2 control: the API restarts", rs.OK(), "systemctl restart cloudground-api: %s", rs.Err)
	h.root.Logf("R-4.2: the restart with a running job took %s", time.Since(started).Round(time.Second))
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if c, _ := h.api.get("/api/session"); c == 200 {
			break
		}
	}
	h.api.relogin()
	// The agent is still frozen, so the change, put back in the queue by the
	// restart, is queued or (started again) running; the old build failed it.
	again := h.api.waitJobState(a, 30*time.Second, "queued", "running", "failed")
	h.assert(t, "R-4.2: after the restart the interrupted change is queued again, not failed", again == "queued" || again == "running",
		"task %d after the restart: %s (%s)", a, again, h.api.task(a).Error)
	resume()

	h.check(t, "R-4.2: the interrupted change runs again and succeeds", h.api.waitJobState(a, 180*time.Second, "succeeded", "failed", "canceled"), "succeeded")
	h.assert(t, "R-4.2: its log says the restart interrupted it", strings.Contains(h.api.task(a).Log, "panel restart"),
		"log of task %d: %q", a, h.api.task(a).Log)
	h.check(t, "R-4.2: the change queued behind it runs after the restart", h.api.waitJobState(b, 180*time.Second, "succeeded", "failed", "canceled"), "succeeded")
	tc := h.api.task(c)
	h.check(t, "R-4.2: the canceled change never ran", fmt.Sprintf("%s started=%v", tc.JobStatus, tc.StartedAt != nil), "canceled started=false")

	_, sb = h.api.get(fmt.Sprintf("/api/sites/%d", id))
	_ = json.Unmarshal(sb, &site)
	h.check(t, "R-4.2: the site ends with the last change that ran, applied",
		fmt.Sprintf("ttl=%d applied=%v", site.Config.CacheTTLSec, site.Applied == site.Desired), fmt.Sprintf("ttl=%d applied=true", ttl(2)))
}
