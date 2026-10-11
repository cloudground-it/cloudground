package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortBodyDeadline makes the body read deadline base d for one test.
func shortBodyDeadline(t *testing.T, d time.Duration) {
	old := bodyReadBase
	bodyReadBase = d
	t.Cleanup(func() { bodyReadBase = old })
}

// The panel's server bounds the headers and has no whole-connection read
// deadline: in net/http one cancels every request's context once it passes
// (#43).
func TestPanelHTTPServerHasNoReadTimeout(t *testing.T) {
	s := NewHTTPServer(http.NotFoundHandler())
	if s.ReadTimeout != 0 || s.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadTimeout %s, ReadHeaderTimeout %s", s.ReadTimeout, s.ReadHeaderTimeout)
	}
}

// A handler that reads its body and then works past the body's deadline
// keeps its context: the deadline is lifted once the body is read.
func TestBodyDeadlineIsLiftedOnceRead(t *testing.T) {
	shortBodyDeadline(t, 100*time.Millisecond)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = NewHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		withBodyDeadline(w, r, 1<<10)
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(5 * bodyReadBase):
			_, _ = io.WriteString(w, "done")
		case <-r.Context().Done():
			http.Error(w, "cancelled", http.StatusInternalServerError)
		}
	}))
	ts.Start()
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "done" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

// A body that stops arriving is cut at its deadline: the slow-client bound
// that ReadTimeout gave is kept for bodies.
func TestSlowBodyIsCut(t *testing.T) {
	shortBodyDeadline(t, 200*time.Millisecond)
	_, _, ts := testServer(t)
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST /api/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"email\":")
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		resp.Body.Close()
	}
	var ne net.Error
	if err != nil && errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the stalled body was still waited for after 10 s")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the stalled body was cut after %s", d)
	}
}

// A sign-in delay longer than any body deadline ends in the sign-in's own
// answer, 401, never a 500 from a cancelled context (#43).
func TestSignInDelayPastTheBodyDeadline(t *testing.T) {
	shortBodyDeadline(t, 100*time.Millisecond)
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	var waited atomic.Int32
	s.accounts.Sleep = func(ctx context.Context, d time.Duration) error {
		waited.Add(1)
		select {
		case <-time.After(5 * bodyReadBase): // stands for a delay of 32 s or more
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &client{t: t, base: ts.URL}
	for i := range 7 {
		if resp, body := c.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "wrong-password!!"}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d %s, want 401", i, resp.StatusCode, body)
		}
	}
	if waited.Load() == 0 {
		t.Fatal("no attempt waited a delay")
	}
	if resp, body := c.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in after the delays = %d %s", resp.StatusCode, body)
	}
}
