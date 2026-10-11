package agentrpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

const token = "0123456789abcdef0123456789abcdef-test-token"

type echoParams struct {
	Text string `json:"text"`
}

type siteParams struct {
	Site  state.Site  `json:"site"`
	Other *state.Site `json:"other,omitempty"`
}

func (p *siteParams) Sites() []*state.Site { return []*state.Site{&p.Site, p.Other} }

// Test-only rows: they exist only in this package's test binary.
var (
	tEcho     = define[echoParams, echoParams](Spec{Name: "TEcho", Timeout: 5 * time.Second, ReadOnly: true, Fail: "Echo failed"})
	tSlow     = define[None, None](Spec{Name: "TSlow", Timeout: 150 * time.Millisecond, Fail: "Slow failed"})
	tWaitRO   = define[None, None](Spec{Name: "TWaitRO", Timeout: 5 * time.Second, ReadOnly: true, Fail: "Wait failed"})
	tWaitRW   = define[None, None](Spec{Name: "TWaitRW", Timeout: 5 * time.Second, Fail: "Wait failed"})
	tBoom     = define[None, None](Spec{Name: "TBoom", Timeout: 5 * time.Second, Fail: "Could not do the thing"})
	tTyped    = define[None, None](Spec{Name: "TTyped", Timeout: 5 * time.Second, Fail: "Typed failed"})
	tPanic    = define[None, None](Spec{Name: "TPanic", Timeout: 5 * time.Second, Fail: "Panicked"})
	tSite     = define[siteParams, string](Spec{Name: "TSite", Timeout: 5 * time.Second, Sites: true, Fail: "Site failed"})
	tLive     = define[siteParams, string](Spec{Name: "TLive", Timeout: 300 * time.Millisecond, Sites: true, Lock: LockLiveSite, Fail: "Live failed"})
	tLockSite = define[siteParams, string](Spec{Name: "TLockSite", Timeout: 5 * time.Second, Sites: true, Lock: LockSite, Fail: "Lock failed"})
)

type fakeLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	gone  map[string]bool
}

func (l *fakeLocker) get(d string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	if l.locks[d] == nil {
		l.locks[d] = &sync.Mutex{}
	}
	return l.locks[d]
}

func (l *fakeLocker) LockSite(d string) func() {
	m := l.get(d)
	m.Lock()
	return m.Unlock
}

func (l *fakeLocker) LockLiveSite(s state.Site) (func(), error) {
	unlock := l.LockSite(s.Domain)
	l.mu.Lock()
	gone := l.gone[s.Domain]
	l.mu.Unlock()
	if gone {
		unlock()
		return nil, Conflict("site %s no longer exists", s.Domain)
	}
	return unlock, nil
}

type rig struct {
	srv    *Server
	client *Client
	sock   string
	locker *fakeLocker
	ran    atomic.Int32 // handlers entered
	seen   chan string  // what a handler observed
	gate   chan struct{}
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	r := &rig{locker: &fakeLocker{}, seen: make(chan string, 8), gate: make(chan struct{})}
	cfg.Token = token
	cfg.SitesRoot = "/srv/sites"
	cfg.Locker = r.locker
	if cfg.AllowedUIDs == nil {
		cfg.AllowedUIDs = []uint32{uint32(os.Getuid())}
	}
	cfg.Log = slog.New(slog.NewTextHandler(testWriter{t}, nil))
	r.srv = NewServer(cfg)
	Handle(r.srv, tEcho, func(_ context.Context, p echoParams) (echoParams, error) { r.ran.Add(1); return p, nil })
	Handle(r.srv, tSlow, func(ctx context.Context, _ None) (None, error) {
		r.ran.Add(1)
		<-ctx.Done()
		r.seen <- "slow: " + ctx.Err().Error()
		return None{}, ctx.Err()
	})
	wait := func(ctx context.Context, _ None) (None, error) {
		r.ran.Add(1)
		select {
		case <-ctx.Done():
			r.seen <- "canceled"
		case <-r.gate:
			r.seen <- "finished"
		}
		return None{}, nil
	}
	Handle(r.srv, tWaitRO, wait)
	Handle(r.srv, tWaitRW, wait)
	Handle(r.srv, tBoom, func(context.Context, None) (None, error) {
		r.ran.Add(1)
		return None{}, fmt.Errorf("runuser -u cg-site-1 -- wp --path=/srv/sites/a.com/current plugin list: exit status 1: Error: not WordPress")
	})
	Handle(r.srv, tTyped, func(context.Context, None) (None, error) {
		return None{}, fmt.Errorf("stage /srv/sites/a.com/x: %w", Invalid("the archive must be .tar.gz"))
	})
	Handle(r.srv, tPanic, func(context.Context, None) (None, error) { panic("boom") })
	Handle(r.srv, tSite, func(_ context.Context, p siteParams) (string, error) {
		r.ran.Add(1)
		other := ""
		if p.Other != nil {
			other = p.Other.RootPath
		}
		return p.Site.RootPath + "|" + other, nil
	})
	live := func(_ context.Context, p siteParams) (string, error) {
		r.ran.Add(1)
		if r.locker.get(p.Site.Domain).TryLock() {
			return "", errors.New("the site lock was not held")
		}
		return "ok", nil
	}
	Handle(r.srv, tLive, live)
	Handle(r.srv, tLockSite, live)

	r.sock = filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	go r.srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	r.client = NewClient(r.sock, token, cfg.Log)
	return r
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func site1() state.Site {
	return state.Site{ID: 1, Domain: "a.com", Type: state.SitePHP, SystemUser: "cg-site-1", RootPath: "/srv/sites/a.com", PHPVersion: "8.4"}
}

func wantCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	e := AsError(err)
	if e == nil {
		t.Fatalf("want a *Error with code %s, got %T %v", code, err, err)
	}
	if e.Code != code {
		t.Fatalf("code %s, want %s (%q / %q)", e.Code, code, e.Message, e.Detail)
	}
	return e
}

func TestRoundTripWithRealPeerCredentials(t *testing.T) {
	r := newRig(t, Config{}) // the kernel's peer uid, which is ours
	out, err := Call(context.Background(), r.client, tEcho, echoParams{Text: "hello"})
	if err != nil || out.Text != "hello" {
		t.Fatalf("echo: %v %+v", err, out)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			want := strings.Repeat("k", n+1)
			if out, err := Call(context.Background(), r.client, tEcho, echoParams{Text: want}); err != nil || out.Text != want {
				t.Errorf("call %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
}

// A peer that is neither the API nor root gets its connection closed before
// a byte is read, even with the right token.
func TestPeerWithAnotherUIDIsRefused(t *testing.T) {
	r := newRig(t, Config{AllowedUIDs: []uint32{0, 999}, PeerUID: func(net.Conn) (uint32, error) { return 4242, nil }})
	_, err := Call(context.Background(), r.client, tEcho, echoParams{Text: "x"})
	wantCode(t, err, CodeUnavailable)
	if r.ran.Load() != 0 {
		t.Fatal("a handler ran for a refused peer")
	}
	// On the wire: nothing comes back, not even an HTTP error.
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "POST /rpc/TEcho HTTP/1.1\r\nHost: agent\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\n\r\n{}", token)
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err == nil || n > 0 {
		t.Fatalf("a refused peer got an answer: %q", buf[:n])
	}
}

// The allowed set is exactly the configured uids: an error from the
// credential lookup refuses too.
func TestPeerCredentialErrorRefuses(t *testing.T) {
	r := newRig(t, Config{PeerUID: func(net.Conn) (uint32, error) { return 0, errors.New("no creds") }, AllowedUIDs: []uint32{0}})
	_, err := Call(context.Background(), r.client, tEcho, echoParams{})
	wantCode(t, err, CodeUnavailable)
}

func TestRootAndAPIUIDAreServed(t *testing.T) {
	for _, uid := range []uint32{0, 997} {
		r := newRig(t, Config{AllowedUIDs: []uint32{0, 997}, PeerUID: func(net.Conn) (uint32, error) { return uid, nil }})
		if _, err := Call(context.Background(), r.client, tEcho, echoParams{}); err != nil {
			t.Fatalf("uid %d: %v", uid, err)
		}
	}
}

func TestBadTokenIsUnauthorized(t *testing.T) {
	r := newRig(t, Config{})
	c := NewClient(r.sock, "wrong", nil)
	_, err := Call(context.Background(), c, tEcho, echoParams{})
	wantCode(t, err, CodeUnauthorized)
	if r.ran.Load() != 0 {
		t.Fatal("a handler ran without the token")
	}
}

func TestUnknownMethod(t *testing.T) {
	r := newRig(t, Config{})
	err := r.client.Call(context.Background(), "Nope", nil, nil)
	wantCode(t, err, CodeInternal) // the client refuses a name not in the table
	_, e := r.srv.Invoke(context.Background(), "Nope", nil)
	if e == nil || e.Code != CodeNotFound {
		t.Fatalf("server: %v", e)
	}
}

// The method's deadline cancels the handler's context, and the caller gets a
// typed timeout promptly.
func TestTimeoutCancelsTheHandler(t *testing.T) {
	r := newRig(t, Config{})
	start := time.Now()
	_, err := Call(context.Background(), r.client, tSlow, None{})
	e := wantCode(t, err, CodeTimeout)
	if strings.Contains(e.Message, "TSlow") || !strings.HasPrefix(e.Message, "Slow failed") {
		t.Fatalf("timeout message %q", e.Message)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	select {
	case got := <-r.seen:
		if got != "slow: context deadline exceeded" {
			t.Fatalf("handler saw %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler's context was never cancelled")
	}
}

// A read-only call carries the caller's cancellation to the handler.
func TestCallerCancellationReachesReadOnlyHandlers(t *testing.T) {
	r := newRig(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := Call(ctx, r.client, tWaitRO, None{}); errc <- err }()
	waitRan(t, r, 1)
	cancel()
	select {
	case got := <-r.seen:
		if got != "canceled" {
			t.Fatalf("handler saw %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach the handler")
	}
	if err := <-errc; err == nil {
		t.Fatal("a cancelled call reported success")
	}
}

// A mutating call, once sent, is not abandoned when its caller goes away.
func TestMutatingCallIsDetachedFromCallerCancellation(t *testing.T) {
	r := newRig(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := Call(ctx, r.client, tWaitRW, None{}); errc <- err }()
	waitRan(t, r, 1)
	cancel()
	time.Sleep(100 * time.Millisecond)
	close(r.gate)
	if got := <-r.seen; got != "finished" {
		t.Fatalf("handler saw %q", got)
	}
	if err := <-errc; err != nil {
		t.Fatalf("detached call: %v", err)
	}
}

func waitRan(t *testing.T, r *rig, n int32) {
	t.Helper()
	for i := 0; i < 300 && r.ran.Load() < n; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if r.ran.Load() < n {
		t.Fatal("handler never started")
	}
}

// Shutdown (the agent's answer to systemd's SIGTERM) lets a request in
// flight finish and reach its caller, then serves nothing more.
func TestShutdownAnswersTheRequestInFlight(t *testing.T) {
	r := newRig(t, Config{})
	done := make(chan error, 1)
	go func() {
		_, err := Call(context.Background(), r.client, tWaitRW, None{})
		done <- err
	}()
	waitRan(t, r, 1)
	shut := make(chan error, 1)
	go func() { shut <- r.srv.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned while a request was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(r.gate)
	if err := <-done; err != nil {
		t.Fatalf("the request in flight was cut off: %v", err)
	}
	if err := <-shut; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := Call(context.Background(), r.client, tEcho, echoParams{Text: "x"}); err == nil {
		t.Fatal("served after Shutdown")
	}
}

// An untyped handler error reaches the caller as the method's Fail message;
// the command line and the path stay in the detail.
func TestInternalErrorsKeepDetailsOutOfTheMessage(t *testing.T) {
	r := newRig(t, Config{})
	_, err := Call(context.Background(), r.client, tBoom, None{})
	e := wantCode(t, err, CodeInternal)
	if e.Message != "Could not do the thing" || err.Error() != e.Message {
		t.Fatalf("message %q / Error() %q", e.Message, err.Error())
	}
	if !strings.Contains(e.Detail, "/srv/sites/a.com") {
		t.Fatalf("detail lost: %q", e.Detail)
	}
	wrapped := fmt.Errorf("task: %w", err)
	if strings.Contains(wrapped.Error(), "/srv") || strings.Contains(wrapped.Error(), "runuser") {
		t.Fatalf("wrapping leaked the detail: %q", wrapped)
	}
	if HTTPStatus(err, 0) != http.StatusBadGateway || HTTPStatus(err, http.StatusBadRequest) != http.StatusBadRequest {
		t.Fatal("internal error status")
	}
}

func TestTypedErrorsKeepTheirMessage(t *testing.T) {
	r := newRig(t, Config{})
	_, err := Call(context.Background(), r.client, tTyped, None{})
	e := wantCode(t, err, CodeInvalid)
	if e.Message != "the archive must be .tar.gz" || !strings.Contains(e.Detail, "/srv/sites/a.com/x") {
		t.Fatalf("%+v", e)
	}
	if HTTPStatus(err, 0) != http.StatusBadRequest {
		t.Fatal("invalid is not 400")
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	for code, want := range map[Code]int{
		CodeInvalid: 400, CodeNotFound: 404, CodeConflict: 409, CodeTimeout: 504,
		CodeUnavailable: 503, CodeInternal: 502, CodeUnauthorized: 502, CodeCanceled: 502,
	} {
		if got := HTTPStatus(&Error{Code: code, Message: "m"}, 0); got != want {
			t.Errorf("%s: %d, want %d", code, got, want)
		}
	}
	if HTTPStatus(errors.New("plain"), 0) != 502 || Message(errors.New("/etc/x"), "safe") != "safe" {
		t.Fatal("a plain error is not treated as internal")
	}
}

func TestPanicIsRecovered(t *testing.T) {
	r := newRig(t, Config{})
	_, err := Call(context.Background(), r.client, tPanic, None{})
	if e := wantCode(t, err, CodeInternal); e.Message != "Panicked" {
		t.Fatalf("%+v", e)
	}
	if _, err := Call(context.Background(), r.client, tEcho, echoParams{}); err != nil {
		t.Fatalf("server did not survive a panic: %v", err)
	}
}

// Sites are validated before the handler, which sees the canonical record.
func TestSitesAreValidatedCentrally(t *testing.T) {
	r := newRig(t, Config{})
	s := site1()
	s.RootPath = "/srv/sites/../sites/a.com/"
	out, err := Call(context.Background(), r.client, tSite, siteParams{Site: s})
	if err != nil || out != "/srv/sites/a.com|" {
		t.Fatalf("valid site: %v %q", err, out)
	}
	other := site1()
	other.Domain, other.RootPath = "b.com", "/srv/sites/b.com"
	if out, err := Call(context.Background(), r.client, tSite, siteParams{Site: site1(), Other: &other}); err != nil || out != "/srv/sites/a.com|/srv/sites/b.com" {
		t.Fatalf("second site: %v %q", err, out)
	}
	before := r.ran.Load()
	for name, bad := range map[string]siteParams{
		"root elsewhere": {Site: func() state.Site { s := site1(); s.RootPath = "/etc"; return s }()},
		"bad user":       {Site: func() state.Site { s := site1(); s.SystemUser = "root"; return s }()},
		"bad second":     {Site: site1(), Other: &state.Site{Domain: "b.com"}},
	} {
		_, err := Call(context.Background(), r.client, tSite, bad)
		e := wantCode(t, err, CodeInvalid)
		if strings.Contains(e.Message, "/etc") || strings.Contains(e.Message, "/srv") {
			t.Errorf("%s: message names a path: %q", name, e.Message)
		}
	}
	if r.ran.Load() != before {
		t.Fatal("a handler ran with an invalid site")
	}
}

func TestLockLiveSiteIsHeldAndRefusesAGoneSite(t *testing.T) {
	r := newRig(t, Config{})
	if out, err := Call(context.Background(), r.client, tLive, siteParams{Site: site1()}); err != nil || out != "ok" {
		t.Fatalf("live: %v %q", err, out)
	}
	if out, err := Call(context.Background(), r.client, tLockSite, siteParams{Site: site1()}); err != nil || out != "ok" {
		t.Fatalf("lock: %v %q", err, out)
	}
	r.locker.mu.Lock()
	r.locker.gone = map[string]bool{"a.com": true}
	r.locker.mu.Unlock()
	before := r.ran.Load()
	_, err := Call(context.Background(), r.client, tLive, siteParams{Site: site1()})
	if e := wantCode(t, err, CodeConflict); e.Message != "site a.com no longer exists" {
		t.Fatalf("%+v", e)
	}
	// A deletion holding the lock is not refused by the plain site lock.
	if _, err := Call(context.Background(), r.client, tLockSite, siteParams{Site: site1()}); err != nil {
		t.Fatalf("LockSite on a gone site: %v", err)
	}
	if r.ran.Load() != before+1 {
		t.Fatal("handler ran for a gone site")
	}
}

// A call whose deadline passes while it waits for the site lock does not run
// once the lock comes free.
func TestExpiredWhileWaitingForTheLockDoesNotRun(t *testing.T) {
	r := newRig(t, Config{})
	unlock := r.locker.LockSite("a.com")
	_, err := Call(context.Background(), r.client, tLive, siteParams{Site: site1()})
	wantCode(t, err, CodeTimeout)
	unlock()
	time.Sleep(100 * time.Millisecond)
	if r.ran.Load() != 0 {
		t.Fatal("the handler ran after its caller had timed out")
	}
	if !r.locker.get("a.com").TryLock() {
		t.Fatal("the lock was not released")
	}
}

// Before authentication a peer cannot make the agent buffer an unbounded
// header.
func TestOversizedHeaderIsRefused(t *testing.T) {
	r := newRig(t, Config{})
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "POST /rpc/TEcho HTTP/1.1\r\nHost: agent\r\nX-Big: %s", strings.Repeat("a", 1<<20))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	if strings.Contains(string(buf[:n]), " 200 ") {
		t.Fatalf("served an oversized header: %q", buf[:n])
	}
	if r.ran.Load() != 0 {
		t.Fatal("handler ran")
	}
}

func TestBadParams(t *testing.T) {
	r := newRig(t, Config{})
	err := r.client.Call(context.Background(), tEcho.Name(), "not an object", nil)
	if e := wantCode(t, err, CodeInvalid); e.Message != "bad parameters" {
		t.Fatalf("%+v", e)
	}
}

// Every row of the real table is complete, and a row that disagrees with its
// parameter type about carrying sites does not load.
func TestTable(t *testing.T) {
	n := 0
	for _, s := range Specs() {
		if strings.HasPrefix(s.Name, "T") && len(s.Name) > 1 && s.Name[1] >= 'A' && s.Name[1] <= 'Z' {
			continue // this file's rows
		}
		n++
		if s.Timeout <= 0 || s.Fail == "" {
			t.Errorf("%s: incomplete row %+v", s.Name, s)
		}
		if strings.ContainsAny(s.Fail, "/%") {
			t.Errorf("%s: Fail is not plain text: %q", s.Name, s.Fail)
		}
	}
	if n < 50 {
		t.Fatalf("only %d methods in the table", n)
	}
	mustPanic := func(name string, f func()) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("sites without Sites", func() { define[siteParams, None](Spec{Name: "TX1", Timeout: time.Second, Fail: "x"}) })
	mustPanic("Sites without sites", func() { define[echoParams, None](Spec{Name: "TX2", Timeout: time.Second, Sites: true, Fail: "x"}) })
	mustPanic("duplicate", func() { define[echoParams, None](Spec{Name: "TEcho", Timeout: time.Second, Fail: "x"}) })
	mustPanic("no Fail", func() { define[echoParams, None](Spec{Name: "TX3", Timeout: time.Second}) })
}
