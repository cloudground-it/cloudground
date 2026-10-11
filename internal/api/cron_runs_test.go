package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A job's last run includes its scheduled runs, which the runner records on
// the agent's side: it used to stay empty for a job that ran only on its
// schedule. A run on demand later than the scheduled one keeps its own.
func TestCronListShowsScheduledRuns(t *testing.T) {
	_, agent, c, site := cronSite(t)
	mk := func(cmd string) state.CronJob {
		resp, body := c.do("POST", fmt.Sprintf("/api/sites/%d/cron", site.ID), map[string]string{"schedule": "* * * * *", "command": cmd})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create = %d %s", resp.StatusCode, body)
		}
		var j state.CronJob
		json.Unmarshal(body, &j)
		return j
	}
	scheduled, onDemand := mk("echo scheduled"), mk("echo now")
	agent.mu.Lock()
	agent.results[agentrpc.RunCron.Name()] = rpc.RunCronResult{Status: "succeeded"}
	agent.mu.Unlock()
	if resp, body := c.do("POST", fmt.Sprintf("/api/cron/%d/run", onDemand.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("run = %d %s", resp.StatusCode, body)
	}
	ranAt := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	agent.setResult(agentrpc.CronRuns.Name(), rpc.CronRunsResult{Runs: map[string]rpc.CronRun{
		strconv.FormatInt(scheduled.ID, 10): {At: ranAt, Status: "failed", ExitCode: 2},
		strconv.FormatInt(onDemand.ID, 10):  {At: ranAt.Add(-24 * time.Hour), Status: "failed", ExitCode: 1},
	}})
	list := func() map[int64]state.CronJob {
		resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d/cron", site.ID), nil)
		var jobs []state.CronJob
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &jobs) != nil {
			t.Fatalf("list = %d %s", resp.StatusCode, body)
		}
		out := map[int64]state.CronJob{}
		for _, j := range jobs {
			out[j.ID] = j
		}
		return out
	}
	got := list()
	if j := got[scheduled.ID]; j.LastRun == nil || !j.LastRun.Equal(ranAt) || j.LastStatus != "failed" {
		t.Fatalf("scheduled job: %+v", j)
	}
	if j := got[onDemand.ID]; j.LastRun == nil || j.LastRun.Equal(ranAt.Add(-24*time.Hour)) || j.LastStatus != "succeeded" {
		t.Fatalf("the run on demand, later, lost to an older scheduled run: %+v", j)
	}

	// An agent that does not answer leaves the store's record.
	agent.setErr(agentrpc.CronRuns.Name(), errors.New("down"))
	if j := list()[scheduled.ID]; j.LastRun != nil {
		t.Fatalf("without the agent: %+v", j)
	}
}
