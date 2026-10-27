package agent

import "github.com/cloudground-it/cloudground/internal/agent/ops/traffic"

// trafficHandlers returns the agent's ops/traffic handlers (the sites'
// statistics from their access logs, #33), built once.
func (a *Agent) trafficHandlers() *traffic.Ops {
	a.trafficOnce.Do(func() {
		a.trafficOps = &traffic.Ops{LogRoot: a.Paths.LogRoot, Dir: a.Paths.TrafficDir, Log: a.log()}
	})
	return a.trafficOps
}
