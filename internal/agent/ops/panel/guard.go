package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
)

// Guard restarts the panel on a release the agent has swapped in, checks it
// came up, and otherwise puts the previous release back. It runs as its own
// transient unit (`panel-agent update-guard <plan>`), so it outlives the
// restarts of both services.
type Guard struct {
	Plan    Plan
	Systemd host.Systemd
	// Proc is where /proc/<pid>/exe is read. Empty: /proc.
	Proc string
	// Healthz asks the API's /healthz and returns its status code. Nil:
	// over Plan.APISocket.
	Healthz func(ctx context.Context) (int, error)
	// Timeout bounds each health wait (0: 60 s); Poll is its interval
	// (0: 250 ms).
	Timeout time.Duration
	Poll    time.Duration
	Log     *slog.Logger
	// Now stamps the result; nil means time.Now.
	Now func() time.Time
}

// Run applies the plan (SPEC §2.4): the panel is stopped, its state
// snapshotted, then started on the new release and checked. It returns nil
// when the new release is healthy; otherwise the previous release and the
// snapshot have been put back and the error says why.
func (g *Guard) Run(ctx context.Context) error {
	res := Result{Version: g.Plan.Version, Previous: g.Plan.Previous, Started: g.now().UTC().Format(time.RFC3339Nano)}
	before, err := g.mainPIDs(ctx)
	if err == nil {
		err = g.stopPanel(ctx)
	}
	var snap Snapshot
	if err == nil {
		snap, err = g.Plan.takeSnapshot(g.now())
		res.Snapshot = snap.Dir
	}
	if err != nil {
		// The new release never started: put the files back and start
		// the panel as it was.
		return g.abort(ctx, res, before, err)
	}
	err = g.startAndWait(ctx, before)
	if err == nil {
		res.OK = true
		if herr := recordHighest(g.Plan.StateDir, g.Plan.Version); herr != nil {
			g.log().Warn("could not record the installed release", "err", herr)
		}
		if perr := pruneSnapshots(g.Plan.SnapshotRoot, g.Plan.KeepSnapshots); perr != nil {
			g.log().Warn("could not prune old update snapshots", "err", perr)
		}
		g.log().Info("update healthy", "version", g.Plan.Version, "snapshot", snap.Dir)
		g.writeResult(res)
		return nil
	}
	g.log().Error("update unhealthy, rolling back", "version", g.Plan.Version, "err", err)
	res.Error, res.RolledBack = err.Error(), true
	failed, _ := g.mainPIDs(ctx)
	rerr := g.stopPanel(ctx)
	if ferr := g.Plan.restoreFiles(); ferr != nil {
		rerr = errors.Join(rerr, ferr)
	}
	if snap.Dir != "" {
		serr := g.Plan.restoreSnapshot(snap)
		res.StateRestored = serr == nil
		if serr != nil {
			rerr = errors.Join(rerr, fmt.Errorf("the state snapshot %s could not be put back: %w", snap.Dir, serr))
		}
	}
	if rerr == nil {
		rerr = g.startAndWait(ctx, failed)
	}
	res.RestoredHealthy = rerr == nil
	if rerr != nil {
		res.Error += "; the previous release did not come back: " + rerr.Error()
		g.log().Error("rollback failed", "err", rerr)
	} else {
		g.log().Info("previous release restored", "version", g.Plan.Previous, "snapshot", snap.Dir)
	}
	if perr := pruneSnapshots(g.Plan.SnapshotRoot, g.Plan.KeepSnapshots); perr != nil {
		g.log().Warn("could not prune old update snapshots", "err", perr)
	}
	g.writeResult(res)
	return fmt.Errorf("update to %s failed and was rolled back: %w", g.Plan.Version, err)
}

// abort undoes an update that failed before the new release started: the
// previous files go back and the panel starts on them.
func (g *Guard) abort(ctx context.Context, res Result, before map[string]uint32, cause error) error {
	g.log().Error("update aborted before the new release started", "version", g.Plan.Version, "err", cause)
	res.Error, res.RolledBack = cause.Error()+"; the update was not applied", true
	rerr := g.Plan.restoreFiles()
	if rerr == nil {
		rerr = g.startAndWait(ctx, before)
	}
	res.RestoredHealthy = rerr == nil
	if rerr != nil {
		res.Error += "; the previous release did not come back: " + rerr.Error()
	}
	g.writeResult(res)
	return fmt.Errorf("update to %s not applied: %w", g.Plan.Version, cause)
}

// mainPIDs records both services' main PIDs, to tell a restart happened.
func (g *Guard) mainPIDs(ctx context.Context) (map[string]uint32, error) {
	pids := map[string]uint32{}
	for _, u := range []string{AgentUnit, APIUnit} {
		p, err := g.Systemd.UnitProps(ctx, u)
		if err != nil {
			return nil, err
		}
		pids[u] = p.MainPID
	}
	return pids, nil
}

// stopPanel stops the API and its socket, then the agent, so nothing
// writes the state database or the agent's records while the snapshot is
// taken or put back, and no connection starts the API on the new binary
// before the snapshot. The socket goes first: stopping the service while
// the socket listens would let the next request start it again. The API
// drains its requests in flight (the update's own reply included) before it
// exits, and the agent then finishes the RPCs the API had made (an RPC the
// API stopped waiting for still runs to its end in the agent, and may write
// a record).
func (g *Guard) stopPanel(ctx context.Context) error {
	for _, u := range []string{APISocketUnit, APIUnit, AgentUnit} {
		if err := g.Systemd.Stop(ctx, u); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			return err
		}
	}
	return nil
}

// startAndWait starts both services on the binaries now in place (the
// agent first: the API's /healthz pings it) and waits until they are
// healthy on them.
func (g *Guard) startAndWait(ctx context.Context, before map[string]uint32) error {
	if g.Plan.unitsChanged() {
		if err := g.Systemd.DaemonReload(ctx); err != nil {
			return err
		}
	}
	for _, u := range []string{AgentUnit, APIUnit} {
		// A unit that crash-looped earlier must not refuse its restart.
		if err := g.Systemd.ResetFailed(ctx, u); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			g.log().Warn("reset-failed", "unit", u, "err", err)
		}
	}
	// The agent drains its requests in flight on SIGTERM before it stops.
	if err := g.Systemd.Restart(ctx, AgentUnit); err != nil {
		return err
	}
	if err := g.Systemd.Start(ctx, APISocketUnit); err != nil {
		return err
	}
	if err := g.Systemd.Start(ctx, APIUnit); err != nil {
		return err
	}
	return g.waitHealthy(ctx, before)
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Guard) waitHealthy(ctx context.Context, before map[string]uint32) error {
	timeout, poll := g.Timeout, g.Poll
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	var last error
	for {
		err := g.healthy(ctx, before)
		if err == nil {
			return nil
		}
		// The check cut short by the deadline says less than the one before.
		if ctx.Err() == nil || last == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy within %s: %w", timeout, last)
		case <-tick.C:
		}
	}
}

// healthy: both services restarted, each running the binary now at its live
// path (same inode), and the API's /healthz, which pings the agent, says 200.
func (g *Guard) healthy(ctx context.Context, before map[string]uint32) error {
	for unit, bin := range map[string]string{AgentUnit: "panel-agent", APIUnit: "panel-api"} {
		p, err := g.Systemd.UnitProps(ctx, unit)
		if err != nil {
			return err
		}
		if p.ActiveState != "active" || p.MainPID == 0 {
			return fmt.Errorf("%s is %s", unit, p.ActiveState)
		}
		if p.MainPID == before[unit] {
			return fmt.Errorf("%s has not restarted", unit)
		}
		proc := g.Proc
		if proc == "" {
			proc = "/proc"
		}
		same, err := sameFile(filepath.Join(proc, strconv.FormatUint(uint64(p.MainPID), 10), "exe"), filepath.Join(g.Plan.BinDir, bin))
		if err != nil {
			return fmt.Errorf("%s: %w", unit, err)
		}
		if !same {
			return fmt.Errorf("%s does not run %s", unit, filepath.Join(g.Plan.BinDir, bin))
		}
	}
	code, err := g.healthz(ctx)
	if err != nil {
		return fmt.Errorf("/healthz: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("/healthz answered %d", code)
	}
	return nil
}

func (g *Guard) healthz(ctx context.Context) (int, error) {
	if g.Healthz != nil {
		return g.Healthz(ctx)
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", g.Plan.APISocket)
			},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/healthz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (g *Guard) writeResult(r Result) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	r.Finished = now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(r, "", "  ")
	if err == nil {
		err = rootfile.Replace(filepath.Join(g.Plan.StateDir, ResultName), append(b, '\n'), 0o600)
	}
	if err != nil {
		g.log().Warn("could not record the update's result", "err", err)
	}
}

func (g *Guard) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

// GuardMain is `panel-agent update-guard <plan>`: it runs the guard with the
// real systemd and returns the process's exit status.
func GuardMain(args []string, log *slog.Logger) int {
	if len(args) != 1 {
		log.Error("usage: panel-agent update-guard <plan>")
		return 2
	}
	plan, err := ReadPlan(args[0])
	if err != nil {
		log.Error("cannot read the update plan", "err", err)
		return 2
	}
	g := &Guard{Plan: plan, Systemd: host.NewSystemd(), Log: log}
	// Two restarts and two health waits, with room for slow stops.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := g.Run(ctx); err != nil {
		log.Error("self-update failed", "err", err)
		return 1
	}
	return 0
}
