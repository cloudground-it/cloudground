package agent

import (
	"context"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/system"
	"github.com/cloudground-it/cloudground/internal/tune"
)

// System-level paths the agent does not create, only reads.
const (
	nginxLogDir   = "/var/log/nginx"
	mariaDBLogDir = "/var/log/mysql"
	sshdConfig    = "/etc/ssh/sshd_config"
)

// systemHandlers returns the agent's ops/system handlers, built once from its
// adapters and paths (they keep a version cache).
func (a *Agent) systemHandlers() *system.Ops {
	a.systemOnce.Do(func() {
		e := a.Exec
		if e == nil {
			e = host.OSExec{}
		}
		fw := a.Firewall
		if fw == nil {
			fw = host.NewFirewall(e)
		}
		a.systemOps = &system.Ops{
			Systemd:  a.Systemd,
			Exec:     e,
			Firewall: fw,
			Log:      a.Log,
			Paths: system.Paths{
				SitesRoot:     a.Paths.SitesRoot,
				LogRoot:       a.Paths.LogRoot,
				NginxLogDir:   nginxLogDir,
				MariaDBLogDir: mariaDBLogDir,
				SSHDConfig:    sshdConfig,
				NginxSites:    a.Paths.NginxSites,
				NginxConfDir:  a.Paths.NginxConfDir,
				MariaDBConf:   filepath.Dir(tune.ConfigPath),
				PHPMasterDir:  a.Paths.PHPMasterDir,
				NginxAuthDir:  a.Paths.NginxAuthDir,
				SystemdDir:    a.Paths.SystemdDir,
			},
		}
	})
	return a.systemOps
}

// sshPort is the port shown to users for SFTP.
func (a *Agent) sshPort(ctx context.Context) int {
	s := a.systemHandlers()
	return system.SSHPort(ctx, s.Exec, s.Paths.SSHDConfig)
}
