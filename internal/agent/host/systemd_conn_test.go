package host

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

// dialer hands out conns in order (the last one again when they run out)
// and counts the dials.
type dialer struct {
	mu    sync.Mutex
	conns []dbusConn
	n     int
	ctxs  []context.Context
}

func (d *dialer) dial(ctx context.Context) (dbusConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ctxs = append(d.ctxs, ctx)
	c := d.conns[min(d.n, len(d.conns)-1)]
	d.n++
	return c, nil
}

func (d *dialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (s *stubConn) callsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// dropMidJob enqueues the job and then loses the connection before the
// result comes: what happened when another request's context closed the
// shared connection under a waiting restart.
type dropMidJob struct {
	*stubConn
	after time.Duration
}

func (d *dropMidJob) lose(op, n, m string) (int, error) {
	d.record(op + " " + n + " " + m)
	go func() { time.Sleep(d.after); d.drop() }()
	return 1, nil
}

func (d *dropMidJob) RestartUnitContext(ctx context.Context, n, m string, ch chan<- string) (int, error) {
	return d.lose("restart", n, m)
}

func (d *dropMidJob) StartTransientUnitContext(ctx context.Context, n, m string, props []sddbus.Property, ch chan<- string) (int, error) {
	return d.lose("start-transient", n, m)
}

// A job whose connection drops while it is waited for fails at once with
// ErrConnLost, not at the deadline, and an idempotent one is enqueued again
// on a fresh connection, which completes it. The dead connection is closed.
func TestSystemdDropMidJobFailsFastAndRetries(t *testing.T) {
	first := &dropMidJob{stubConn: newStub(), after: 20 * time.Millisecond}
	second := newStub()
	d := &dialer{conns: []dbusConn{first, second}}
	sd := &DBusSystemd{dial: d.dial, JobTimeout: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute) // a request's deadline
	defer cancel()
	start := time.Now()
	if err := sd.Restart(ctx, "cg-php-cg-site-1"); err != nil {
		t.Fatalf("retry on a fresh connection: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the waiter took %v to notice the dead connection", el)
	}
	if d.dials() != 2 || !first.isClosed() {
		t.Fatalf("dials %d, first closed %v", d.dials(), first.isClosed())
	}
	if c1, c2 := first.callsSnapshot(), second.callsSnapshot(); len(c1) != 1 || len(c2) != 1 || c2[0] != "restart cg-php-cg-site-1.service replace" {
		t.Fatalf("calls %v / %v", c1, c2)
	}

	// Without the retry the waiter still fails at once, typed.
	lone := &dropMidJob{stubConn: newStub(), after: 20 * time.Millisecond}
	sd = &DBusSystemd{dial: func(context.Context) (dbusConn, error) { return lone, nil }}
	start = time.Now()
	err := sd.StartTransient(ctx, "cg-guard", "guard", []string{"/bin/true"})
	if !errors.Is(err, ErrConnLost) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	// A transient start is not idempotent: it is not started twice.
	if c := lone.callsSnapshot(); len(c) != 1 {
		t.Fatalf("started %d times", len(c))
	}
}

// dropOnReload loses the connection during the call, as godbus fails the
// calls in flight on a closed socket.
type dropOnReload struct{ *stubConn }

func (d *dropOnReload) ReloadContext(ctx context.Context) error {
	d.record("daemon-reload")
	d.drop()
	return errors.New("read unix @->/run/systemd/private: use of closed network connection")
}

// A daemon-reload cut off by a lost connection is ErrConnLost and runs again
// on a fresh connection (the journal's "site cleanup incomplete: systemd
// daemon-reload: … use of closed network connection").
func TestSystemdDroppedCallRetries(t *testing.T) {
	first, second := &dropOnReload{newStub()}, newStub()
	d := &dialer{conns: []dbusConn{first, second}}
	sd := &DBusSystemd{dial: d.dial}
	if err := sd.DaemonReload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.dials() != 2 || len(second.callsSnapshot()) != 1 {
		t.Fatalf("dials %d, calls %v", d.dials(), second.callsSnapshot())
	}
	// Every connection dies: the error is typed, and nothing loops.
	d = &dialer{conns: []dbusConn{&dropOnReload{newStub()}, &dropOnReload{newStub()}}}
	sd = &DBusSystemd{dial: d.dial}
	if err := sd.DaemonReload(context.Background()); !errors.Is(err, ErrConnLost) {
		t.Fatalf("err %v", err)
	}
	if d.dials() != 2 {
		t.Fatalf("dials %d", d.dials())
	}
	// A conn that is dead as soon as it is dialled is not dialled again
	// and again.
	dead := newStub()
	dead.drop()
	d = &dialer{conns: []dbusConn{dead}}
	sd = &DBusSystemd{dial: d.dial}
	if err := sd.ResetFailed(context.Background(), "x"); !errors.Is(err, ErrConnLost) {
		t.Fatalf("err %v", err)
	}
	if d.dials() > 2 {
		t.Fatalf("dials %d", d.dials())
	}
}

// A result that never comes is waited for no longer than the ceiling, even
// under a request deadline far away; the error says what the unit is doing.
func TestSystemdJobWaitIsBounded(t *testing.T) {
	c := newStub()
	c.result = "" // never
	c.props = map[string]any{"LoadState": "loaded", "ActiveState": "activating", "SubState": "start"}
	sd, _ := stubSystemd(c)
	sd.JobTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	start := time.Now()
	err := sd.Restart(ctx, "mariadb")
	el := time.Since(start)
	var te *JobTimeoutError
	if !errors.As(err, &te) || !errors.Is(err, ErrJobTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if el < 100*time.Millisecond || el > 2*time.Second {
		t.Fatalf("waited %v", el)
	}
	if te.State.ActiveState != "activating" || te.Unit != "mariadb.service" || te.Op != "restart" {
		t.Fatalf("%+v", te)
	}
	if !strings.Contains(err.Error(), "activating/start") {
		t.Fatalf("message %q", err)
	}

	// No deadline at all: a ceiling still applies (shortened here through
	// the per-call override).
	sd.JobTimeout = 0
	start = time.Now()
	err = sd.Start(WithJobTimeout(context.Background(), 50*time.Millisecond), "mariadb")
	if !errors.Is(err, ErrJobTimeout) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if DefaultJobTimeout != 2*time.Minute {
		t.Fatal("the default ceiling changed")
	}
}

// The connection is shared, never dialled with a caller's context (godbus
// closes a connection when its dial context ends), and never closed while a
// caller waits on it: Close retires it, and its last user closes it.
func TestSystemdConnectionOutlivesItsCallers(t *testing.T) {
	c := newStub()
	c.delay = 200 * time.Millisecond
	d := &dialer{conns: []dbusConn{c}}
	sd := &DBusSystemd{dial: d.dial}
	type key struct{}
	reqCtx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "request"))
	if err := sd.DaemonReload(reqCtx); err != nil {
		t.Fatal(err)
	}
	cancel() // the request that dialled is over
	if d.ctxs[0].Value(key{}) != nil {
		t.Fatal("the connection was dialled with the caller's context")
	}
	waited := make(chan error, 1)
	go func() { waited <- sd.Restart(context.Background(), "nginx") }()
	for !strings.Contains(strings.Join(c.callsSnapshot(), ","), "restart") {
		time.Sleep(time.Millisecond)
	}
	sd.Close()
	if c.isClosed() {
		t.Fatal("closed under a waiting caller")
	}
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	if !c.isClosed() {
		t.Fatal("the retired connection was not closed by its last user")
	}
	if d.dials() != 1 {
		t.Fatalf("dials %d", d.dials())
	}
}

// mergeStub is systemd merging a job into one already queued for the unit
// (same job path) and go-systemd keeping one listener per path: the earlier
// waiter's channel is replaced, and only the last one hears the result.
type mergeStub struct {
	*stubConn
	pmu     sync.Mutex
	pending map[string]chan<- string
}

func (m *mergeStub) RestartUnitContext(ctx context.Context, n, mode string, ch chan<- string) (int, error) {
	m.record("restart " + n)
	m.pmu.Lock()
	_, queued := m.pending[n]
	m.pending[n] = ch
	m.pmu.Unlock()
	if !queued {
		go func() {
			time.Sleep(50 * time.Millisecond)
			m.pmu.Lock()
			last := m.pending[n]
			delete(m.pending, n)
			m.pmu.Unlock()
			last <- "done"
		}()
	}
	return 1, nil
}

// Concurrent jobs on one unit are not merged by systemd into a job whose
// result only one of them would hear: the adapter runs them one after the
// other.
func TestSystemdJobsOnOneUnitDoNotMerge(t *testing.T) {
	m := &mergeStub{stubConn: newStub(), pending: map[string]chan<- string{}}
	sd := &DBusSystemd{dial: func(context.Context) (dbusConn, error) { return m, nil }, JobTimeout: 3 * time.Second}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sd.Restart(context.Background(), "cg-php-cg-site-1")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(sd.units) != 0 {
		t.Fatalf("unit locks left: %v", sd.units)
	}
}
