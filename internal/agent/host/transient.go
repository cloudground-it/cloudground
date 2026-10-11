package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// TransientRun is a command run to completion as an unprivileged account, in
// a transient service of its own: systemd starts it, bounds its run time and,
// when it ends or overruns, kills every process it left in its cgroup.
type TransientRun struct {
	Unit        string // a .service name (".service" is appended to a bare one)
	Description string
	Argv        []string // argv[0] absolute
	User        string   // the account it runs as (User=); required
	Dir         string   // working directory, absolute; required
	// Env is added to the variables systemd sets itself for User= (HOME,
	// USER, LOGNAME, SHELL). Nothing of the agent's environment is passed.
	Env []string
	// MaxRuntime is RuntimeMaxSec: past it systemd stops the unit, which
	// kills its whole cgroup (SIGTERM, then SIGKILL after StopGrace).
	MaxRuntime time.Duration
	// Output receives stdout and stderr, interleaved, through a pipe whose
	// write end only the unit holds. Nil discards them.
	Output io.Writer
	// Slice places the unit in a slice (Slice=), so that stopping the slice
	// stops it with everything else there. Empty leaves systemd's default.
	Slice string
}

// ValidSlice reports whether name is a slice unit name the agent may hand
// to systemd: letters, digits, '-', '_' and '.', ending in ".slice", never
// starting with '-'.
func ValidSlice(name string) bool {
	base, ok := strings.CutSuffix(name, ".slice")
	if !ok || base == "" || base[0] == '-' {
		return false
	}
	for _, c := range base {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// TransientResult is how a TransientRun ended.
type TransientResult struct {
	// Result is systemd's service result: success, exit-code, signal,
	// core-dump, timeout (MaxRuntime ran out), …
	Result string
	// Status is the main process's exit status when it exited, or the
	// number of the signal that killed it.
	Status int
}

// OK reports a run that exited 0.
func (r TransientResult) OK() bool { return r.Result == "success" }

// StopGrace is how long a stopped run gets between SIGTERM and SIGKILL
// (TimeoutStopSec). systemd's default is 90 s.
const StopGrace = 10 * time.Second

// endPoll is the interval at which RunTransient reads the unit's state once
// its output has closed, until systemd has recorded the end (normally the
// first read).
const endPoll = 50 * time.Millisecond

func checkRun(r TransientRun) error {
	if err := checkTransient(r.Unit, r.Argv); err != nil {
		return err
	}
	if !ValidAccountName(r.User) {
		return fmt.Errorf("transient unit %s: user %q: %w", r.Unit, r.User, ErrInvalidName)
	}
	if !filepath.IsAbs(r.Dir) || filepath.Clean(r.Dir) != r.Dir {
		return fmt.Errorf("transient unit %s: working directory must be absolute and clean: %w", r.Unit, ErrInvalidArgument)
	}
	if r.Slice != "" && !ValidSlice(r.Slice) {
		return fmt.Errorf("transient unit %s: slice %q: %w", r.Unit, r.Slice, ErrInvalidArgument)
	}
	if r.MaxRuntime <= 0 {
		return fmt.Errorf("transient unit %s: a run time limit is required: %w", r.Unit, ErrInvalidArgument)
	}
	for _, e := range r.Env {
		if i := strings.IndexByte(e, '='); i <= 0 || strings.ContainsAny(e, "\x00\n") {
			return fmt.Errorf("transient unit %s: bad environment entry: %w", r.Unit, ErrInvalidArgument)
		}
	}
	return nil
}

// RunTransient starts r and returns when it has ended, with its output
// copied to r.Output. A unit of that name still running is ErrUnitExists; a
// failed one left by an earlier run is reset and replaced. Cancelling ctx
// stops the unit (its whole cgroup) and returns ctx's error.
func (s *DBusSystemd) RunTransient(ctx context.Context, r TransientRun) (TransientResult, error) {
	if err := checkRun(r); err != nil {
		return TransientResult{}, err
	}
	unit := UnitName(r.Unit)
	pr, pw, err := os.Pipe()
	if err != nil {
		return TransientResult{}, err
	}
	defer pr.Close()
	env := append([]string{}, r.Env...)
	// The write end goes to systemd as a file descriptor (what
	// `systemd-run --pipe` does): no file on disk, and EOF on the read end
	// once every process of the unit has gone.
	fd := godbus.UnixFD(pw.Fd())
	props := []sddbus.Property{
		sddbus.PropDescription(r.Description),
		sddbus.PropType("exec"),
		// go-systemd's flag is ExecStart's third field, which systemd reads
		// as "ignore failure": true made an exit 3 "Deactivated
		// successfully" (measured on systemd 255 and 259).
		sddbus.PropExecStart(r.Argv, false),
		{Name: "User", Value: godbus.MakeVariant(r.User)},
		{Name: "WorkingDirectory", Value: godbus.MakeVariant(r.Dir)},
		{Name: "Environment", Value: godbus.MakeVariant(env)},
		{Name: "RuntimeMaxUSec", Value: godbus.MakeVariant(uint64(r.MaxRuntime / time.Microsecond))},
		{Name: "TimeoutStopUSec", Value: godbus.MakeVariant(uint64(StopGrace / time.Microsecond))},
		{Name: "StandardInput", Value: godbus.MakeVariant("null")},
		{Name: "StandardOutputFileDescriptor", Value: godbus.MakeVariant(fd)},
		{Name: "StandardErrorFileDescriptor", Value: godbus.MakeVariant(fd)},
		// Kept when it fails, so its result can be read; collected as soon
		// as it ends cleanly.
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive")},
	}
	if r.Slice != "" {
		props = append(props, sddbus.Property{Name: "Slice", Value: godbus.MakeVariant(r.Slice)})
	}
	start := func() error {
		return s.job(ctx, "run-transient", unit, false, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
			return func(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
				return c.StartTransientUnitContext(ctx, name, mode, props, ch)
			}
		})
	}
	err = start()
	if errors.Is(err, ErrUnitExists) {
		if p, perr := s.UnitProps(ctx, unit); perr == nil && p.ActiveState == "failed" {
			if rerr := s.ResetFailed(ctx, unit); rerr == nil {
				err = start()
			}
		}
	}
	// systemd holds its own copy now; ours must go, or EOF never comes.
	pw.Close()
	runtime.KeepAlive(pw)
	if err != nil {
		var je *JobError
		if errors.As(err, &je) {
			// It never ran (no such user, no such directory): drop the
			// failed unit so the next run can take the name.
			_ = s.ResetFailed(context.WithoutCancel(ctx), unit)
		}
		return TransientResult{}, err
	}

	out := r.Output
	if out == nil {
		out = io.Discard
	}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, pr)
		close(copied)
	}()
	select {
	case <-copied:
	case <-ctx.Done():
		s.stopRun(ctx, unit)
		pr.Close()
		<-copied
		return TransientResult{}, fmt.Errorf("systemd run %s: %w", unit, ctx.Err())
	}
	return s.waitEnd(ctx, unit)
}

// stopRun stops a run its caller abandoned. The stop job gets its own
// deadline: the caller's has already passed.
func (s *DBusSystemd) stopRun(ctx context.Context, unit string) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopGrace+30*time.Second)
	defer cancel()
	_ = s.Stop(sctx, unit)
	_ = s.ResetFailed(sctx, unit)
}

// waitEnd reads the unit's state until systemd has recorded its end. The
// output pipe closes when the last process of the unit exits, so this is
// normally one read; it keeps reading while a process that closed its
// output runs on (until MaxRuntime stops it).
func (s *DBusSystemd) waitEnd(ctx context.Context, unit string) (TransientResult, error) {
	t := time.NewTicker(endPoll)
	defer t.Stop()
	for {
		res, done, err := s.endState(ctx, unit)
		if err != nil || done {
			return res, err
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			s.stopRun(ctx, unit)
			return TransientResult{}, fmt.Errorf("systemd run %s: %w", unit, ctx.Err())
		}
	}
}

// endState reads the unit's end once. A read is idempotent, and so is the
// reset of a failed unit: a lost connection is retried on a fresh one.
func (s *DBusSystemd) endState(ctx context.Context, unit string) (res TransientResult, done bool, err error) {
	err = s.call(ctx, "show", unit, true, func(ctx context.Context, c dbusConn) error {
		var cerr error
		res, done, cerr = endStateOn(ctx, c, unit)
		return cerr
	})
	return res, done, err
}

func endStateOn(ctx context.Context, c dbusConn, unit string) (TransientResult, bool, error) {
	props, err := c.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return TransientResult{}, false, classifyDBus("show", unit, err)
	}
	if ls := propString(props, "LoadState"); ls == "" || ls == "not-found" {
		// Collected: with CollectMode=inactive only a clean end is.
		return TransientResult{Result: "success"}, true, nil
	}
	switch propString(props, "ActiveState") {
	case "inactive", "failed":
	default:
		return TransientResult{}, false, nil
	}
	svc, err := c.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err != nil {
		return TransientResult{}, false, classifyDBus("show", unit, err)
	}
	st, _ := svc["ExecMainStatus"].(int32)
	res := TransientResult{Result: propString(svc, "Result"), Status: int(st)}
	if res.Result == "" {
		res.Result = "success"
	}
	if propString(props, "ActiveState") == "failed" {
		if err := c.ResetFailedUnitContext(ctx, unit); err != nil {
			return res, true, classifyDBus("reset-failed", unit, err)
		}
	}
	return res, true, nil
}
