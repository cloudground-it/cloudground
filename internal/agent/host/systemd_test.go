package host

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// stubConn is a dbusConn whose jobs complete asynchronously, as systemd's
// do: the call returns at once and the result arrives later as a signal.
type stubConn struct {
	mu        sync.Mutex
	calls     []string
	result    string        // job result to deliver ("" = never)
	delay     time.Duration // before delivering it
	callErr   error         // returned by the job call itself
	props     map[string]any
	svcProps  map[string]any
	units     []sddbus.UnitStatus
	patterns  []string
	connected bool
	closed    bool
	done      chan struct{}
	delivered chan struct{}
	// transientProps are the properties of the last transient unit.
	transientProps []sddbus.Property
}

func newStub() *stubConn {
	return &stubConn{result: "done", connected: true, done: make(chan struct{}), delivered: make(chan struct{}, 8)}
}

func (s *stubConn) record(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *stubConn) jobCall(op string) func(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	return func(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
		s.record(op + " " + name + " " + mode)
		if s.callErr != nil {
			return 0, s.callErr
		}
		if s.result != "" {
			go func() {
				time.Sleep(s.delay)
				ch <- s.result // must never block: the channel is buffered
				s.delivered <- struct{}{}
			}()
		}
		return 1, nil
	}
}

func (s *stubConn) StartTransientUnitContext(ctx context.Context, n, m string, props []sddbus.Property, ch chan<- string) (int, error) {
	s.mu.Lock()
	s.transientProps = props
	s.mu.Unlock()
	return s.jobCall("start-transient")(ctx, n, m, ch)
}

func (s *stubConn) StartUnitContext(ctx context.Context, n, m string, ch chan<- string) (int, error) {
	return s.jobCall("start")(ctx, n, m, ch)
}
func (s *stubConn) StopUnitContext(ctx context.Context, n, m string, ch chan<- string) (int, error) {
	return s.jobCall("stop")(ctx, n, m, ch)
}
func (s *stubConn) RestartUnitContext(ctx context.Context, n, m string, ch chan<- string) (int, error) {
	return s.jobCall("restart")(ctx, n, m, ch)
}
func (s *stubConn) ReloadUnitContext(ctx context.Context, n, m string, ch chan<- string) (int, error) {
	return s.jobCall("reload")(ctx, n, m, ch)
}
func (s *stubConn) ReloadOrRestartUnitContext(ctx context.Context, n, m string, ch chan<- string) (int, error) {
	return s.jobCall("reload-or-restart")(ctx, n, m, ch)
}
func (s *stubConn) EnableUnitFilesContext(ctx context.Context, files []string, runtime, force bool) (bool, []sddbus.EnableUnitFileChange, error) {
	s.record("enable " + files[0])
	return false, nil, nil
}
func (s *stubConn) DisableUnitFilesContext(ctx context.Context, files []string, runtime bool) ([]sddbus.DisableUnitFileChange, error) {
	s.record("disable " + files[0])
	return nil, nil
}
func (s *stubConn) ReloadContext(ctx context.Context) error { s.record("daemon-reload"); return nil }
func (s *stubConn) ResetFailedUnitContext(ctx context.Context, name string) error {
	s.record("reset-failed " + name)
	return s.callErr
}
func (s *stubConn) GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error) {
	s.record("props " + unit)
	return s.props, nil
}
func (s *stubConn) GetUnitTypePropertiesContext(ctx context.Context, unit, typ string) (map[string]any, error) {
	s.record("typeprops " + unit + " " + typ)
	return s.svcProps, nil
}
func (s *stubConn) ListUnitsByPatternsContext(ctx context.Context, states, patterns []string) ([]sddbus.UnitStatus, error) {
	s.patterns = patterns
	return s.units, nil
}
func (s *stubConn) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}
func (s *stubConn) Done() <-chan struct{} { return s.done }

// drop kills the connection, as a closed socket does.
func (s *stubConn) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected {
		s.connected = false
		close(s.done)
	}
}

func (s *stubConn) Close() {
	s.drop()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *stubConn) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func stubSystemd(c *stubConn) (*DBusSystemd, *int) {
	dials := 0
	return &DBusSystemd{dial: func(context.Context) (dbusConn, error) { dials++; return c, nil }}, &dials
}

// The call returns only after the job's completion signal, not when the
// job is merely enqueued.
func TestSystemdWaitsForJobCompletion(t *testing.T) {
	c := newStub()
	c.delay = 150 * time.Millisecond
	sd, _ := stubSystemd(c)
	start := time.Now()
	if err := sd.Restart(context.Background(), "mariadb"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("Restart returned before the job completed")
	}
	if c.calls[0] != "restart mariadb.service replace" {
		t.Fatalf("calls %v", c.calls)
	}
}

func TestSystemdJobResults(t *testing.T) {
	for _, res := range []string{"done", "skipped"} {
		c := newStub()
		c.result = res
		sd, _ := stubSystemd(c)
		if err := sd.Start(context.Background(), "nginx.service"); err != nil {
			t.Fatalf("%s: %v", res, err)
		}
	}
	for _, res := range []string{"failed", "timeout", "canceled", "dependency"} {
		c := newStub()
		c.result = res
		sd, _ := stubSystemd(c)
		err := sd.Stop(context.Background(), "nginx")
		var je *JobError
		if !errors.Is(err, ErrJobFailed) || !errors.As(err, &je) || je.Result != res || je.Unit != "nginx.service" || je.Op != "stop" {
			t.Fatalf("%s: err = %v", res, err)
		}
	}
}

// A cancelled wait returns the context's error, and the late result does
// not block go-systemd's dispatcher (the channel is buffered).
func TestSystemdCancelledWait(t *testing.T) {
	c := newStub()
	c.delay = 200 * time.Millisecond
	sd, _ := stubSystemd(c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := sd.Reload(ctx, "nginx"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-c.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("the late job result blocked")
	}
}

func TestSystemdNoSuchUnit(t *testing.T) {
	for _, e := range []error{
		godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []any{"Unit x.service not found."}},
		&godbus.Error{Name: "org.freedesktop.systemd1.LoadFailed"},
	} {
		c := newStub()
		c.callErr = e
		sd, _ := stubSystemd(c)
		if err := sd.Restart(context.Background(), "x"); !errors.Is(err, ErrNoSuchUnit) {
			t.Fatalf("err = %v", err)
		}
		if err := sd.ResetFailed(context.Background(), "x"); !errors.Is(err, ErrNoSuchUnit) {
			t.Fatalf("reset-failed err = %v", err)
		}
	}
	c := newStub()
	c.callErr = godbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}
	sd, _ := stubSystemd(c)
	if err := sd.Start(context.Background(), "x"); err == nil || errors.Is(err, ErrNoSuchUnit) {
		t.Fatalf("access denied classified as %v", err)
	}
}

func TestSystemdUnitProps(t *testing.T) {
	c := newStub()
	c.props = map[string]any{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		"UnitFileState": "enabled", "ActiveEnterTimestamp": uint64(1791486000123456),
	}
	c.svcProps = map[string]any{"MainPID": uint32(4242), "MemoryCurrent": uint64(52428800)}
	sd, _ := stubSystemd(c)
	p, err := sd.UnitProps(context.Background(), "nginx")
	if err != nil {
		t.Fatal(err)
	}
	want := time.UnixMicro(1791486000123456).UTC()
	if p.Name != "nginx.service" || p.ActiveState != "active" || p.SubState != "running" || p.UnitFileState != "enabled" ||
		p.MainPID != 4242 || !p.MemoryKnown || p.MemoryCurrent != 52428800 || !p.ActiveEnter.Equal(want) || !p.Exists() {
		t.Fatalf("props %+v", p)
	}

	// Unset memory accounting and a unit that never started.
	c.props["ActiveEnterTimestamp"] = uint64(0)
	c.svcProps["MemoryCurrent"] = uint64(math.MaxUint64)
	p, _ = sd.UnitProps(context.Background(), "nginx")
	if p.MemoryKnown || !p.ActiveEnter.IsZero() {
		t.Fatalf("props %+v", p)
	}

	// A missing unit is not an error, and its Service properties are not read.
	c.props = map[string]any{"LoadState": "not-found", "ActiveState": "inactive"}
	c.calls = nil
	p, err = sd.UnitProps(context.Background(), "gone")
	if err != nil || p.Exists() {
		t.Fatalf("missing unit: %+v %v", p, err)
	}
	for _, call := range c.calls {
		if strings.HasPrefix(call, "typeprops") {
			t.Fatal("read Service properties of a missing unit")
		}
	}
}

func TestSystemdEnableReloadsDaemon(t *testing.T) {
	c := newStub()
	sd, _ := stubSystemd(c)
	if err := sd.Enable(context.Background(), "cloudground-php-x"); err != nil {
		t.Fatal(err)
	}
	if err := sd.Disable(context.Background(), "cloudground-php-x.service"); err != nil {
		t.Fatal(err)
	}
	want := []string{"enable cloudground-php-x.service", "daemon-reload", "disable cloudground-php-x.service", "daemon-reload"}
	if len(c.calls) != len(want) {
		t.Fatalf("calls %v", c.calls)
	}
	for i := range want {
		if c.calls[i] != want[i] {
			t.Fatalf("calls %v", c.calls)
		}
	}
}

func TestSystemdListUnits(t *testing.T) {
	c := newStub()
	c.units = []sddbus.UnitStatus{{Name: "cloudground-php-a.service", LoadState: "loaded", ActiveState: "active", SubState: "running"}}
	sd, _ := stubSystemd(c)
	us, err := sd.ListUnits(context.Background(), "cloudground-php-*.service")
	if err != nil || len(us) != 1 || us[0].ActiveState != "active" || c.patterns[0] != "cloudground-php-*.service" {
		t.Fatalf("%+v %v %v", us, err, c.patterns)
	}
}

// A dropped connection (systemd re-exec) is re-dialled on the next call.
func TestSystemdRedialsDroppedConnection(t *testing.T) {
	c := newStub()
	sd, dials := stubSystemd(c)
	_ = sd.DaemonReload(context.Background())
	_ = sd.DaemonReload(context.Background())
	if *dials != 1 {
		t.Fatalf("dials = %d, want 1", *dials)
	}
	c.drop()
	fresh := newStub()
	sd.dial = func(context.Context) (dbusConn, error) { *dials++; return fresh, nil }
	if err := sd.DaemonReload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *dials != 2 || !c.isClosed() {
		t.Fatalf("dials = %d closed = %v", *dials, c.isClosed())
	}
}

func TestUnitName(t *testing.T) {
	for in, want := range map[string]string{
		"nginx": "nginx.service", "nginx.service": "nginx.service", "cg-site-1.slice": "cg-site-1.slice",
		"redis-server": "redis-server.service", "a.b": "a.b.service",
	} {
		if got := UnitName(in); got != want {
			t.Errorf("UnitName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFakeSystemd(t *testing.T) {
	f := NewFakeSystemd()
	ctx := context.Background()
	if err := f.Restart(ctx, "nginx"); !errors.Is(err, ErrNoSuchUnit) {
		t.Fatalf("unknown unit: %v", err)
	}
	f.SetUnit(UnitProps{Name: "nginx", ActiveState: "inactive"})
	if err := f.Restart(ctx, "nginx"); err != nil {
		t.Fatal(err)
	}
	p, _ := f.UnitProps(ctx, "nginx")
	if p.ActiveState != "active" || p.MainPID == 0 || p.ActiveEnter.IsZero() {
		t.Fatalf("after restart %+v", p)
	}
	f.FailJob["nginx.service"] = "failed"
	if err := f.Restart(ctx, "nginx"); !errors.Is(err, ErrJobFailed) {
		t.Fatalf("err = %v", err)
	}
	if !f.Did("restart", "nginx") {
		t.Fatal("restart not recorded")
	}
	if p, _ := f.UnitProps(ctx, "nope"); p.Exists() {
		t.Fatal("a missing unit exists")
	}
}

// A transient unit is started as Type=exec, collected when it ends, with the
// argv given; a name systemd already has loaded is ErrUnitExists; a relative
// program is refused before D-Bus is asked.
func TestStartTransient(t *testing.T) {
	st := newStub()
	sd := &DBusSystemd{dial: func(context.Context) (dbusConn, error) { return st, nil }}
	argv := []string{"/usr/local/bin/panel-agent.bak", "update-guard", "/var/lib/plan.json"}
	if err := sd.StartTransient(context.Background(), "cloudground-update-guard", "guard", argv); err != nil {
		t.Fatal(err)
	}
	props := map[string]any{}
	st.mu.Lock()
	for _, p := range st.transientProps {
		props[p.Name] = p.Value.Value()
	}
	calls := strings.Join(st.calls, ";")
	st.mu.Unlock()
	if props["Type"] != "exec" || props["CollectMode"] != "inactive-or-failed" {
		t.Fatalf("props %v", props)
	}
	if !strings.Contains(calls, "start-transient cloudground-update-guard.service replace") {
		t.Fatalf("calls %v", calls)
	}
	st.callErr = godbus.Error{Name: "org.freedesktop.systemd1.UnitExists"}
	if err := sd.StartTransient(context.Background(), "cloudground-update-guard", "guard", argv); !errors.Is(err, ErrUnitExists) {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range [][]string{nil, {"panel-agent"}, {""}} {
		if err := sd.StartTransient(context.Background(), "x", "x", bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if err := sd.StartTransient(context.Background(), "x.timer", "x", argv); !errors.Is(err, ErrInvalidArgument) {
		t.Error("a non-service transient unit was accepted")
	}

	f := NewFakeSystemd()
	if err := f.StartTransient(context.Background(), "g", "g", argv); err != nil {
		t.Fatal(err)
	}
	if err := f.StartTransient(context.Background(), "g", "g", argv); !errors.Is(err, ErrUnitExists) {
		t.Fatalf("fake: %v", err)
	}
	if got, _ := f.TransientArgv("g"); strings.Join(got, " ") != strings.Join(argv, " ") {
		t.Fatalf("fake argv %v", got)
	}
}
