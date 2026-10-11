package rpc

import (
	"time"

	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/state"
)

// CronParams is every scheduled job of a site. The agent renders the
// site's crontab from them and installs it (ops/cron); no shell text comes
// from the API.
type CronParams struct {
	Site state.Site    `json:"site"`
	Jobs []cronjob.Job `json:"jobs"`
}

// Sites lists the site whose crontab is replaced.
func (p *CronParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// CronResult counts the crontab's entries, WordPress's own among them; 0
// means the site has no crontab.
type CronResult struct {
	Entries int `json:"entries"`
}

// RunCronParams runs one job now, as the site's user, for at most
// MaxSeconds (0, or more than the agent's 300 s limit, means the limit).
type RunCronParams struct {
	Site       state.Site  `json:"site"`
	Job        cronjob.Job `json:"job"`
	MaxSeconds int         `json:"max_seconds,omitempty"`
}

// Sites lists the site the job belongs to.
func (p *RunCronParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// RunCronResult is how the run ended ("succeeded", "failed" or "timeout"),
// its exit code and the last 64 KiB of its output.
type RunCronResult struct {
	Status   string `json:"status"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// CronRunsParams asks for the scheduled runs a site's jobs last ended.
type CronRunsParams struct {
	Site state.Site `json:"site"`
}

// Sites lists the site whose runs are read.
func (p *CronRunsParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// CronRun is how a scheduled run of a job ended: when, "succeeded" or
// "failed", and its exit code.
type CronRun struct {
	At       time.Time `json:"at"`
	Status   string    `json:"status"`
	ExitCode int       `json:"exit_code"`
}

// CronRunsResult is the last scheduled run of each job that has had one,
// by job id. The runner records them (ops/cron SPEC); a run on demand is
// the API's own record.
type CronRunsResult struct {
	Runs map[string]CronRun `json:"runs"`
}
