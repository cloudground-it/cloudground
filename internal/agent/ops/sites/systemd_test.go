package sites

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cron"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// stuckIn lists the goroutines whose stack has one of frames in it.
func stuckIn(frames ...string) []string {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, f := range frames {
			if strings.Contains(g, f) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// noneStuck fails t if a goroutine is still inside the adapter or this
// package once everything returned (a short grace for ones on their way
// out).
func noneStuck(t *testing.T) {
	t.Helper()
	frames := []string{"host.(*DBusSystemd)", "ops/sites.(*Ops)"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		left := stuckIn(frames...)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines still blocked:\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// R-3.7 storm: creates, deletes and applies at once, over the real systemd
// adapter on a bus that drops connections at random. Nothing blocks: every
// call returns well within the bound, any failure is a lost connection
// (typed), and a repeat with a steady bus brings every site to its state.
func TestConcurrentLifecycleOverAFlakyBus(t *testing.T) {
	e := newEnv(t)
	var dropping atomic.Bool
	var mu sync.Mutex
	rng := rand.New(rand.NewSource(37))
	bus := &host.FlakyBus{Fake: e.systemd, Drop: func(string) bool {
		if !dropping.Load() {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		return rng.Float64() < 0.04
	}}
	sd := bus.NewSystemd()
	sd.JobTimeout = 10 * time.Second
	e.Systemd = sd
	e.Cron.(*cron.Ops).Systemd = sd

	const n = 8
	var old, kept, fresh []state.Site
	for i := int64(1); i <= 2*n; i++ {
		typ := state.SitePHP
		if i%4 == 0 {
			typ = state.SiteStatic
		}
		s := e.site(i, fmt.Sprintf("old%d.example.com", i), typ)
		e.create(t, s)
		if i <= n {
			old = append(old, s)
		} else {
			kept = append(kept, s)
		}
	}
	for i := int64(100); i < 100+n; i++ {
		fresh = append(fresh, e.site(i, fmt.Sprintf("new%d.example.com", i), state.SitePHP))
	}

	dropping.Store(true)
	type outcome struct {
		what string
		err  error
	}
	results := make(chan outcome, 3*n)
	var wg sync.WaitGroup
	run := func(what string, f func(ctx context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute) // a request's deadline
			defer cancel()
			results <- outcome{what, f(ctx)}
		}()
	}
	for i := 0; i < n; i++ {
		s, d, k := fresh[i], old[i], kept[i]
		run("create "+s.Domain, func(ctx context.Context) error {
			_, err := e.Create(ctx, rpc.CreateSiteParams{Site: s})
			return err
		})
		run("delete "+d.Domain, func(ctx context.Context) error {
			_, err := e.Delete(ctx, rpc.SiteRef{Site: d})
			return err
		})
		run("apply "+k.Domain, func(ctx context.Context) error {
			k := k
			k.Config.PHP.MemoryLimitMB = 320 // a changed pool: the master restarts
			_, err := e.Apply(ctx, rpc.SiteRef{Site: k})
			return err
		})
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatalf("the storm did not finish:\n%s", strings.Join(stuckIn("host.(*DBusSystemd)", "ops/sites.(*Ops)", "sync.(*Mutex).Lock"), "\n\n"))
	}
	close(results)
	failed := map[string]bool{}
	for r := range results {
		if r.err == nil {
			continue
		}
		if !errors.Is(r.err, host.ErrConnLost) {
			t.Errorf("%s: an error other than a lost connection: %v", r.what, r.err)
		}
		failed[r.what] = true
		t.Logf("%s: %v", r.what, r.err)
	}
	noneStuck(t)
	if bus.Drops() == 0 {
		t.Fatal("no connection was dropped: the test proves nothing")
	}
	t.Logf("drops %d, dials %d, failed calls %d of %d", bus.Drops(), bus.Dials(), len(failed), 3*n)

	// A steady bus: every operation repeated brings the machine to the
	// desired state (deletes and applies are idempotent; a failed create
	// was undone, or is torn down here).
	dropping.Store(false)
	ctx := context.Background()
	for _, s := range old {
		if err := e.Teardown(ctx, s); err != nil {
			t.Fatal(err)
		}
		if left := e.leftovers(s); len(left) != 0 {
			t.Fatalf("%s left %v", s.Domain, left)
		}
	}
	for _, s := range fresh {
		if failed["create "+s.Domain] {
			if err := e.Teardown(ctx, s); err != nil {
				t.Fatal(err)
			}
			e.create(t, s)
		}
		if !exists(e.vhostPath(s)) {
			t.Fatalf("%s has no vhost", s.Domain)
		}
		if u, ok := e.systemd.Unit(phpfpm.UnitName(s.SystemUser)); !ok || u.ActiveState != "active" {
			t.Fatalf("%s: master %+v", s.Domain, u)
		}
	}
	for _, s := range kept {
		if _, err := e.Apply(ctx, rpc.SiteRef{Site: s}); err != nil {
			t.Fatal(err)
		}
	}
	noneStuck(t)
}

// hangingSystemd never finishes a restart of one unit until released.
type hangingSystemd struct {
	*host.FakeSystemd
	unit    string
	entered chan struct{}
	release chan struct{}
}

func (h *hangingSystemd) hang(ctx context.Context, unit string) error {
	if host.UnitName(unit) == h.unit {
		close(h.entered)
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (h *hangingSystemd) Restart(ctx context.Context, unit string) error {
	if err := h.hang(ctx, unit); err != nil {
		return err
	}
	return h.FakeSystemd.Restart(ctx, unit)
}

func (h *hangingSystemd) ReloadOrRestart(ctx context.Context, unit string) error {
	if err := h.hang(ctx, unit); err != nil {
		return err
	}
	return h.FakeSystemd.ReloadOrRestart(ctx, unit)
}

// A PHP master's restart is not waited for under the nginx lock: while one
// site's restart hangs, another site is created and deleted, and nginx
// reloaded, without waiting for it.
func TestPHPRestartDoesNotHoldTheNginxLock(t *testing.T) {
	e := newEnv(t)
	a := e.site(1, "a.example.com", state.SitePHP)
	e.create(t, a)
	h := &hangingSystemd{FakeSystemd: e.systemd, unit: phpfpm.UnitName(a.SystemUser),
		entered: make(chan struct{}), release: make(chan struct{})}
	e.Systemd = h
	applied := make(chan error, 1)
	go func() {
		_, err := e.Apply(context.Background(), rpc.SiteRef{Site: a})
		applied <- err
	}()
	<-h.entered

	done := make(chan error, 1)
	go func() {
		b := e.site(2, "b.example.com", state.SitePHP)
		if _, err := e.Create(context.Background(), rpc.CreateSiteParams{Site: b}); err != nil {
			done <- err
			return
		}
		if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: b}); err != nil {
			done <- err
			return
		}
		done <- e.ReloadNginx(context.Background())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("another site waited for this site's PHP restart")
	}
	close(h.release)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
}
