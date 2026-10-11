// Package system serves the agent's machine-level methods: system status,
// managed services, log tails, the firewall and the drift check. SPEC.md in
// this directory is the contract; everything goes through the host adapters.
package system

import (
	"context"
	"log/slog"
	"sync"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Paths are the places this package reads. The agent fills them from its own
// layout; tests point them into temp directories.
type Paths struct {
	SitesRoot     string // disk numbers; drift scope for mu-plugins
	LogRoot       string // <LogRoot>/<domain>/{access,error,php-error}.log
	NginxLogDir   string // /var/log/nginx
	MariaDBLogDir string // /var/log/mysql
	SSHDConfig    string // /etc/ssh/sshd_config, read only when `sshd -T` fails

	// Drift scope: the places the panel writes managed files.
	NginxSites   string
	NginxConfDir string
	MariaDBConf  string // the tuning fragment's directory
	PHPMasterDir string
	NginxAuthDir string
	SystemdDir   string
}

// PanelPort is the port the panel answers on (internal/install, the bootstrap vhost).
const PanelPort = 8443

// Ops holds the adapters the handlers use.
type Ops struct {
	Paths    Paths
	Systemd  host.Systemd
	Exec     host.Exec
	Firewall host.Firewall
	Log      *slog.Logger

	mu       sync.Mutex
	versions map[versionKey]string
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.SystemStatus, func(ctx context.Context, _ agentrpc.None) (rpc.SystemStatusResult, error) {
		return o.SystemStatus(ctx)
	})
	agentrpc.Handle(s, agentrpc.ServiceStatus, func(ctx context.Context, _ agentrpc.None) (rpc.ServiceStatusResult, error) {
		return o.ServiceStatus(ctx)
	})
	agentrpc.Handle(s, agentrpc.RestartService, o.RestartService)
	agentrpc.Handle(s, agentrpc.TailLog, o.TailLog)
	agentrpc.Handle(s, agentrpc.FirewallStatus, func(ctx context.Context, _ agentrpc.None) (rpc.FirewallStatusResult, error) {
		return o.FirewallStatus(ctx)
	})
	agentrpc.Handle(s, agentrpc.FirewallRule, o.FirewallRule)
	agentrpc.Handle(s, agentrpc.CheckDrift, o.CheckDrift)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}
