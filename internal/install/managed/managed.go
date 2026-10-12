// Package managed is the one list of configuration files the installer
// owns (internal/install/SPEC.md §7). The installer renders exactly these,
// `cgctl doctor` checks exactly these, and the agent's drift check accepts
// these paths besides the ones it writes itself. Secrets (the agent token,
// the certificates) are created once and never compared, so they are not here.
package managed

import "io/fs"

// How the installer writes a file.
const (
	// Rendered: the whole file comes from a template; any difference is drift.
	Rendered = "rendered"
	// Edited: the installer edits a file it does not own (the distribution's
	// nginx.conf, the shared panel.env) and checks only its own part.
	Edited = "edited"
)

// File is one managed file.
type File struct {
	Path  string
	Mode  fs.FileMode
	Owner string // user name; "" = root
	Group string // group name; "" = root
	Kind  string // Rendered or Edited
	// Template names the file that renders it: "templates/<name>" under
	// internal/install/templates, or "units/<name>" for a unit of
	// internal/units ("" for an edited file).
	Template string
}

// The paths, for the steps that name them.
const (
	Sysctl      = "/etc/sysctl.d/60-cloudground.conf"
	NginxConf   = "/etc/nginx/nginx.conf"
	Vhost       = "/etc/nginx/sites-enabled/cloudground-panel-bootstrap.conf"
	SSHD        = "/etc/ssh/sshd_config.d/10-cloudground-sftp.conf"
	Logrotate   = "/etc/logrotate.d/cloudground"
	Needrestart = "/etc/needrestart/conf.d/cloudground.conf"
	RenewHook   = "/etc/letsencrypt/renewal-hooks/deploy/cloudground-reload-nginx.sh"
	PanelEnv    = "/etc/cloudground/panel.env"
	AgentUnit   = "/etc/systemd/system/cloudground-agent.service"
	APIUnit     = "/etc/systemd/system/cloudground-api.service"
	SocketUnit  = "/etc/systemd/system/cloudground-api.socket"
)

// Files is the list, in the order the installer writes them.
var Files = []File{
	{Path: PanelEnv, Mode: 0o640, Group: "cloudground", Kind: Edited},
	{Path: Sysctl, Mode: 0o644, Kind: Rendered, Template: "templates/sysctl.conf"},
	{Path: NginxConf, Mode: 0o644, Kind: Edited},
	{Path: Vhost, Mode: 0o644, Kind: Rendered, Template: "templates/panel-bootstrap.conf"},
	{Path: AgentUnit, Mode: 0o644, Kind: Rendered, Template: "units/cloudground-agent.service"},
	{Path: APIUnit, Mode: 0o644, Kind: Rendered, Template: "units/cloudground-api.service"},
	{Path: SocketUnit, Mode: 0o644, Kind: Rendered, Template: "units/cloudground-api.socket"},
	{Path: SSHD, Mode: 0o644, Kind: Rendered, Template: "templates/sshd-sftp.conf"},
	{Path: Logrotate, Mode: 0o644, Kind: Rendered, Template: "templates/logrotate.conf"},
	{Path: Needrestart, Mode: 0o644, Kind: Rendered, Template: "templates/needrestart.conf"},
	{Path: RenewHook, Mode: 0o755, Kind: Rendered, Template: "templates/renew-hook.sh"},
}

// Lookup returns the entry for path.
func Lookup(path string) (File, bool) {
	for _, f := range Files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

// Has reports whether path is one of the installer's files.
func Has(path string) bool {
	_, ok := Lookup(path)
	return ok
}
