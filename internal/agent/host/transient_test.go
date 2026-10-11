package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// runStub is a stubConn whose transient unit writes to the descriptor it was
// handed, as the service's processes would, before the start job completes.
type runStub struct {
	*stubConn
	output    string
	existsErr int // fail this many starts with UnitExists
}

func (r *runStub) StartTransientUnitContext(ctx context.Context, n, m string, props []sddbus.Property, ch chan<- string) (int, error) {
	if r.existsErr > 0 {
		r.existsErr--
		r.record("start-transient " + n + " exists")
		return 0, godbus.Error{Name: "org.freedesktop.systemd1.UnitExists"}
	}
	for _, p := range props {
		if p.Name == "StandardOutputFileDescriptor" {
			fd, err := syscall.Dup(int(p.Value.Value().(godbus.UnixFD)))
			if err != nil {
				return 0, err
			}
			f := os.NewFile(uintptr(fd), "unit-stdout")
			f.WriteString(r.output)
			f.Close()
		}
	}
	return r.stubConn.StartTransientUnitContext(ctx, n, m, props, ch)
}

func newRunStub() (*runStub, *DBusSystemd) {
	r := &runStub{stubConn: newStub()}
	return r, &DBusSystemd{dial: func(context.Context) (dbusConn, error) { return r, nil }}
}

func goodRun() TransientRun {
	return TransientRun{
		Unit: "cg-cron-cg-site-7-3", Description: "job", Argv: []string{"/bin/sh", "-c", "echo hi"},
		User: "cg-site-7", Dir: "/srv/sites/a.example", Env: []string{"PATH=/usr/bin:/bin"}, MaxRuntime: 5 * time.Minute,
	}
}

// The unit gets the account, directory, time limit, stop grace and the
// output pipe; what it writes reaches Output; a failed end is read and
// reset.
func TestRunTransientFailedEnd(t *testing.T) {
	st, sd := newRunStub()
	st.output = "out\nerr\n"
	st.props = map[string]any{"LoadState": "loaded", "ActiveState": "failed"}
	st.svcProps = map[string]any{"Result": "exit-code", "ExecMainCode": int32(1), "ExecMainStatus": int32(3)}
	var out bytes.Buffer
	r := goodRun()
	r.Output = &out
	res, err := sd.RunTransient(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "exit-code" || res.Status != 3 || res.OK() {
		t.Fatalf("result %+v", res)
	}
	if out.String() != "out\nerr\n" {
		t.Fatalf("output %q", out.String())
	}
	props := map[string]any{}
	for _, p := range st.transientProps {
		props[p.Name] = p.Value.Value()
	}
	if props["User"] != "cg-site-7" || props["WorkingDirectory"] != "/srv/sites/a.example" || props["Type"] != "exec" ||
		props["RuntimeMaxUSec"] != uint64(300_000_000) || props["TimeoutStopUSec"] != uint64(10_000_000) ||
		props["CollectMode"] != "inactive" || props["StandardInput"] != "null" {
		t.Fatalf("props %v", props)
	}
	if env, _ := props["Environment"].([]string); len(env) != 1 || env[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("environment %v", props["Environment"])
	}
	// ExecStart's last field is systemd's "ignore failure": it must be off.
	if es := fmt.Sprint(props["ExecStart"]); !strings.HasSuffix(es, "false}]") {
		t.Fatalf("ExecStart %s ignores failure", es)
	}
	if _, ok := props["StandardErrorFileDescriptor"].(godbus.UnixFD); !ok {
		t.Fatal("stderr is not piped")
	}
	if calls := strings.Join(st.calls, ";"); !strings.Contains(calls, "reset-failed cg-cron-cg-site-7-3.service") {
		t.Fatalf("the failed unit was not reset: %s", calls)
	}
}

// A run that ended cleanly has been collected by the time it is read.
func TestRunTransientCollected(t *testing.T) {
	st, sd := newRunStub()
	st.props = map[string]any{"LoadState": "not-found", "ActiveState": "inactive"}
	res, err := sd.RunTransient(context.Background(), goodRun())
	if err != nil || !res.OK() {
		t.Fatalf("%+v %v", res, err)
	}
	for _, c := range st.calls {
		if strings.HasPrefix(c, "typeprops") || strings.HasPrefix(c, "reset-failed") {
			t.Fatalf("read or reset a collected unit: %v", st.calls)
		}
	}
}

// A name still held by a failed earlier run is reset and the run goes
// ahead; one held by a running unit is ErrUnitExists.
func TestRunTransientUnitExists(t *testing.T) {
	st, sd := newRunStub()
	st.existsErr = 1
	st.props = map[string]any{"LoadState": "loaded", "ActiveState": "failed"}
	st.svcProps = map[string]any{"Result": "success"}
	if _, err := sd.RunTransient(context.Background(), goodRun()); err != nil {
		t.Fatalf("a failed leftover blocked the run: %v", err)
	}
	st, sd = newRunStub()
	st.existsErr = 1
	st.props = map[string]any{"LoadState": "loaded", "ActiveState": "active"}
	if _, err := sd.RunTransient(context.Background(), goodRun()); !errors.Is(err, ErrUnitExists) {
		t.Fatalf("err = %v", err)
	}
}

// A run that never started (its job failed) is reset and returned as the
// job error.
func TestRunTransientStartFails(t *testing.T) {
	st, sd := newRunStub()
	st.result = "failed"
	_, err := sd.RunTransient(context.Background(), goodRun())
	if !errors.Is(err, ErrJobFailed) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(st.calls, ";"), "reset-failed") {
		t.Fatal("the unit that never started was not reset")
	}
}

// A caller that gives up stops the unit.
func TestRunTransientCancelStops(t *testing.T) {
	st, sd := newRunStub()
	st.props = map[string]any{"LoadState": "loaded", "ActiveState": "active"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := sd.RunTransient(ctx, goodRun())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(st.calls, ";"), "stop cg-cron-cg-site-7-3.service") {
		t.Fatalf("not stopped: %v", st.calls)
	}
}

func TestRunTransientRefusesBadInput(t *testing.T) {
	_, sd := newRunStub()
	f := NewFakeSystemd()
	for name, mut := range map[string]func(r *TransientRun){
		"user":     func(r *TransientRun) { r.User = "-root" },
		"no user":  func(r *TransientRun) { r.User = "" },
		"dir":      func(r *TransientRun) { r.Dir = "srv" },
		"unclean":  func(r *TransientRun) { r.Dir = "/srv/../etc" },
		"limit":    func(r *TransientRun) { r.MaxRuntime = 0 },
		"env":      func(r *TransientRun) { r.Env = []string{"=x"} },
		"env nl":   func(r *TransientRun) { r.Env = []string{"A=1\nB=2"} },
		"argv":     func(r *TransientRun) { r.Argv = []string{"sh"} },
		"non-unit": func(r *TransientRun) { r.Unit = "x.timer" },
	} {
		r := goodRun()
		mut(&r)
		if _, err := sd.RunTransient(context.Background(), r); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := f.RunTransient(context.Background(), r); err == nil {
			t.Errorf("%s: the fake accepted it", name)
		}
	}
}

func TestFakeRunTransient(t *testing.T) {
	f := NewFakeSystemd()
	f.OnRun = func(r TransientRun) TransientResult {
		r.Output.Write([]byte("hello"))
		return TransientResult{Result: "timeout"}
	}
	var out bytes.Buffer
	r := goodRun()
	r.Output = &out
	res, err := f.RunTransient(context.Background(), r)
	if err != nil || res.Result != "timeout" || out.String() != "hello" || len(f.Runs()) != 1 || !f.Did("run-transient", r.Unit) {
		t.Fatalf("%+v %v %q", res, err, out.String())
	}
	f.SetUnit(UnitProps{Name: r.Unit, ActiveState: "active"})
	if _, err := f.RunTransient(context.Background(), r); !errors.Is(err, ErrUnitExists) {
		t.Fatalf("err = %v", err)
	}
}
