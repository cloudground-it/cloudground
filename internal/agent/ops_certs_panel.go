package agent

import (
	"context"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/certs"
	"github.com/cloudground-it/cloudground/internal/agent/ops/panel"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// panelGroup is the API's group: it reads the panel's certificate and key.
const panelGroup = "cloudground"

// certHandlers returns the agent's ops/certs handlers, built once from its
// adapters, paths and the shared certbot lock.
func (a *Agent) certHandlers() *certs.Ops {
	a.certsOnce.Do(func() {
		a.certsOps = &certs.Ops{
			Paths: certs.Paths{
				CertLiveDir:   a.Paths.CertLiveDir,
				CustomCertDir: a.Paths.CustomCertDir,
				ACMEWebroot:   a.Paths.ACMEWebroot,
				PanelCert:     a.Paths.PanelCert,
			},
			ACME: a.acme(),
			// Resolved at call time: ops/sites is built on this package.
			Render: func(ctx context.Context, s state.Site) ([]rpc.ManagedFile, error) {
				return a.sitesHandlers().Render(ctx, s)
			},
			CertMu: &a.certMu,
			Log:    a.log(),
		}
	})
	return a.certsOps
}

// panelHandlers returns the agent's ops/panel handlers, built once.
func (a *Agent) panelHandlers() *panel.Ops {
	a.panelOnce.Do(func() {
		a.panelOps = &panel.Ops{
			Paths: panel.Paths{
				NginxSites:  a.Paths.NginxSites,
				ACMEWebroot: a.Paths.ACMEWebroot,
				CertLiveDir: a.Paths.CertLiveDir,
				PanelCert:   a.Paths.PanelCert,
				PanelKey:    a.Paths.PanelKey,
				PanelAccess: a.Paths.PanelAccess,
				PanelGroup:  panelGroup,
				APISocket:   a.Paths.APISocket,
				BinDir:      a.Paths.BinDir,
				SystemdDir:  a.Paths.SystemdDir,
				WorkDir:     a.Paths.WorkDir,
				StateDB:     a.Paths.StateDB,
				Records:     a.records(),
			},
			ACME:         a.acme(),
			Nginx:        a.nginxCtl(),
			Systemd:      a.Systemd,
			CertMu:       &a.certMu,
			NginxMu:      &a.nginxMu,
			Preflight:    a.certHandlers().Preflight,
			UpdateClient: a.UpdateClient,
			Log:          a.log(),
		}
	})
	return a.panelOps
}

// acme is the certbot adapter; an agent built without one gets the real
// certbot.
func (a *Agent) acme() host.ACME {
	if a.ACME != nil {
		return a.ACME
	}
	return host.NewCertbot(host.OSExec{Timeout: 20 * time.Minute})
}
