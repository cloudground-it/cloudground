package agent

import (
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/gitdeploy"
)

// gitDeployHandlers returns the agent's ops/gitdeploy handlers, built once.
func (a *Agent) gitDeployHandlers() *gitdeploy.Ops {
	a.gitDeployOnce.Do(func() {
		e := a.Exec
		if e == nil {
			e = host.OSExec{Timeout: time.Minute}
		}
		a.gitDeployOps = &gitdeploy.Ops{Open: a.openSite, Exec: e}
	})
	return a.gitDeployOps
}
