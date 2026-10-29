package agent

import (
	"context"
	"os"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/agent/ops/system"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/version"
)

// processStart is when this agent process started: the API sees a restart
// as a new Capabilities.StartedAt.
var processStart = time.Now().UTC().Truncate(time.Second)

// capabilities answers Capabilities (internal/rpc SPEC.md §4) from the
// domains that own each fact: the installed PHP versions and the nginx
// build from ops/sites, the managed services from ops/system, the push
// policy from ops/staging, the layout from Paths, restic's version from
// `restic version`. It only reads.
func (a *Agent) capabilities(ctx context.Context, _ agentrpc.None) (rpc.Capabilities, error) {
	s := a.sitesHandlers()
	php := s.PHPVersions()
	if php == nil {
		php = []string{}
	}
	caps := rpc.Capabilities{
		AgentVersion:    version.Version,
		StartedAt:       processStart,
		PHPVersions:     php,
		Services:        system.Services(),
		ProtectedTables: staging.ProtectedRules(),
		Paths:           rpc.HostPaths{SitesRoot: a.Paths.SitesRoot, LogRoot: a.Paths.LogRoot, MySQLSocket: a.Paths.MySQLSocket},
		Features:        rpc.Features{HTTP3: s.HTTP3(ctx)},
		Runtimes:        a.runtimesHandlers().List(),
	}
	if v, err := a.ResticVersion(ctx); err == nil {
		caps.ResticVersion, caps.ResticSupported = v.Line, v.TypedExitCodes()
	}
	if b, err := os.ReadFile(ephemeralPortsFile); err == nil {
		caps.EphemeralPorts, _ = rpc.ParsePortRange(string(b))
	}
	return caps, nil
}

// ephemeralPortsFile is the kernel's range for the ports it picks itself.
var ephemeralPortsFile = "/proc/sys/net/ipv4/ip_local_port_range"
