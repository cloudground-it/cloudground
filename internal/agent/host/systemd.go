package host

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// ErrNoSuchUnit is a unit systemd does not know.
var ErrNoSuchUnit = errors.New("no such systemd unit")

// ErrUnitExists is a transient unit whose name is already taken by a unit
// systemd still has loaded (one that is running, typically).
var ErrUnitExists = errors.New("systemd unit already exists")

// ErrJobFailed matches every *JobError.
var ErrJobFailed = errors.New("systemd job failed")

// ErrConnLost is a call or a job wait cut short because the connection to
// systemd dropped under it. The job may or may not have run.
var ErrConnLost = errors.New("connection to systemd lost")

// ErrJobTimeout matches every *JobTimeoutError.
var ErrJobTimeout = errors.New("systemd job did not finish in time")

// JobError is a job that completed with a result other than done/skipped.
type JobError struct {
	Op, Unit, Result string
}

func (e *JobError) Error() string {
	return fmt.Sprintf("systemd %s %s: job %s", e.Op, e.Unit, e.Result)
}

// Is makes errors.Is(err, ErrJobFailed) true.
func (e *JobError) Is(target error) bool { return target == ErrJobFailed }

// JobTimeoutError is a job whose result did not come before the deadline
// (the caller's or the adapter's ceiling). systemd goes on with the job;
// State is the unit as it was read when the wait gave up, so the caller
// can tell a slow start ("activating") from a dead one.
type JobTimeoutError struct {
	Op, Unit string
	Waited   time.Duration
	State    UnitProps // zero when it could not be read (StateErr)
	StateErr error
}

func (e *JobTimeoutError) Error() string {
	st := "unknown"
	if e.StateErr == nil {
		st = e.State.ActiveState + "/" + e.State.SubState
	}
	return fmt.Sprintf("systemd %s %s: no job result after %s (unit %s)", e.Op, e.Unit, e.Waited.Round(time.Millisecond), st)
}

// Is makes errors.Is true for ErrJobTimeout and context.DeadlineExceeded.
func (e *JobTimeoutError) Is(target error) bool {
	return target == ErrJobTimeout || target == context.DeadlineExceeded
}

// UnitProps is what the agent reads about a unit.
type UnitProps struct {
	Name          string
	LoadState     string // loaded | not-found | masked | error …
	ActiveState   string // active | inactive | failed | activating | deactivating | reloading
	SubState      string
	UnitFileState string // enabled | disabled | static | masked | "" …
	MainPID       uint32
	MemoryCurrent uint64
	MemoryKnown   bool      // false when systemd does not account it
	ActiveEnter   time.Time // zero if the unit never became active
}

// Exists is false for a unit systemd could not find.
func (p UnitProps) Exists() bool { return p.LoadState != "" && p.LoadState != "not-found" }

// UnitState is one row of ListUnits.
type UnitState struct {
	Name, LoadState, ActiveState, SubState string
}

// Systemd controls units over D-Bus. Every method waits for what it started.
type Systemd interface {
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	Reload(ctx context.Context, unit string) error
	ReloadOrRestart(ctx context.Context, unit string) error
	Enable(ctx context.Context, units ...string) error
	Disable(ctx context.Context, units ...string) error
	DaemonReload(ctx context.Context) error
	ResetFailed(ctx context.Context, unit string) error
	UnitProps(ctx context.Context, unit string) (UnitProps, error)
	ListUnits(ctx context.Context, patterns ...string) ([]UnitState, error)
	// StartTransient runs argv as a transient service unit (Type=exec,
	// garbage-collected once it ends, failed or not) and returns when
	// systemd has executed it. argv[0] must be absolute.
	StartTransient(ctx context.Context, unit, description string, argv []string) error
	// RunTransient runs a command as an account in a transient service and
	// waits for it to end (transient.go).
	RunTransient(ctx context.Context, r TransientRun) (TransientResult, error)
}

// UnitName appends ".service" to a bare name, as systemctl does.
func UnitName(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		switch name[i+1:] {
		case "service", "socket", "target", "timer", "slice", "scope", "mount", "path", "device", "swap", "automount":
			return name
		}
	}
	return name + ".service"
}

// DefaultJobTimeout bounds every call and job wait, whatever the caller's
// context says: a lost job result must never hold the caller (and the
// locks it holds) for the length of a request.
const DefaultJobTimeout = 2 * time.Minute

// dialTimeout bounds connecting and authenticating to systemd.
const dialTimeout = 15 * time.Second

// stateTimeout bounds the read of a unit's state after a timed-out wait.
const stateTimeout = 10 * time.Second

type jobTimeoutKey struct{}

// WithJobTimeout raises (or lowers) the adapter's ceiling for the calls made
// with ctx, for a job known to take long (a MariaDB restart). The context's
// own deadline still applies.
func WithJobTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, jobTimeoutKey{}, d)
}

// dbusConn is the part of *sddbus.Conn the adapter uses, so the logic above
// the wire (job results, property decoding, connection loss) is testable.
type dbusConn interface {
	StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	RestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	ReloadUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	ReloadOrRestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	StartTransientUnitContext(ctx context.Context, name, mode string, props []sddbus.Property, ch chan<- string) (int, error)
	EnableUnitFilesContext(ctx context.Context, files []string, runtime, force bool) (bool, []sddbus.EnableUnitFileChange, error)
	DisableUnitFilesContext(ctx context.Context, files []string, runtime bool) ([]sddbus.DisableUnitFileChange, error)
	ReloadContext(ctx context.Context) error
	ResetFailedUnitContext(ctx context.Context, name string) error
	GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error)
	ListUnitsByPatternsContext(ctx context.Context, states, patterns []string) ([]sddbus.UnitStatus, error)
	// Connected is false, and Done closed, once the connection is dead:
	// either of go-systemd's two sockets (calls, and the signals that carry
	// job results) closed.
	Connected() bool
	Done() <-chan struct{}
	Close()
}

// DBusSystemd is the real Systemd.
//
// It shares one connection between all its callers. The connection is
// never bound to a caller's context (SPEC: the connection lived only as long
// as the request that dialled it), never closed while a caller uses it, and
// replaced when it dies: callers holding the dead one fail at once with
// ErrConnLost, and the idempotent operations retry once on a fresh one.
type DBusSystemd struct {
	dial func(ctx context.Context) (dbusConn, error)
	// JobTimeout is the ceiling on every call and job wait; 0 means
	// DefaultJobTimeout. WithJobTimeout overrides it per call.
	JobTimeout time.Duration

	mu      sync.Mutex
	cur     *busConn
	gen     uint64    // generations dialled so far
	dialing *dialCall // the dial in progress, if any
	units   map[string]*unitLock
}

// unitLock serializes this adapter's jobs on one unit. systemd merges a job
// into one already queued for the unit and returns the same job path, and
// go-systemd keeps one listener per path: the first waiter's channel is
// replaced and its result never comes (measured on systemd 255 and 259: a
// restart left waiting with the unit active and the connection up).
type unitLock struct {
	sem   chan struct{}
	users int
}

// lockUnit takes unit's job lock, or gives up with ctx.
func (s *DBusSystemd) lockUnit(ctx context.Context, unit string) (func(), error) {
	s.mu.Lock()
	if s.units == nil {
		s.units = map[string]*unitLock{}
	}
	l := s.units[unit]
	if l == nil {
		l = &unitLock{sem: make(chan struct{}, 1)}
		s.units[unit] = l
	}
	l.users++
	s.mu.Unlock()
	done := func() {
		s.mu.Lock()
		if l.users--; l.users == 0 {
			delete(s.units, unit)
		}
		s.mu.Unlock()
	}
	select {
	case l.sem <- struct{}{}:
		return func() { <-l.sem; done() }, nil
	case <-ctx.Done():
		done()
		return nil, ctx.Err()
	}
}

// busConn is one connection, counted while callers use it.
type busConn struct {
	dbusConn
	gen     uint64
	refs    int
	retired bool // replaced: closed once the last user lets go
	closed  bool
}

// dead is read from the sockets themselves (godbus cancels a connection's
// context before it fails the calls in flight), not from Done, which a
// goroutine closes a moment later.
func (b *busConn) dead() bool { return !b.Connected() }

// lostOn reports whether err is the failure of a call whose connection
// died, rather than an answer from systemd.
func lostOn(b *busConn, ctx context.Context, err error) bool {
	var de godbus.Error
	var dep *godbus.Error
	return b.dead() && ctx.Err() == nil && !errors.As(err, &de) && !errors.As(err, &dep)
}

type dialCall struct {
	done chan struct{}
	err  error
}

// NewSystemd returns a Systemd that dials on first use.
func NewSystemd() *DBusSystemd {
	return &DBusSystemd{dial: dialSystemd}
}

// realConn is go-systemd's connection with a death signal: its two godbus
// connections each cancel their own context when they close, whoever
// closes them (a read error, systemd going away, or Close).
type realConn struct {
	*sddbus.Conn
	done chan struct{}
}

func (r *realConn) Done() <-chan struct{} { return r.done }

// dialSystemd prefers systemd's private socket as root, which is what
// systemctl uses and needs neither dbus-daemon nor polkit, and falls back to
// the system bus.
//
// The godbus connections are dialled without a context on purpose:
// godbus closes a connection when the context it was dialled with ends, and
// go-systemd's *Context constructors pass theirs down. The connection used to
// be dialled with the context of whichever request came first, so it died
// with that request, under every other caller's call and job wait.
func dialSystemd(ctx context.Context) (dbusConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		if c, err := newRealConn(func() (*godbus.Conn, error) {
			conn, err := godbus.Dial("unix:path=/run/systemd/private")
			if err != nil {
				return nil, err
			}
			return conn, authExternal(conn, false)
		}); err == nil {
			return c, nil
		}
	}
	c, err := newRealConn(func() (*godbus.Conn, error) {
		conn, err := godbus.SystemBusPrivate()
		if err != nil {
			return nil, err
		}
		return conn, authExternal(conn, true)
	})
	if err != nil {
		return nil, fmt.Errorf("connect to systemd: %w", err)
	}
	return c, nil
}

// authExternal authenticates as go-systemd does (EXTERNAL with the numeric
// uid, no user lookup) and says Hello to a bus daemon.
func authExternal(conn *godbus.Conn, hello bool) error {
	err := conn.Auth([]godbus.Auth{godbus.AuthExternal(strconv.Itoa(os.Getuid()))})
	if err == nil && hello {
		err = conn.Hello()
	}
	if err != nil {
		conn.Close()
	}
	return err
}

func newRealConn(dialBus func() (*godbus.Conn, error)) (*realConn, error) {
	var buses []*godbus.Conn
	c, err := sddbus.NewConnection(func() (*godbus.Conn, error) {
		conn, err := dialBus()
		if err == nil {
			buses = append(buses, conn)
		}
		return conn, err
	})
	if err != nil {
		return nil, err
	}
	r := &realConn{Conn: c, done: make(chan struct{})}
	go func() {
		select {
		case <-buses[0].Context().Done():
		case <-buses[1].Context().Done():
		}
		close(r.done)
	}()
	return r, nil
}

// acquire returns the live connection, dialling one if there is none, and
// counts the caller as its user until release.
func (s *DBusSystemd) acquire(ctx context.Context) (*busConn, error) {
	for dialled := false; ; dialled = true {
		s.mu.Lock()
		var stale *busConn
		if b := s.cur; b != nil {
			if !b.dead() {
				b.refs++
				s.mu.Unlock()
				return b, nil
			}
			if s.retireLocked(b) {
				stale = b
			}
		}
		if dialled {
			// Dead as soon as it was made: no dialling in a loop.
			s.mu.Unlock()
			closeConn(stale)
			return nil, fmt.Errorf("connect to systemd: %w", ErrConnLost)
		}
		if s.dialing == nil {
			d := &dialCall{done: make(chan struct{})}
			s.dialing = d
			go s.runDial(d)
		}
		d := s.dialing
		s.mu.Unlock()
		closeConn(stale)
		select {
		case <-d.done:
			if d.err != nil {
				return nil, d.err
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to systemd: %w", ctx.Err())
		}
	}
}

func closeConn(b *busConn) {
	if b != nil {
		b.Close()
	}
}

// runDial dials with a context of its own: no caller's, so the connection
// outlives the request that happened to need it first.
func (s *DBusSystemd) runDial(d *dialCall) {
	type dialed struct {
		c   dbusConn
		err error
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	got := make(chan dialed, 1)
	go func() {
		c, err := s.dial(ctx)
		got <- dialed{c, err}
	}()
	var r dialed
	select {
	case r = <-got:
	case <-ctx.Done():
		r.err = fmt.Errorf("connect to systemd: %w", ctx.Err())
		go func() { // a dial that comes back late is not used
			if late := <-got; late.c != nil {
				late.c.Close()
			}
		}()
	}
	s.mu.Lock()
	if r.err == nil {
		s.gen++
		s.cur = &busConn{dbusConn: r.c, gen: s.gen}
	}
	d.err = r.err
	s.dialing = nil
	s.mu.Unlock()
	close(d.done)
}

// retireLocked takes b out of service. It reports whether b must be closed
// now (nobody uses it), which the caller does after letting go of mu;
// otherwise its last user closes it. The caller holds mu.
func (s *DBusSystemd) retireLocked(b *busConn) bool {
	if s.cur == b {
		s.cur = nil
	}
	b.retired = true
	if b.refs == 0 && !b.closed {
		b.closed = true
		return true
	}
	return false
}

func (s *DBusSystemd) release(b *busConn) {
	s.mu.Lock()
	b.refs--
	last := b.retired && b.refs == 0 && !b.closed
	if last {
		b.closed = true
	}
	s.mu.Unlock()
	if last {
		b.Close()
	}
}

// Close retires the connection: it is closed once its current users are
// done, and the next call dials again.
func (s *DBusSystemd) Close() {
	s.mu.Lock()
	var stale *busConn
	if b := s.cur; b != nil && s.retireLocked(b) {
		stale = b
	}
	s.mu.Unlock()
	closeConn(stale)
}

// ceiling bounds ctx with the adapter's job timeout.
func (s *DBusSystemd) ceiling(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	d := s.JobTimeout
	if v, ok := ctx.Value(jobTimeoutKey{}).(time.Duration); ok && v > 0 {
		d = v
	}
	if d <= 0 {
		d = DefaultJobTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	return ctx, cancel, d
}

// lost is the error of a call that failed because its connection died.
func lost(op, unit string) error {
	if unit == "" {
		return fmt.Errorf("systemd %s: %w", op, ErrConnLost)
	}
	return fmt.Errorf("systemd %s %s: %w", op, unit, ErrConnLost)
}

// call runs fn on the live connection under the ceiling. A failure on a
// connection that died is ErrConnLost; with retry, fn runs once more on a
// fresh connection. Only idempotent operations retry.
func (s *DBusSystemd) call(ctx context.Context, op, unit string, retry bool, fn func(ctx context.Context, c dbusConn) error) error {
	ctx, cancel, _ := s.ceiling(ctx)
	defer cancel()
	err := s.callOnce(ctx, op, unit, fn)
	if retry && errors.Is(err, ErrConnLost) && ctx.Err() == nil {
		err = s.callOnce(ctx, op, unit, fn)
	}
	return err
}

func (s *DBusSystemd) callOnce(ctx context.Context, op, unit string, fn func(ctx context.Context, c dbusConn) error) error {
	b, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer s.release(b)
	if err := fn(ctx, b.dbusConn); err != nil {
		if lostOn(b, ctx, err) {
			return lost(op, unit)
		}
		return err
	}
	return nil
}

type jobFunc func(c dbusConn) func(ctx context.Context, name, mode string, ch chan<- string) (int, error)

// job enqueues a job and waits for its result, under the ceiling. A wait
// that runs out reports the unit's state (*JobTimeoutError).
func (s *DBusSystemd) job(ctx context.Context, op, unit string, retry bool, start jobFunc) error {
	unit = UnitName(unit)
	parent := ctx
	ctx, cancel, _ := s.ceiling(ctx)
	defer cancel()
	began := time.Now()
	unlock, err := s.lockUnit(ctx, unit)
	if err == nil {
		defer unlock()
		err = s.jobOnce(ctx, op, unit, start)
	} else {
		err = fmt.Errorf("systemd %s %s: waiting for the unit's previous job: %w", op, unit, err)
	}
	if err == nil {
		return nil
	}
	if retry && errors.Is(err, ErrConnLost) && ctx.Err() == nil {
		err = s.jobOnce(ctx, op, unit, start)
	}
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrJobTimeout) {
		te := &JobTimeoutError{Op: op, Unit: unit, Waited: time.Since(began)}
		sctx, scancel := context.WithTimeout(context.WithoutCancel(parent), stateTimeout)
		te.State, te.StateErr = s.UnitProps(sctx, unit)
		scancel()
		return te
	}
	return err
}

func (s *DBusSystemd) jobOnce(ctx context.Context, op, unit string, start jobFunc) error {
	b, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer s.release(b)
	// Buffered: go-systemd delivers the result while holding its listener
	// lock, so an abandoned wait must not block it.
	ch := make(chan string, 1)
	if _, err := start(b.dbusConn)(ctx, unit, "replace", ch); err != nil {
		if lostOn(b, ctx, err) {
			return lost(op, unit)
		}
		return classifyDBus(op, unit, err)
	}
	result := func(res string) error {
		if res == "done" || res == "skipped" {
			return nil
		}
		return &JobError{Op: op, Unit: unit, Result: res}
	}
	select {
	case res := <-ch:
		return result(res)
	case <-b.Done():
		// The result travels on the signal socket: once it is gone, it
		// never comes. One that arrived just before is still counted.
		select {
		case res := <-ch:
			return result(res)
		default:
		}
		return lost(op, unit)
	case <-ctx.Done():
		return fmt.Errorf("systemd %s %s: %w", op, unit, ctx.Err())
	}
}

func classifyDBus(op, unit string, err error) error {
	var de godbus.Error
	if errors.As(err, &de) {
		switch de.Name {
		case "org.freedesktop.systemd1.NoSuchUnit", "org.freedesktop.systemd1.LoadFailed":
			return fmt.Errorf("systemd %s %s: %w", op, unit, ErrNoSuchUnit)
		case "org.freedesktop.systemd1.UnitExists":
			return fmt.Errorf("systemd %s %s: %w", op, unit, ErrUnitExists)
		}
	}
	var dep *godbus.Error
	if errors.As(err, &dep) {
		return classifyDBus(op, unit, *dep)
	}
	return fmt.Errorf("systemd %s %s: %w", op, unit, err)
}

// Start, Stop, Restart, Reload and ReloadOrRestart are idempotent: run twice
// they leave the unit as once, so a job cut off by a lost connection is
// enqueued again on a fresh one.

func (s *DBusSystemd) Start(ctx context.Context, unit string) error {
	return s.job(ctx, "start", unit, true, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
		return c.StartUnitContext
	})
}

func (s *DBusSystemd) Stop(ctx context.Context, unit string) error {
	return s.job(ctx, "stop", unit, true, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
		return c.StopUnitContext
	})
}

func (s *DBusSystemd) Restart(ctx context.Context, unit string) error {
	return s.job(ctx, "restart", unit, true, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
		return c.RestartUnitContext
	})
}

func (s *DBusSystemd) Reload(ctx context.Context, unit string) error {
	return s.job(ctx, "reload", unit, true, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
		return c.ReloadUnitContext
	})
}

func (s *DBusSystemd) ReloadOrRestart(ctx context.Context, unit string) error {
	return s.job(ctx, "reload-or-restart", unit, true, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
		return c.ReloadOrRestartUnitContext
	})
}

// StartTransient is not retried: a transient unit started twice runs its
// program twice.
func (s *DBusSystemd) StartTransient(ctx context.Context, unit, description string, argv []string) error {
	if err := checkTransient(unit, argv); err != nil {
		return err
	}
	props := []sddbus.Property{
		sddbus.PropDescription(description),
		sddbus.PropType("exec"),
		sddbus.PropExecStart(argv, true),
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive-or-failed")},
	}
	return s.job(ctx, "start-transient", unit, false, func(c dbusConn) func(context.Context, string, string, chan<- string) (int, error) {
		return func(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
			return c.StartTransientUnitContext(ctx, name, mode, props, ch)
		}
	})
}

// checkTransient refuses a transient unit that is not a plain service name
// or whose program is not an absolute path.
func checkTransient(unit string, argv []string) error {
	name := UnitName(unit)
	if !strings.HasSuffix(name, ".service") || strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("transient unit %q: %w", unit, ErrInvalidArgument)
	}
	if len(argv) == 0 || !strings.HasPrefix(argv[0], "/") {
		return fmt.Errorf("transient unit %s: program must be an absolute path: %w", unit, ErrInvalidArgument)
	}
	return nil
}

func unitNames(units []string) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = UnitName(u)
	}
	return out
}

func (s *DBusSystemd) Enable(ctx context.Context, units ...string) error {
	names := unitNames(units)
	return s.call(ctx, "enable", strings.Join(names, " "), true, func(ctx context.Context, c dbusConn) error {
		if _, _, err := c.EnableUnitFilesContext(ctx, names, false, true); err != nil {
			return classifyDBus("enable", strings.Join(names, " "), err)
		}
		return reload(ctx, c)
	})
}

func (s *DBusSystemd) Disable(ctx context.Context, units ...string) error {
	names := unitNames(units)
	return s.call(ctx, "disable", strings.Join(names, " "), true, func(ctx context.Context, c dbusConn) error {
		if _, err := c.DisableUnitFilesContext(ctx, names, false); err != nil {
			return classifyDBus("disable", strings.Join(names, " "), err)
		}
		return reload(ctx, c)
	})
}

func reload(ctx context.Context, c dbusConn) error {
	if err := c.ReloadContext(ctx); err != nil {
		return fmt.Errorf("systemd daemon-reload: %w", err)
	}
	return nil
}

func (s *DBusSystemd) DaemonReload(ctx context.Context) error {
	return s.call(ctx, "daemon-reload", "", true, reload)
}

func (s *DBusSystemd) ResetFailed(ctx context.Context, unit string) error {
	unit = UnitName(unit)
	return s.call(ctx, "reset-failed", unit, true, func(ctx context.Context, c dbusConn) error {
		if err := c.ResetFailedUnitContext(ctx, unit); err != nil {
			return classifyDBus("reset-failed", unit, err)
		}
		return nil
	})
}

func (s *DBusSystemd) UnitProps(ctx context.Context, unit string) (UnitProps, error) {
	unit = UnitName(unit)
	var p UnitProps
	err := s.call(ctx, "show", unit, true, func(ctx context.Context, c dbusConn) error {
		var err error
		p, err = unitProps(ctx, c, unit)
		return err
	})
	return p, err
}

func unitProps(ctx context.Context, c dbusConn, unit string) (UnitProps, error) {
	props, err := c.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return UnitProps{}, classifyDBus("show", unit, err)
	}
	p := UnitProps{
		Name:          unit,
		LoadState:     propString(props, "LoadState"),
		ActiveState:   propString(props, "ActiveState"),
		SubState:      propString(props, "SubState"),
		UnitFileState: propString(props, "UnitFileState"),
	}
	if us, ok := props["ActiveEnterTimestamp"].(uint64); ok && us > 0 {
		p.ActiveEnter = time.UnixMicro(int64(us)).UTC()
	}
	if !strings.HasSuffix(unit, ".service") || !p.Exists() {
		return p, nil
	}
	svc, err := c.GetUnitTypePropertiesContext(ctx, unit, "Service")
	if err != nil {
		return UnitProps{}, classifyDBus("show", unit, err)
	}
	if pid, ok := svc["MainPID"].(uint32); ok {
		p.MainPID = pid
	}
	if mem, ok := svc["MemoryCurrent"].(uint64); ok && mem != math.MaxUint64 {
		p.MemoryCurrent, p.MemoryKnown = mem, true
	}
	return p, nil
}

func propString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (s *DBusSystemd) ListUnits(ctx context.Context, patterns ...string) ([]UnitState, error) {
	var out []UnitState
	err := s.call(ctx, "list-units", "", true, func(ctx context.Context, c dbusConn) error {
		units, err := c.ListUnitsByPatternsContext(ctx, nil, patterns)
		if err != nil {
			return fmt.Errorf("systemd list-units: %w", err)
		}
		out = make([]UnitState, 0, len(units))
		for _, u := range units {
			out = append(out, UnitState{Name: u.Name, LoadState: u.LoadState, ActiveState: u.ActiveState, SubState: u.SubState})
		}
		return nil
	})
	return out, err
}
