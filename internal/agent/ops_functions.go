package agent

import (
	"github.com/cloudground-it/cloudground/internal/agent/ops/functions"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sites"
)

// functionsHandlers returns the agent's ops/functions handlers (standalone
// functions, #143 v0.9.0), built once.
func (a *Agent) functionsHandlers() *functions.Ops {
	a.functionsOnce.Do(func() {
		p := a.Paths
		a.functions = &functions.Ops{
			Systemd: a.Systemd, Accounts: a.Accounts, Nginx: a.nginxCtl(), ACME: a.acme(),
			LookupOwner: a.LookupOwner, OpenSite: a.openSite,
			Run:       a.runtimesHandlers().Run,
			Installed: func(runtime, version string) bool { return a.runtimesHandlers().List().Has(runtime, version) },
			Preflight: a.certHandlers().Preflight,
			KillUser:  sites.KillUser,
			NginxMu:   &a.nginxMu, CertMu: &a.certMu,
			Root: p.FunctionsRoot, Dir: p.FunctionsDir, LogDir: p.FunctionsLogDir,
			SystemdDir: p.SystemdDir, SitesDir: p.NginxSites,
			CertLiveDir: p.CertLiveDir, FallbackCert: p.FallbackCert, FallbackKey: p.FallbackKey, ACMEWebroot: p.ACMEWebroot,
			Log: a.log(),
		}
	})
	return a.functions
}
