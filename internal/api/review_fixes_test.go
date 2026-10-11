package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// syncBuffer is a log destination safe for the webhook's concurrent posts.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// raiseN puts n server-wide alerts in the table.
func raiseN(t *testing.T, s *Server, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.Store.RaiseAlert(state.Alert{Key: fmt.Sprintf("service:s%d", i), Kind: "service", Level: "error", Message: fmt.Sprintf("down #%d#", i)}); err != nil {
			t.Fatal(err)
		}
	}
}

// The webhook URL is a credential, and the panel's journal is readable by
// read-only accounts (GET /api/logs): a failed post logs why, never where.
func TestWebhookFailureDoesNotLogTheURL(t *testing.T) {
	s, _, _ := testServer(t)
	var logged syncBuffer
	s.Log = slog.New(slog.NewTextHandler(&logged, nil))
	raiseN(t, s, 1)

	// The guarded client refuses loopback; the error is the dialer's.
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, "https://127.0.0.1:1/services/T0/hook-secret-path"); err != nil {
		t.Fatal(err)
	}
	s.notifyAlerts(context.Background())
	// An endpoint that answers 500.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer hook.Close()
	s.webhookClient = hook.Client()
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, hook.URL+"/services/T1/hook-secret-path"); err != nil {
		t.Fatal(err)
	}
	s.notifyAlerts(context.Background())

	out := logged.String()
	if strings.Count(out, "alert webhook failed") != 2 {
		t.Fatalf("want two failures logged, got:\n%s", out)
	}
	if strings.Contains(out, "hook-secret-path") || strings.Contains(out, "/services/") {
		t.Fatalf("the webhook's path reached the journal:\n%s", out)
	}
	if !strings.Contains(out, "not a public address") || !strings.Contains(out, "500") {
		t.Fatalf("the log does not say why:\n%s", out)
	}
	// A failed post is not marked: the next run tries again.
	if due, _ := s.Store.AlertsToNotify(time.Now().Add(-notifyEvery)); len(due) != 1 {
		t.Fatalf("%d alerts due after failed posts, want 1", len(due))
	}
}

// Posts run a few at a time, each alert is marked when its own post
// succeeded, and a redirect is not followed.
func TestWebhookPostsConcurrentlyAndMarksEach(t *testing.T) {
	s, _, _ := testServer(t)
	var inflight, peak atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		if strings.Contains(body.String(), "#3#") {
			w.WriteHeader(http.StatusBadGateway) // this one fails
		}
	}))
	defer hook.Close()
	s.webhookClient = hook.Client()
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, hook.URL); err != nil {
		t.Fatal(err)
	}
	raiseN(t, s, 8)
	start := time.Now()
	s.notifyAlerts(context.Background())
	if took := time.Since(start); took > 8*150*time.Millisecond*3/4 {
		t.Fatalf("8 posts took %v: not concurrent", took)
	}
	if p := peak.Load(); p < 2 || p > webhookPosts {
		t.Fatalf("at most %d posts at once, saw %d", webhookPosts, p)
	}
	due, _ := s.Store.AlertsToNotify(time.Now().Add(-notifyEvery))
	if len(due) != 1 || due[0].Key != "service:s3" {
		t.Fatalf("due after the round = %+v, want only the failed one", due)
	}

	if guardedWebhookClient.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("the webhook client follows redirects")
	}
	redirect := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/latest/meta-data/", http.StatusFound))
	defer redirect.Close()
	c := &http.Client{CheckRedirect: guardedWebhookClient.CheckRedirect}
	if err := postWebhook(context.Background(), c, redirect.URL, "x"); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a redirect = %v, want a refusal naming the 302", err)
	}
}

// The dialer refuses what only the server can reach.
func TestWebhookRefusesInwardAddresses(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"fe80::1", "100.64.0.1", "0.0.0.0", "224.0.0.1", "::ffff:127.0.0.1"} {
		if !inwardIP(net.ParseIP(a)) {
			t.Errorf("%s is allowed", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if inwardIP(net.ParseIP(a)) {
			t.Errorf("%s is refused", a)
		}
	}
	if err := refuseInward("tcp", "127.0.0.1:443", nil); err == nil {
		t.Fatal("the dialer allows loopback")
	}
}

// Wrong connector tokens from every address together raise one alert past
// the threshold; the right token keeps working.
func TestConnectorTokenGuessingAlert(t *testing.T) {
	var g tokenGuesses
	now := time.Now()
	for i := 1; i < guessThreshold; i++ {
		if g.wrong(now) {
			t.Fatalf("a burst began at %d", i)
		}
	}
	if _, ok := g.condition(now); ok {
		t.Fatal("an alert below the threshold")
	}
	if !g.wrong(now) {
		t.Fatal("the threshold did not begin a burst")
	}
	a, ok := g.condition(now)
	if !ok || a.Key != "connector.token_guessing" || !strings.HasPrefix(a.Message, fmt.Sprint(guessThreshold)+" invalid") {
		t.Fatalf("condition = %+v, %v", a, ok)
	}
	g.wrong(now.Add(time.Minute))
	if b, _ := g.condition(now.Add(time.Minute)); b.ID != a.ID || b.Fingerprint != a.Fingerprint || b.Text.Args[0] != fmt.Sprint(guessThreshold+1) {
		t.Fatalf("the burst is not one occurrence: %+v then %+v", a, b)
	}
	if _, ok := g.condition(now.Add(time.Minute + guessHold)); ok {
		t.Fatal("the alert outlived its hold")
	}

	s, _, ts := testServer(t)
	site := activeSite(t, s, "guess.example.com")
	token := randomToken(24)
	if err := s.Store.SetSecret(state.SiteSecret(site.ID, state.SiteSecretConnectorToken), token); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < guessThreshold; i++ {
		connectorPost(t, ts.URL, fmt.Sprintf("guess-%d", i))
	}
	if code := connectorPost(t, ts.URL, token); code != http.StatusOK {
		t.Fatalf("the right token during guessing = %d", code)
	}
	evaluateAt(t, s, time.Now())
	found := false
	for _, a := range tableAlerts(t, s) {
		found = found || (a.Kind == "service" && a.SiteID == 0 && strings.Contains(a.Message, "invalid connector tokens"))
	}
	if !found {
		t.Fatalf("no token-guessing alert: %+v", tableAlerts(t, s))
	}
}

// fail answers the panel's own failures 500 with a fixed message; only an
// agent's error goes through errorReply.
func TestFailMapsStoreErrorsTo500(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		msg    string
	}{
		{stored(errors.New("database is locked: /var/lib/cloudground/state.db")), 500, msgInternal},
		{fmt.Errorf("read: %w", stored(errors.New("state.db: disk I/O error"))), 500, msgInternal},
		{errors.New("not the agent's type: /var/lib/cloudground/state.db"), 502, "The agent could not complete the operation"},
		{state.ErrNotFound, 500, msgInternal},
		{agentrpc.NotFound("no such file"), 404, "no such file"},
		{&agentrpc.Error{Code: agentrpc.CodeInternal, Message: "Could not read the file", Detail: "/srv/x: EIO"}, 502, "Could not read the file"},
		{decline(http.StatusConflict, "busy"), 409, "busy"},
	} {
		rec := httptest.NewRecorder()
		fail(rec, tc.err)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.msg) || strings.Contains(rec.Body.String(), "state.db") {
			t.Errorf("%v: %d %s, want %d %q", tc.err, rec.Code, rec.Body.String(), tc.status, tc.msg)
		}
	}
}

// Evaluations run one at a time.
func TestAlertEvaluationsAreSerialised(t *testing.T) {
	s, agent, _ := testServer(t)
	var running, overlap atomic.Int32
	agent.setHook(func(method string) {
		if method == agentrpc.ServiceStatus.Name() {
			if running.Add(1) > 1 {
				overlap.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
			running.Add(-1)
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.evaluateAlerts(context.Background()) }()
	}
	wg.Wait()
	if overlap.Load() != 0 {
		t.Fatal("two evaluations ran at once")
	}
}
