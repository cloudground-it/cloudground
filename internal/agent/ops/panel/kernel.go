package panel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/install"
	"github.com/cloudground-it/cloudground/internal/install/managed"
)

// Kernel brings the installer's kernel settings file to this release's
// (SPEC §2.7): the self-update installs binaries and units, never the
// installer's files, so a release that changes a setting brings it in
// itself, as it does its packages.
type Kernel struct {
	Exec host.Exec
	// Path is managed.Sysctl in production; Want its content for this
	// release (KernelSettings).
	Path string
	Want []byte
	Log  *slog.Logger
}

// KernelSettings is this release's sysctl file, as the installer renders it.
func KernelSettings() ([]byte, error) {
	f, ok := managed.Lookup(managed.Sysctl)
	if !ok {
		return nil, fmt.Errorf("%s is not a managed file", managed.Sysctl)
	}
	return install.Render(f, install.Vars{})
}

// Ensure writes the file when it differs from Want and loads it, and
// reports whether it did. A file that already matches is left alone, and
// so are the running values: an operator's change at run time stays until
// the installer or the next release that changes the file (doctor reports
// it). sysctl runs as a transient root service: /proc/sys is read-only in
// the agent's sandbox (ProtectKernelTunables). A failed load is logged,
// never an error: a kernel that refuses a value must not stop the agent,
// as it does not stop the install.
func (k *Kernel) Ensure(ctx context.Context) (bool, error) {
	cur, err := rootfile.ReadNoFollow(k.Path)
	if err == nil && bytes.Equal(cur, k.Want) {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := rootfile.Replace(k.Path, k.Want, 0o644); err != nil {
		return false, err
	}
	args := []string{"--quiet", "--collect", "--wait", "--pipe", "--service-type=exec",
		fmt.Sprintf("--unit=cg-sysctl-%d", time.Now().UnixNano()),
		"--description=CloudGround: load the kernel settings", "-p", "RuntimeMaxSec=60",
		"--", "sysctl", "-p", k.Path}
	if _, err := k.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: args, Env: host.AptEnv[:2], Timeout: 30 * time.Second}); err != nil {
		k.log().Warn("the kernel settings were written but not loaded: they apply from the next boot", "file", k.Path, "fix", "sysctl -p "+k.Path, "err", err)
		return true, nil
	}
	k.log().Info("loaded the kernel settings this release needs", "file", k.Path)
	return true, nil
}

func (k *Kernel) log() *slog.Logger {
	if k.Log != nil {
		return k.Log
	}
	return slog.Default()
}
