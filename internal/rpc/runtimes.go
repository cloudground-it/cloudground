package rpc

import (
	"errors"
	"fmt"
	"time"

	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// RuntimeVersion is one installed runtime version (#28, ops/runtimes SPEC
// §1): Version as sites name it ("22", "3.12"), Full as it reports itself.
type RuntimeVersion struct {
	Version string `json:"version"`
	Full    string `json:"full"`
}

// RuntimesResult is what the server has.
type RuntimesResult struct {
	Node   []RuntimeVersion `json:"node"`
	Python []RuntimeVersion `json:"python"`
}

// Has reports whether runtime's version is installed.
func (r RuntimesResult) Has(runtime, version string) bool {
	list := r.Python
	if runtime == runtimes.Node {
		list = r.Node
	}
	for _, v := range list {
		if v.Version == version {
			return true
		}
	}
	return false
}

// RuntimeInstallParams asks for one runtime version.
type RuntimeInstallParams struct {
	Runtime string `json:"runtime"`
	Version string `json:"version"`
}

// Validate checks the runtime and the version's form (SPEC §3).
func (p *RuntimeInstallParams) Validate() error {
	if p.Runtime != runtimes.Node && p.Runtime != runtimes.Python {
		return fmt.Errorf("unknown runtime %q", p.Runtime)
	}
	if !runtimes.ValidVersion(p.Runtime, p.Version) {
		return fmt.Errorf("invalid %s version %q", p.Runtime, p.Version)
	}
	return nil
}

// AppEnvParams is a managed app's whole environment, plain and secret
// values together (ops/sites SPEC §11.2).
type AppEnvParams struct {
	Site state.Site        `json:"site"`
	Env  map[string]string `json:"env"`
}

func (p *AppEnvParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the site's type and the environment (SPEC §3).
func (p *AppEnvParams) Validate() error {
	if !p.Site.Type.IsApp() {
		return errors.New("not a Node.js or Python site")
	}
	return runtimes.CheckEnv(p.Env)
}

// AppControlParams starts, stops or restarts a managed app.
type AppControlParams struct {
	Site   state.Site `json:"site"`
	Action string     `json:"action"`
}

func (p *AppControlParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate checks the action (SPEC §3).
func (p *AppControlParams) Validate() error {
	switch p.Action {
	case "start", "stop", "restart":
		return nil
	}
	return fmt.Errorf("unknown action %q", p.Action)
}

// AppHealth is the health probe's answer (ops/sites SPEC §11.3).
type AppHealth struct {
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

// AppStatusResult is a managed app's unit and health.
type AppStatusResult struct {
	Active   bool       `json:"active"`
	State    string     `json:"state"`
	Since    *time.Time `json:"since,omitempty"`
	Restarts int        `json:"restarts"`
	LastExit int        `json:"last_exit"`
	Health   AppHealth  `json:"health"`
}

// PrepareAppResult is the dependency step's output (the last 4 KiB).
type PrepareAppResult struct {
	Output string `json:"output"`
}
