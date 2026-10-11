package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Invocation is one wp-cli command for one site.
type Invocation struct {
	User string   // the site's Unix user
	Path string   // the WordPress root (--path)
	Args []string // wp-cli arguments
	// Stdin is fed to wp-cli (a secret read with --prompt=<field>), with a
	// trailing newline added. Empty means no stdin.
	Stdin string
	// Slice and Sandbox are required: wp-cli runs the site's code (its
	// plugins, its theme, wp-config.php), so it runs as the site's PHP does,
	// in a transient service in the site's slice with the site's sandbox
	// (Sandbox: the PHP master's unit lines, phpfpm.Sandbox).
	Slice   string
	Sandbox []string
}

// WPCLI runs wp-cli as a site's user.
type WPCLI interface {
	Run(ctx context.Context, inv Invocation) ([]byte, error)
}

// WPCLIRunner is the real WPCLI.
type WPCLIRunner struct {
	Exec Exec
	// Env is the environment wp-cli gets; nil means WPEnv.
	Env []string
}

// WPEnv is the explicit, minimal environment wp-cli runs with: nothing of
// the agent's own (tokens, cache paths) reaches code the site controls.
// systemd sets HOME, USER, LOGNAME and SHELL for the site user; wp-cli's
// cache goes to the run's private /tmp (the site root, its home, is
// root's).
var WPEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "WP_CLI_CACHE_DIR=/tmp/.wp-cli-cache"}

// WPMaxRuntime bounds one wp-cli run (an update may take long).
const WPMaxRuntime = 50 * time.Minute

// NewWPCLI returns the real adapter.
func NewWPCLI(e Exec) *WPCLIRunner { return &WPCLIRunner{Exec: e} }

// WPArgs is wp-cli's argv for inv.
func WPArgs(inv Invocation) []string {
	return append([]string{"wp", "--path=" + inv.Path}, inv.Args...)
}

// ScopeArgs is the systemd-run argv (after the binary, before the command)
// that runs a command in a transient scope of slice. Nothing of the agent
// runs site code this way any more (a scope keeps the caller's mounts);
// the slice integration test still uses it.
func ScopeArgs(slice string) []string {
	return []string{"--scope", "--quiet", "--collect", "--slice=" + slice, "--"}
}

var wpRuns atomic.Uint64

// WPService is the transient service a wp-cli invocation runs in.
func WPService(inv Invocation, env []string) Service {
	unit := fmt.Sprintf("cg-wp-%s-%d-%d.service", inv.User, time.Now().UnixNano(), wpRuns.Add(1))
	return Service{
		Unit: unit, Description: "wp-cli for " + inv.User,
		User: inv.User, Slice: inv.Slice, Dir: inv.Path,
		Env: env, Props: inv.Sandbox, MaxRuntime: WPMaxRuntime, Pipe: true,
		Argv: WPArgs(inv),
	}
}

func (w *WPCLIRunner) Run(ctx context.Context, inv Invocation) ([]byte, error) {
	if !ValidAccountName(inv.User) {
		return nil, fmt.Errorf("wp-cli user %q: %w", inv.User, ErrInvalidName)
	}
	if !filepath.IsAbs(inv.Path) || filepath.Clean(inv.Path) != inv.Path {
		return nil, fmt.Errorf("wp-cli path %q: %w", inv.Path, ErrInvalidArgument)
	}
	if strings.ContainsRune(inv.Stdin, '\n') {
		return nil, errors.New("wp-cli: a stdin secret cannot contain a newline")
	}
	if !ValidSlice(inv.Slice) || len(inv.Sandbox) == 0 {
		return nil, fmt.Errorf("wp-cli for %s: a slice and the site's sandbox are required: %w", inv.User, ErrInvalidArgument)
	}
	env := w.Env
	if env == nil {
		env = WPEnv
	}
	svc := WPService(inv, env)
	c := Cmd{Name: "systemd-run", Args: ServiceArgs(svc), Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}}
	if inv.Stdin != "" {
		c.Stdin = strings.NewReader(inv.Stdin + "\n")
	}
	res, err := w.Exec.Run(ctx, c)
	if err != nil && (ctx.Err() != nil || errors.Is(err, ErrStdoutTooLarge)) {
		// systemd-run was killed, with the request or for printing past
		// the limit; the service is not its child and would run on.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = w.Exec.Run(sctx, Cmd{Name: "systemctl", Args: []string{"stop", svc.Unit}})
		cancel()
	}
	return res.Stdout, err
}

var _ WPCLI = (*WPCLIRunner)(nil)
