package agent

import (
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cron"
)

// cronHandlers returns the agent's ops/cron handlers, built once.
func (a *Agent) cronHandlers() *cron.Ops {
	a.cronOnce.Do(func() {
		e := a.Exec
		if e == nil {
			e = host.OSExec{Timeout: time.Minute}
		}
		a.cronOps = &cron.Ops{Exec: e, Systemd: a.Systemd, CrontabDir: a.Paths.CrontabDir,
			CronDir: a.Paths.CronDir, SpecDir: a.Paths.CronSpecDir, BinDir: a.Paths.BinDir}
	})
	return a.cronOps
}
