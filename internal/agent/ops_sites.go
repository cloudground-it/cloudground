package agent

import (
	"github.com/cloudground-it/cloudground/internal/agent/ops/sites"
)

// sitesHandlers returns the agent's ops/sites handlers (create, delete,
// apply, and the render and PHP control other domains call), built once.
func (a *Agent) sitesHandlers() *sites.Ops {
	a.sitesOnce.Do(func() {
		p := a.Paths
		a.sitesOps = &sites.Ops{
			Paths: sites.Paths{
				SitesRoot: p.SitesRoot, CacheRoot: p.CacheRoot, LogRoot: p.LogRoot,
				ACMEWebroot: p.ACMEWebroot, FallbackCert: p.FallbackCert, FallbackKey: p.FallbackKey,
				CertLiveDir: p.CertLiveDir, CustomCertDir: p.CustomCertDir,
				NginxSites: p.NginxSites, NginxConfDir: p.NginxConfDir, NginxAuthDir: p.NginxAuthDir,
				PHPBinDir: p.PHPBinDir, PHPSocketDir: p.PHPSocketDir, PHPMasterDir: p.PHPMasterDir,
				SystemdDir: p.SystemdDir, Quarantine: p.Quarantine, AuthLogSocket: p.AuthLogSocket,
			},
			Renderer:  a.Renderer,
			Accounts:  a.Accounts,
			Systemd:   a.Systemd,
			Nginx:     a.nginxCtl(),
			Exec:      a.exec(),
			Open:      a.openSite,
			Owner:     a.siteOwner,
			DB:        a.dbHandlers(),
			SFTP:      a.sftpHandlers(),
			Certs:     a.certHandlers(),
			Cron:      a.cronHandlers(),
			WordPress: a.wpHandlers(),
			Relink:    relinkSite,
			NginxMu:   &a.nginxMu,
			Log:       a.log(),
		}
	})
	return a.sitesOps
}
