package agent

import (
	"context"
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
	}
	if v, err := a.ResticVersion(ctx); err == nil {
		caps.ResticVersion, caps.ResticSupported = v.Line, v.TypedExitCodes()
	}
	return caps, nil
}
