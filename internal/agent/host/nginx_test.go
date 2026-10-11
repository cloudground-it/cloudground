package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeProc is a /proc tree with an nginx master and its children.
type fakeProc struct {
	t    *testing.T
	root string
	pid  string
}

func newFakeProc(t *testing.T, master int) *fakeProc {
	root := t.TempDir()
	pidFile := filepath.Join(root, "nginx.pid")
	os.WriteFile(pidFile, []byte(strconv.Itoa(master)+"\n"), 0o644)
	p := &fakeProc{t: t, root: filepath.Join(root, "proc"), pid: pidFile}
	os.MkdirAll(p.root, 0o755)
	p.add(master, 1, "nginx: master process /usr/sbin/nginx")
	return p
}

func (p *fakeProc) add(pid, ppid int, title string) {
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	os.MkdirAll(dir, 0o755)
	// The comm field may hold spaces and parentheses.
	stat := strconv.Itoa(pid) + " (nginx (x) y) S " + strconv.Itoa(ppid) + " 1 1 0 -1\n"
	os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644)
	cmdline := append([]byte(title), make([]byte, 30)...) // nginx pads the title with NULs
	os.WriteFile(filepath.Join(dir, "cmdline"), cmdline, 0o644)
}

func (p *fakeProc) remove(pid int) { os.RemoveAll(filepath.Join(p.root, strconv.Itoa(pid))) }

// reloadingSystemd runs onReload when a reload job is issued.
type reloadingSystemd struct {
	*FakeSystemd
	onReload func()
}

func (r *reloadingSystemd) Reload(ctx context.Context, unit string) error {
	if err := r.FakeSystemd.Reload(ctx, unit); err != nil {
		return err
	}
	if r.onReload != nil {
		r.onReload()
	}
	return nil
}

func nginxFixture(t *testing.T, onReload func(p *fakeProc)) (*NginxCtl, *fakeProc) {
	p := newFakeProc(t, 100)
	p.add(101, 100, "nginx: worker process")
	p.add(102, 100, "nginx: worker process")
	p.add(103, 100, "nginx: cache manager process")
	p.add(500, 1, "nginx: worker process") // another master's worker
	sd := NewFakeSystemd()
	sd.SetUnit(UnitProps{Name: "nginx", ActiveState: "active"})
	rs := &reloadingSystemd{FakeSystemd: sd}
	if onReload != nil {
		rs.onReload = func() { onReload(p) }
	}
	return &NginxCtl{Exec: &FakeExec{}, Systemd: rs, Unit: "nginx.service", PIDFile: p.pid, Proc: p.root,
		Timeout: 500 * time.Millisecond, Poll: 5 * time.Millisecond}, p
}

// Reload returns once a worker that was not there before is running, and
// not before.
func TestNginxReloadWaitsForNewGeneration(t *testing.T) {
	n, _ := nginxFixture(t, func(p *fakeProc) {
		go func() {
			time.Sleep(80 * time.Millisecond)
			p.add(101, 100, "nginx: worker process is shutting down")
			p.remove(102)
			p.add(104, 100, "nginx: worker process")
		}()
	})
	start := time.Now()
	if err := n.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("returned after %v, before the new generation", d)
	}
}

// The new workers are up but an old one has not been told to stop yet: it
// still accepts connections with the old configuration, so the reload is not
// done until it is shutting down.
func TestNginxReloadWaitsForOldWorkersToStopAccepting(t *testing.T) {
	n, _ := nginxFixture(t, func(p *fakeProc) {
		p.add(104, 100, "nginx: worker process")
		p.add(105, 100, "nginx: worker process")
		p.add(101, 100, "nginx: worker process is shutting down")
		go func() {
			time.Sleep(80 * time.Millisecond)
			p.add(102, 100, "nginx: worker process is shutting down")
		}()
	})
	start := time.Now()
	if err := n.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("returned after %v, while an old worker was still serving", d)
	}
}

func TestNewGeneration(t *testing.T) {
	old := map[int]bool{1: true, 2: true}
	for _, c := range []struct {
		now  map[int]bool
		want bool
	}{
		{map[int]bool{1: true, 2: true}, false},          // nothing happened
		{map[int]bool{1: true, 2: true, 3: true}, false}, // new up, old still serving
		{map[int]bool{2: true, 3: true}, false},          // one old still serving
		{map[int]bool{3: true, 4: true}, true},           // only the new generation serves
		{map[int]bool{}, false},                          // no worker serves at all
	} {
		if got := newGeneration(old, c.now); got != c.want {
			t.Errorf("newGeneration(%v, %v) = %v, want %v", old, c.now, got, c.want)
		}
	}
}

// The master keeps the old generation (the new config failed at runtime):
// the reload reports it instead of passing silently.
func TestNginxReloadTimesOutWithoutNewGeneration(t *testing.T) {
	n, _ := nginxFixture(t, func(p *fakeProc) {
		// Old workers draining and a respawned helper are not a new generation.
		p.add(102, 100, "nginx: worker process is shutting down")
		p.add(105, 100, "nginx: cache manager process")
		p.add(106, 1, "nginx: worker process")
	})
	err := n.Reload(context.Background())
	if !errors.Is(err, ErrReloadTimeout) {
		t.Fatalf("err = %v, want ErrReloadTimeout", err)
	}
}

func TestNginxReloadNoMaster(t *testing.T) {
	n, p := nginxFixture(t, nil)
	os.Remove(p.pid)
	if err := n.Reload(context.Background()); err == nil {
		t.Fatal("reload without a master succeeded")
	}
}

func TestNginxReloadPropagatesJobFailure(t *testing.T) {
	n, _ := nginxFixture(t, nil)
	n.Systemd.(*reloadingSystemd).FailJob["nginx.service"] = "failed"
	if err := n.Reload(context.Background()); !errors.Is(err, ErrJobFailed) {
		t.Fatalf("err = %v", err)
	}
}

func TestNginxWorkers(t *testing.T) {
	n, _ := nginxFixture(t, nil)
	w, err := n.workers(100)
	if err != nil || len(w) != 2 || !w[101] || !w[102] {
		t.Fatalf("workers %v %v", w, err)
	}
}

func TestNginxTestCapturesStderr(t *testing.T) {
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		return FakeReply{Code: 1, Stderr: "nginx: [emerg] unknown directive \"x\"\nnginx: configuration file /etc/nginx/nginx.conf test failed\n"}
	}}
	n := &NginxCtl{Exec: fx}
	err := n.Test(context.Background())
	var ce *ConfigError
	if !errors.Is(err, ErrConfigInvalid) || !errors.As(err, &ce) || ce.Output == "" {
		t.Fatalf("err = %v", err)
	}
	if fx.Calls()[0].Line() != "nginx -t" {
		t.Fatal(fx.Calls())
	}
	ok := &NginxCtl{Exec: &FakeExec{}}
	if err := ok.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A missing binary is not a config verdict.
	missing := &NginxCtl{Exec: &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Err: os.ErrNotExist} }}}
	if err := missing.Test(context.Background()); errors.Is(err, ErrConfigInvalid) {
		t.Fatal("a missing binary reads as an invalid config")
	}
}

// nginx -V writes to stderr; the probe must read it there.
func TestNginxBuildInfoReadsStderr(t *testing.T) {
	fx := &FakeExec{Respond: func(FakeCall) FakeReply {
		return FakeReply{Stderr: "nginx version: nginx/1.28.0\nconfigure arguments: --with-http_v3_module\n"}
	}}
	out, err := (&NginxCtl{Exec: fx}).BuildInfo(context.Background())
	if err != nil || out == "" || out[:14] != "nginx version:" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestProcPPID(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "stat")
	os.WriteFile(f, []byte("42 (a) b) c) S 7 42 42 0\n"), 0o644)
	if ppid, ok := procPPID(f); !ok || ppid != 7 {
		t.Fatalf("%d %v", ppid, ok)
	}
}
