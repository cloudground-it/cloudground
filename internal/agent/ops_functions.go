package agent

import (
	"github.com/cloudground-it/cloudground/internal/agent/ops/functions"
)

// functionsHandlers returns the agent's ops/functions handlers (a site's
// Lambda-style functions, #143), built once.
func (a *Agent) functionsHandlers() *functions.Ops {
	a.functionsOnce.Do(func() {
		a.functions = &functions.Ops{
			Systemd: a.Systemd, Open: a.openSite, SystemdDir: a.Paths.SystemdDir, Dir: a.Paths.FunctionsDir,
			LogRoot: a.Paths.LogRoot, Log: a.log(),
			Installed: func(runtime, version string) bool { return a.runtimesHandlers().List().Has(runtime, version) },
		}
	})
	return a.functions
}
