package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

type fakeDrainer struct {
	called   bool
	deadline time.Time
	order    *[]string
	addr     string
}

func (f *fakeDrainer) Drain(ctx context.Context) error {
	f.called = true
	f.deadline, _ = ctx.Deadline()
	entry := "drain"
	if c, err := net.Dial("tcp", f.addr); err == nil {
		c.Close()
		entry = "drain (http still open)"
	}
	*f.order = append(*f.order, entry)
	return nil
}

// shutdown stops the loops first, then the HTTP servers (closed by the time
// the hook runs), then calls the drain hook once, all under one deadline.
func TestShutdownOrderAndDeadline(t *testing.T) {
	var order []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go srv.Serve(ln)
	if resp, err := http.Get("http://" + ln.Addr().String()); err == nil { // serving, so Shutdown owns ln
		resp.Body.Close()
	} else {
		t.Fatal(err)
	}

	d := &fakeDrainer{order: &order, addr: ln.Addr().String()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	began := time.Now()
	if err := shutdown(time.Second, 7*time.Second, func() { order = append(order, "app") }, log, d, srv); err != nil {
		t.Fatal(err)
	}
	if !d.called || len(order) != 2 || order[0] != "app" || order[1] != "drain" {
		t.Fatalf("order %v", order)
	}
	if left := d.deadline.Sub(began); left < 6*time.Second || left > 8*time.Second {
		t.Fatalf("drain deadline %v after start, want about 7s", left)
	}
}

// A request waiting on the stop itself (the API restarting itself through
// the agent) is cancelled at the short HTTP deadline: the stop does not wait
// for the task drain's timeout, and the handler sees its context end.
func TestShutdownCancelsRequestsAtTheHTTPDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, cancelled := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(time.Minute):
		}
	})}
	go srv.Serve(ln)
	go func() {
		if resp, err := http.Post("http://"+ln.Addr().String()+"/api/services/cloudground-api/restart", "application/json", nil); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	var order []string
	d := &fakeDrainer{order: &order, addr: ln.Addr().String()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	began := time.Now()
	if err := shutdown(200*time.Millisecond, 30*time.Second, func() {}, log, d, srv); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the stop took %v with a request waiting on it", took)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting request's context was not cancelled")
	}
	// The drain still got its own, full deadline.
	if left := d.deadline.Sub(began); left < 29*time.Second {
		t.Fatalf("drain deadline %v after start, want about 30s", left)
	}
}

// Without socket activation for this process, no listener is inherited.
func TestNoInheritedSocket(t *testing.T) {
	t.Setenv("LISTEN_PID", "1")
	t.Setenv("LISTEN_FDS", "1")
	if ln, err := socketFromSystemd(); ln != nil || err != nil {
		t.Fatalf("%v %v", ln, err)
	}
}
