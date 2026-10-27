package agent

import (
	"os"

	"github.com/cloudground-it/cloudground/internal/agent/ops/runtimes"
)

// runtimesHandlers returns the agent's ops/runtimes handlers (Node.js and
// Python for managed app sites, #28), built once.
func (a *Agent) runtimesHandlers() *runtimes.Ops {
	a.runtimesOnce.Do(func() {
		a.runtimes = &runtimes.Ops{Exec: a.exec(), Dir: a.Paths.RuntimesDir}
		if bin, err := os.Executable(); err == nil {
			a.runtimes.AgentBinary = bin
		}
	})
	return a.runtimes
}
