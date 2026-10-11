package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agentpanel "github.com/cloudground-it/cloudground/internal/agent/ops/panel"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/version"
)

// `cgctl update` (internal/install/SPEC.md §10): the panel's self-update
// from the server's shell. It asks the agent for the same SelfUpdate the
// panel's button asks for, so both paths verify, snapshot, restart and roll
// back with one implementation, and then waits for the guard's verdict.

const updateUsage = `Usage:
  cgctl update [--to <vX.Y.Z>] [--channel stable|beta] [--check] [--timeout <duration>]

Updates CloudGround on this server to the newest signed release of the
channel the panel follows (or --to that release), as the panel's update
button does: the release is verified against the keys built into the
installed CloudGround, a release older than one installed here is refused,
the panel's state is snapshotted, and a release that does not come up healthy
is rolled back with its snapshot. Site data is never touched.

  --check    say what would be installed, change nothing
  --to       a release other than the newest (never an older one)
  --channel  the channel to take the newest release from (default: the panel's setting)
  --timeout  how long to wait for the restart's verdict (default 10m)
`

type updateFlags struct {
	to, channel string
	check       bool
	timeout     time.Duration
}

func parseUpdateFlags(args []string) (updateFlags, error) {
	f := updateFlags{timeout: 10 * time.Minute}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if i+1 >= len(args) || args[i+1] == "" {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "--check":
			f.check = true
		case "--to":
			f.to, err = val()
		case "--channel":
			f.channel, err = val()
		case "--timeout":
			var v string
			if v, err = val(); err == nil {
				f.timeout, err = time.ParseDuration(v)
				if err == nil && f.timeout <= 0 {
					err = errors.New("--timeout must be positive")
				}
			}
		case "-h", "--help", "help":
			return f, errHelp
		default:
			return f, fmt.Errorf("update: unknown argument %q", a)
		}
		if err != nil {
			return f, err
		}
	}
	if f.to != "" {
		if !strings.HasPrefix(f.to, "v") {
			f.to = "v" + f.to
		}
		if _, ok := version.ReleaseTag(f.to); !ok {
			return f, fmt.Errorf("--to %s is not a release tag (vX.Y.Z or vX.Y.Z-beta.N)", f.to)
		}
	}
	if f.channel != "" {
		if _, ok := version.ParseChannel(f.channel); !ok {
			return f, fmt.Errorf("--channel must be stable or beta")
		}
	}
	return f, nil
}

// releaseSource is the release feed (version.Feed in production).
type releaseSource interface {
	Latest(ctx context.Context, channel string) (string, error)
	FetchManifest(ctx context.Context, tag string) (version.Manifest, error)
}

// updater is `cgctl update` over its dependencies, so tests replace them.
type updater struct {
	Agent     agentrpc.Caller
	Feed      releaseSource
	StateDir  string // /var/lib/cloudground-agent
	StateDB   string // the panel's state database, read for the channel and the jobs
	Now       func() time.Time
	Sleep     func(time.Duration)
	Out, Err  io.Writer
	CanUpdate func() error
}

func runUpdate(args []string, stdout, stderr io.Writer) int {
	f, err := parseUpdateFlags(args)
	if err != nil {
		if err != errHelp {
			fmt.Fprintln(stderr, "cgctl:", err)
		}
		fmt.Fprint(stderr, updateUsage)
		return exitUsage
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "cgctl: update runs as root, on the server: sudo cgctl update")
		return exitUsage
	}
	token, err := os.ReadFile("/etc/cloudground/agent.token")
	if err != nil {
		fmt.Fprintln(stderr, "cgctl: cannot read the agent token:", err)
		return exitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	u := &updater{
		Agent:     agentrpc.NewClient("/run/cloudground/agent.sock", strings.TrimSpace(string(token)), nil),
		Feed:      version.Feed{Client: updateHTTPClient()},
		StateDir:  "/var/lib/cloudground-agent",
		StateDB:   defaultStateDB,
		Now:       time.Now,
		Sleep:     time.Sleep,
		Out:       stdout,
		Err:       stderr,
		CanUpdate: version.CanUpdate,
	}
	return u.run(ctx, f)
}

func (u *updater) run(ctx context.Context, f updateFlags) int {
	failf := func(format string, args ...any) int {
		fmt.Fprintf(u.Err, "cgctl: "+format+"\n", args...)
		return exitFailed
	}
	if err := u.CanUpdate(); err != nil {
		return failf("this build cannot update itself: %v", err)
	}
	installed, err := u.installed(ctx)
	if err != nil {
		return failf("the agent does not answer (%v): start it with `systemctl start cloudground-agent`, or run the installer of the release you want", err)
	}
	channel := f.channel
	if channel == "" {
		channel = u.panelChannel()
	}
	target := f.to
	if target == "" {
		if target, err = u.Feed.Latest(ctx, channel); err != nil {
			return failf("find the newest %s release: %v", channel, err)
		}
	}
	fmt.Fprintf(u.Out, "installed: %s\nchannel:   %s\nrelease:   %s\n", installed, channel, target)
	// The agent makes every one of these checks again; making them here
	// first gives the reason before anything is downloaded or stopped.
	m, err := u.Feed.FetchManifest(ctx, target)
	if err != nil {
		return failf("release %s: %v", target, err)
	}
	if err := u.refusal(m, target, installed, channel); err != nil {
		if errors.Is(err, errUpToDate) {
			fmt.Fprintf(u.Out, "up to date: %s is installed\n", m.Version)
			return exitOK
		}
		return failf("%v", err)
	}
	if f.check {
		fmt.Fprintf(u.Out, "update available: %s -> %s (signature verified); run `cgctl update` to install it\n", installed, m.Version)
		return exitOK
	}
	if err := runningBlockers(u.StateDB); err != nil {
		return failf("not updating: %v", err)
	}

	start := u.Now()
	res, err := agentrpc.Call(ctx, u.Agent, agentrpc.SelfUpdate, rpc.SelfUpdateParams{Version: m.Version, Channel: channel})
	if err != nil {
		return failf("the update was not started: %v", err)
	}
	fmt.Fprintf(u.Out, "verified and staged %s; restarting the panel (journalctl -u %s -f shows the guard)\n", res.UpdatedTo, agentpanel.GuardUnit)
	r, err := u.waitResult(ctx, start, f.timeout)
	if err != nil {
		return failf("%v", err)
	}
	if r.OK {
		fmt.Fprintf(u.Out, "updated: %s -> %s\n", r.Previous, r.Version)
		if r.Snapshot != "" {
			fmt.Fprintf(u.Out, "state snapshot taken before the update: %s\n", r.Snapshot)
		}
		return exitOK
	}
	fmt.Fprintf(u.Err, "cgctl: the update to %s failed: %s\n", r.Version, r.Error)
	switch {
	case r.RestoredHealthy:
		fmt.Fprintf(u.Err, "cgctl: rolled back: %s runs again", r.Previous)
		if r.StateRestored {
			fmt.Fprintf(u.Err, ", with its state from %s", r.Snapshot)
		}
		fmt.Fprintln(u.Err)
	default:
		fmt.Fprintf(u.Err, "cgctl: the previous release %s did not come back healthy either: see journalctl -u %s and -u cloudground-agent; "+
			"`cgctl install` re-converges the installed release\n", r.Previous, agentpanel.GuardUnit)
	}
	return exitFailed
}

var errUpToDate = errors.New("up to date")

// updateHTTPClient reads the feed and the manifests: bounded in time, and
// never redirected off https.
func updateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Scheme)
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// refusal is why the signed manifest m may not replace installed, or nil.
func (u *updater) refusal(m version.Manifest, target, installed, channel string) error {
	if err := version.CheckChannel(channel, m.Version); err != nil {
		return err
	}
	if c, ok := version.Compare(m.Version, installed); ok && c <= 0 {
		if c == 0 {
			return errUpToDate
		}
		return fmt.Errorf("release %s is older than %s, installed: refusing to downgrade", m.Version, installed)
	}
	if err := version.CheckUpdate(m.Version, target, installed); err != nil {
		return err
	}
	if high := agentpanel.HighestInstalled(u.StateDir); high != "" && !version.Newer(m.Version, high) {
		if m.Version == high {
			return errUpToDate
		}
		return fmt.Errorf("release %s is not newer than %s, already installed here: refusing to downgrade", m.Version, high)
	}
	return nil
}

// installed is the running agent's version.
func (u *updater) installed(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := agentrpc.Call(ctx, u.Agent, agentrpc.Ping, agentrpc.None{})
	if err != nil {
		return "", err
	}
	return res["version"], nil
}

// panelChannel is the panel's update_channel, stable when it cannot be read.
func (u *updater) panelChannel() string {
	p, err := openStatePeek(u.StateDB)
	if err != nil {
		fmt.Fprintf(u.Err, "cgctl: cannot read the panel's channel (%v): using stable\n", err)
		return version.Stable
	}
	defer p.Close()
	ch, err := p.channel()
	if err != nil || ch == "" {
		return version.Stable
	}
	return ch
}

// waitResult waits for the guard's verdict on the update started at start.
func (u *updater) waitResult(ctx context.Context, start time.Time, timeout time.Duration) (agentpanel.Result, error) {
	deadline := start.Add(timeout)
	for {
		r, err := agentpanel.ReadResult(u.StateDir)
		if err == nil && r.Finished != "" {
			if at, perr := time.Parse(time.RFC3339Nano, r.Started); perr == nil && !at.Before(start.Add(-time.Second)) {
				return r, nil
			}
		}
		if ctx.Err() != nil {
			return agentpanel.Result{}, fmt.Errorf("stopped waiting; the guard goes on: see %s", filepath.Join(u.StateDir, agentpanel.ResultName))
		}
		if !u.Now().Before(deadline) {
			return agentpanel.Result{}, fmt.Errorf("no verdict within %s: see journalctl -u %s and %s", timeout, agentpanel.GuardUnit, filepath.Join(u.StateDir, agentpanel.ResultName))
		}
		u.Sleep(time.Second)
	}
}
