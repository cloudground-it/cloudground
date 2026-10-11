package agent

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/agent/ops/files"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sftp"
)

// filesHandlers returns the agent's ops/files handlers (the file manager).
func (a *Agent) filesHandlers() *files.Ops {
	a.filesOnce.Do(func() {
		a.filesOps = &files.Ops{
			Open: a.openSite,
		}
	})
	return a.filesOps
}

// sftpHandlers returns the agent's ops/sftp handlers, built once: they keep
// the per-site key locks and whether the deny group exists.
func (a *Agent) sftpHandlers() *sftp.Ops {
	a.sftpOnce.Do(func() {
		paths := sftp.DefaultShellPaths()
		paths.BinDir = a.Paths.BinDir
		paths.SystemdDir = a.Paths.SystemdDir
		sd, _ := a.Systemd.(sftp.ShellSystemd)
		a.sftpOps = &sftp.Ops{
			Accounts:   a.Accounts,
			Open:       a.openSite,
			Port:       a.sshPort,
			Systemd:    sd,
			Exec:       a.exec(),
			ShellPaths: paths,
			Log:        a.log(),
		}
	})
	return a.sftpOps
}

// ServeShellGate serves the site shells' gate socket until ctx ends
// (ops/sftp SPEC §5.4).
func (a *Agent) ServeShellGate(ctx context.Context) error {
	return a.sftpHandlers().ServeGate(ctx)
}
