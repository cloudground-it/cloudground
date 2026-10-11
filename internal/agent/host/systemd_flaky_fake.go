package host

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// Where FlakyBus may drop a connection.
const (
	DropBeforeCall = "before" // the call never reaches systemd
	DropAfterCall  = "after"  // systemd did it; the answer (or job result) is lost
)

// FlakyBus is a simulated D-Bus to systemd for tests of code above the
// adapter: NewSystemd over it is the real DBusSystemd (one shared
// connection, bounded waits, retry on a lost connection), and every
// operation is played on Fake. Drop is asked at each call, before and after
// it, whether the connection dies there; nil never drops.
type FlakyBus struct {
	Fake *FakeSystemd
	Drop func(where string) bool

	dials, drops atomic.Int64
}

// NewSystemd returns the real adapter over the simulated bus.
func (b *FlakyBus) NewSystemd() *DBusSystemd {
	return &DBusSystemd{dial: func(context.Context) (dbusConn, error) {
		b.dials.Add(1)
		return &flakyConn{bus: b, done: make(chan struct{})}, nil
	}}
}

// Dials and Drops count the connections made and dropped.
func (b *FlakyBus) Dials() int64 { return b.dials.Load() }
func (b *FlakyBus) Drops() int64 { return b.drops.Load() }

type flakyConn struct {
	bus  *FlakyBus
	once sync.Once
	done chan struct{}
}

func (c *flakyConn) Connected() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *flakyConn) Done() <-chan struct{} { return c.done }
func (c *flakyConn) Close()                { c.once.Do(func() { close(c.done) }) }

func (c *flakyConn) kill() {
	c.bus.drops.Add(1)
	c.Close()
}

var errClosedSocket = &net.OpError{Op: "read", Net: "unix", Err: net.ErrClosed}

// enter is the start of a call: a dead connection, or one dropped now,
// fails it as godbus does.
func (c *flakyConn) enter() error {
	if !c.Connected() {
		return errClosedSocket
	}
	if c.bus.Drop != nil && c.bus.Drop(DropBeforeCall) {
		c.kill()
		return errClosedSocket
	}
	return nil
}

// leave is the end of a call that systemd carried out.
func (c *flakyConn) leave() bool {
	if c.bus.Drop != nil && c.bus.Drop(DropAfterCall) {
		c.kill()
		return false
	}
	return true
}

// asDBus turns the fake's typed errors back into what systemd answers.
func asDBus(err error) error {
	if errors.Is(err, ErrNoSuchUnit) {
		return godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}
	}
	if errors.Is(err, ErrUnitExists) {
		return godbus.Error{Name: "org.freedesktop.systemd1.UnitExists"}
	}
	return err
}

func (c *flakyConn) job(ctx context.Context, name string, ch chan<- string, run func(context.Context, string) error) (int, error) {
	if err := c.enter(); err != nil {
		return 0, err
	}
	err := run(ctx, name)
	var je *JobError
	result := "done"
	switch {
	case errors.As(err, &je):
		result = je.Result
	case err != nil:
		return 0, asDBus(err)
	}
	if c.leave() {
		ch <- result // buffered by the adapter
	}
	return 1, nil
}

func (c *flakyConn) call(err error) error {
	if err != nil {
		return asDBus(err)
	}
	if !c.leave() {
		return errClosedSocket
	}
	return nil
}

func (c *flakyConn) StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	return c.job(ctx, name, ch, c.bus.Fake.Start)
}

func (c *flakyConn) StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	return c.job(ctx, name, ch, c.bus.Fake.Stop)
}

func (c *flakyConn) RestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	return c.job(ctx, name, ch, c.bus.Fake.Restart)
}

func (c *flakyConn) ReloadUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	return c.job(ctx, name, ch, c.bus.Fake.Reload)
}

func (c *flakyConn) ReloadOrRestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	return c.job(ctx, name, ch, c.bus.Fake.ReloadOrRestart)
}

func (c *flakyConn) StartTransientUnitContext(ctx context.Context, name, mode string, props []sddbus.Property, ch chan<- string) (int, error) {
	return 0, errors.New("FlakyBus: transient units are not simulated")
}

// EnableUnitFilesContext and DisableUnitFilesContext change the unit file
// state only; the adapter's daemon-reload follows as its own call.
func (c *flakyConn) EnableUnitFilesContext(ctx context.Context, files []string, runtime, force bool) (bool, []sddbus.EnableUnitFileChange, error) {
	if err := c.enter(); err != nil {
		return false, nil, err
	}
	return false, nil, c.call(c.bus.Fake.setFileState(ctx, "enable", "enabled", files))
}

func (c *flakyConn) DisableUnitFilesContext(ctx context.Context, files []string, runtime bool) ([]sddbus.DisableUnitFileChange, error) {
	if err := c.enter(); err != nil {
		return nil, err
	}
	return nil, c.call(c.bus.Fake.setFileState(ctx, "disable", "disabled", files))
}

func (c *flakyConn) ReloadContext(ctx context.Context) error {
	if err := c.enter(); err != nil {
		return err
	}
	return c.call(c.bus.Fake.DaemonReload(ctx))
}

func (c *flakyConn) ResetFailedUnitContext(ctx context.Context, name string) error {
	if err := c.enter(); err != nil {
		return err
	}
	return c.call(c.bus.Fake.ResetFailed(ctx, name))
}

func (c *flakyConn) GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error) {
	if err := c.enter(); err != nil {
		return nil, err
	}
	p, err := c.bus.Fake.UnitProps(ctx, unit)
	if err := c.call(err); err != nil {
		return nil, err
	}
	return map[string]any{"LoadState": p.LoadState, "ActiveState": p.ActiveState, "SubState": p.SubState, "UnitFileState": p.UnitFileState}, nil
}

func (c *flakyConn) GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error) {
	if err := c.enter(); err != nil {
		return nil, err
	}
	p, err := c.bus.Fake.UnitProps(ctx, unit)
	if err := c.call(err); err != nil {
		return nil, err
	}
	return map[string]any{"MainPID": p.MainPID}, nil
}

func (c *flakyConn) ListUnitsByPatternsContext(ctx context.Context, states, patterns []string) ([]sddbus.UnitStatus, error) {
	if err := c.enter(); err != nil {
		return nil, err
	}
	us, err := c.bus.Fake.ListUnits(ctx, patterns...)
	if err := c.call(err); err != nil {
		return nil, err
	}
	out := make([]sddbus.UnitStatus, 0, len(us))
	for _, u := range us {
		out = append(out, sddbus.UnitStatus{Name: u.Name, LoadState: u.LoadState, ActiveState: u.ActiveState, SubState: u.SubState})
	}
	return out, nil
}
