package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FakeSystemd is an in-memory Systemd. Units must be added with SetUnit
// before they can be started; an unknown unit behaves as systemd's does
// (ErrNoSuchUnit for jobs, LoadState "not-found" for UnitProps).
type FakeSystemd struct {
	mu    sync.Mutex
	units map[string]*UnitProps
	ops   []string
	// FailJob makes the next jobs on a unit complete with this result
	// ("failed", "timeout", …) instead of "done".
	FailJob map[string]string
	// Err, when set, is returned by every call (a lost bus).
	Err error
	// Now stamps ActiveEnter on start; nil means time.Now.
	Now func() time.Time
	pid uint32
	// transient holds each transient unit's argv.
	transient map[string][]string
	// OnRun plays a RunTransient: it may write to r.Output and returns how
	// the run ended. Nil: every run succeeds with no output.
	OnRun func(r TransientRun) TransientResult
	runs  []TransientRun
	// UnitDir, when set, is read on DaemonReload as systemd reads
	// /etc/systemd/system: a new *.service file there is loaded (inactive),
	// a unit whose file is gone is not-found (and forgotten once inactive),
	// and Slice= in the file or its drop-ins (<unit>.d/*.conf) puts the
	// unit in that slice.
	UnitDir string
	fromDir map[string]bool
	inSlice map[string]string // unit -> slice
}

// NewFakeSystemd returns an empty fake.
func NewFakeSystemd() *FakeSystemd {
	return &FakeSystemd{units: map[string]*UnitProps{}, FailJob: map[string]string{}, pid: 1000}
}

// SetUnit adds or replaces a unit. LoadState defaults to "loaded".
func (f *FakeSystemd) SetUnit(p UnitProps) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Name = UnitName(p.Name)
	if p.LoadState == "" {
		p.LoadState = "loaded"
	}
	f.units[p.Name] = &p
}

// Unit returns a unit's current properties.
func (f *FakeSystemd) Unit(name string) (UnitProps, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.units[UnitName(name)]
	if !ok {
		return UnitProps{}, false
	}
	return *u, true
}

// Ops lists every operation as "<op> <unit>", in order.
func (f *FakeSystemd) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// Did reports whether "<op> <unit>" was performed.
func (f *FakeSystemd) Did(op, unit string) bool {
	want := op + " " + UnitName(unit)
	for _, o := range f.Ops() {
		if o == want {
			return true
		}
	}
	return false
}

func (f *FakeSystemd) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *FakeSystemd) job(ctx context.Context, op, unit string, apply func(u *UnitProps)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unit = UnitName(unit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.ops = append(f.ops, op+" "+unit)
	u, ok := f.units[unit]
	// A unit whose file went away while it ran can still be stopped, as
	// with systemd; nothing else can be done to it.
	running := ok && u.ActiveState == "active"
	if !ok || u.LoadState == "not-found" && !(op == "stop" && running) {
		return fmt.Errorf("systemd %s %s: %w", op, unit, ErrNoSuchUnit)
	}
	if res := f.FailJob[unit]; res != "" {
		u.ActiveState, u.SubState, u.MainPID = "failed", "failed", 0
		return &JobError{Op: op, Unit: unit, Result: res}
	}
	apply(u)
	if u.LoadState == "not-found" && u.ActiveState != "active" {
		delete(f.units, unit)
		delete(f.inSlice, unit)
	}
	return nil
}

func (f *FakeSystemd) start(u *UnitProps) {
	f.pid++
	u.ActiveState, u.SubState, u.MainPID, u.ActiveEnter = "active", "running", f.pid, f.now()
}

func (f *FakeSystemd) Start(ctx context.Context, unit string) error {
	return f.job(ctx, "start", unit, func(u *UnitProps) {
		if u.ActiveState != "active" {
			f.start(u)
		}
	})
}

func (f *FakeSystemd) Stop(ctx context.Context, unit string) error {
	if strings.HasSuffix(unit, ".slice") {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.Err != nil {
			return f.Err
		}
		if _, ok := f.units[unit]; !ok {
			return f.stopSlice(unit)
		}
	}
	return f.job(ctx, "stop", unit, func(u *UnitProps) {
		u.ActiveState, u.SubState, u.MainPID = "inactive", "dead", 0
	})
}

func (f *FakeSystemd) Restart(ctx context.Context, unit string) error {
	return f.job(ctx, "restart", unit, f.start)
}

func (f *FakeSystemd) Reload(ctx context.Context, unit string) error {
	return f.job(ctx, "reload", unit, func(u *UnitProps) {})
}

func (f *FakeSystemd) ReloadOrRestart(ctx context.Context, unit string) error {
	return f.job(ctx, "reload-or-restart", unit, func(u *UnitProps) {
		if u.ActiveState != "active" {
			f.start(u)
		}
	})
}

func (f *FakeSystemd) fileOp(ctx context.Context, op, state string, units []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.setFileStateLocked(op, state, units); err != nil {
		return err
	}
	f.ops = append(f.ops, "daemon-reload")
	return f.loadUnitDir()
}

// setFileState is EnableUnitFiles/DisableUnitFiles alone, without the
// daemon-reload that follows (FlakyBus asks for it as its own call).
func (f *FakeSystemd) setFileState(ctx context.Context, op, state string, units []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setFileStateLocked(op, state, units)
}

func (f *FakeSystemd) setFileStateLocked(op, state string, units []string) error {
	if f.Err != nil {
		return f.Err
	}
	for _, name := range units {
		name = UnitName(name)
		f.ops = append(f.ops, op+" "+name)
		u, ok := f.units[name]
		if !ok {
			return fmt.Errorf("systemd %s %s: %w", op, name, ErrNoSuchUnit)
		}
		u.UnitFileState = state
	}
	return nil
}

func (f *FakeSystemd) Enable(ctx context.Context, units ...string) error {
	return f.fileOp(ctx, "enable", "enabled", units)
}

func (f *FakeSystemd) Disable(ctx context.Context, units ...string) error {
	return f.fileOp(ctx, "disable", "disabled", units)
}

func (f *FakeSystemd) DaemonReload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.ops = append(f.ops, "daemon-reload")
	return f.loadUnitDir()
}

// loadUnitDir applies UnitDir to the unit table. The caller holds mu.
func (f *FakeSystemd) loadUnitDir() error {
	if f.UnitDir == "" {
		return nil
	}
	ents, err := os.ReadDir(f.UnitDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if f.fromDir == nil {
		f.fromDir, f.inSlice = map[string]bool{}, map[string]string{}
	}
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !(strings.HasSuffix(name, ".service") || strings.HasSuffix(name, ".socket")) {
			continue
		}
		seen[name] = true
		if u, ok := f.units[name]; !ok {
			f.units[name] = &UnitProps{Name: name, LoadState: "loaded", ActiveState: "inactive", SubState: "dead"}
		} else {
			u.LoadState = "loaded"
		}
		f.fromDir[name] = true
		f.inSlice[name] = unitSlice(filepath.Join(f.UnitDir, name))
	}
	for name := range f.fromDir {
		if seen[name] {
			continue
		}
		delete(f.fromDir, name)
		if u, ok := f.units[name]; ok && u.ActiveState == "active" {
			// Still running, still in its slice, until stopped.
			u.LoadState = "not-found"
			continue
		}
		delete(f.units, name)
		delete(f.inSlice, name)
	}
	return nil
}

// unitSlice is the last Slice= in a unit file and its drop-ins.
func unitSlice(path string) string {
	files := []string{path}
	drop, _ := filepath.Glob(path + ".d/*.conf")
	sort.Strings(drop)
	slice := ""
	for _, p := range append(files, drop...) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Slice="); ok {
				slice = v
			}
		}
	}
	return slice
}

// stopSlice stops every unit in slice. A slice nothing is in is unknown,
// as an unloaded slice is to systemd. The caller holds mu.
func (f *FakeSystemd) stopSlice(slice string) error {
	f.ops = append(f.ops, "stop "+slice)
	found := false
	for unit, s := range f.inSlice {
		if s != slice {
			continue
		}
		found = true
		if u, ok := f.units[unit]; ok {
			u.ActiveState, u.SubState, u.MainPID = "inactive", "dead", 0
			if u.LoadState == "not-found" {
				delete(f.units, unit)
				delete(f.inSlice, unit)
			}
		}
	}
	for i := range f.runs {
		if f.runs[i].Slice == slice {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("systemd stop %s: %w", slice, ErrNoSuchUnit)
	}
	return nil
}

// SliceOf is the slice a unit loaded from UnitDir is in ("" for none).
func (f *FakeSystemd) SliceOf(unit string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inSlice[UnitName(unit)]
}

func (f *FakeSystemd) ResetFailed(ctx context.Context, unit string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unit = UnitName(unit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.ops = append(f.ops, "reset-failed "+unit)
	u, ok := f.units[unit]
	if !ok {
		return fmt.Errorf("systemd reset-failed %s: %w", unit, ErrNoSuchUnit)
	}
	if u.ActiveState == "failed" {
		u.ActiveState, u.SubState = "inactive", "dead"
	}
	// A not-found unit that is no longer failed is forgotten, as systemd
	// garbage-collects it.
	if u.LoadState == "not-found" {
		delete(f.units, unit)
	}
	return nil
}

func (f *FakeSystemd) UnitProps(ctx context.Context, unit string) (UnitProps, error) {
	if err := ctx.Err(); err != nil {
		return UnitProps{}, err
	}
	unit = UnitName(unit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return UnitProps{}, f.Err
	}
	u, ok := f.units[unit]
	if !ok {
		return UnitProps{Name: unit, LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}, nil
	}
	return *u, nil
}

func (f *FakeSystemd) ListUnits(ctx context.Context, patterns ...string) ([]UnitState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var out []UnitState
	for name, u := range f.units {
		match := len(patterns) == 0
		for _, p := range patterns {
			if ok, _ := path.Match(p, name); ok {
				match = true
			}
		}
		if match {
			out = append(out, UnitState{Name: name, LoadState: u.LoadState, ActiveState: u.ActiveState, SubState: u.SubState})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// StartTransient creates the unit, active, and remembers its argv. A unit of
// that name that is still active is ErrUnitExists, as with systemd.
func (f *FakeSystemd) StartTransient(ctx context.Context, unit, description string, argv []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkTransient(unit, argv); err != nil {
		return err
	}
	unit = UnitName(unit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.ops = append(f.ops, "start-transient "+unit)
	if u, ok := f.units[unit]; ok && u.ActiveState == "active" {
		return fmt.Errorf("systemd start-transient %s: %w", unit, ErrUnitExists)
	}
	if res := f.FailJob[unit]; res != "" {
		return &JobError{Op: "start-transient", Unit: unit, Result: res}
	}
	u := &UnitProps{Name: unit, LoadState: "loaded"}
	f.start(u)
	f.units[unit] = u
	if f.transient == nil {
		f.transient = map[string][]string{}
	}
	f.transient[unit] = append([]string(nil), argv...)
	return nil
}

// TransientArgv returns the argv a transient unit was started with.
func (f *FakeSystemd) TransientArgv(unit string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv, ok := f.transient[UnitName(unit)]
	return argv, ok
}

// RunTransient validates r as the real adapter does, refuses a unit that is
// running (ErrUnitExists), and plays the run through OnRun. A unit marked to
// fail with FailJob never starts, as when its user or directory is missing.
func (f *FakeSystemd) RunTransient(ctx context.Context, r TransientRun) (TransientResult, error) {
	if err := ctx.Err(); err != nil {
		return TransientResult{}, err
	}
	if err := checkRun(r); err != nil {
		return TransientResult{}, err
	}
	unit := UnitName(r.Unit)
	f.mu.Lock()
	if f.Err != nil {
		f.mu.Unlock()
		return TransientResult{}, f.Err
	}
	f.ops = append(f.ops, "run-transient "+unit)
	if u, ok := f.units[unit]; ok && u.ActiveState == "active" {
		f.mu.Unlock()
		return TransientResult{}, fmt.Errorf("systemd run-transient %s: %w", unit, ErrUnitExists)
	}
	if res := f.FailJob[unit]; res != "" {
		f.mu.Unlock()
		return TransientResult{}, &JobError{Op: "run-transient", Unit: unit, Result: res}
	}
	r.Argv = append([]string(nil), r.Argv...)
	r.Env = append([]string(nil), r.Env...)
	f.runs = append(f.runs, r)
	play := f.OnRun
	f.mu.Unlock()
	if play == nil {
		return TransientResult{Result: "success"}, nil
	}
	return play(r), nil
}

// Runs lists every TransientRun played, in order.
func (f *FakeSystemd) Runs() []TransientRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TransientRun(nil), f.runs...)
}

var _ Systemd = (*FakeSystemd)(nil)
var _ Systemd = (*DBusSystemd)(nil)
