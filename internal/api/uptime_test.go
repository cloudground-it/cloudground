package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// uptimeTarget is a site's public address in the tests: whatever host a
// check asks for, it reaches srv, which answers with code.
func uptimeTarget(t *testing.T, s *Server, code *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "" || !strings.HasPrefix(r.UserAgent(), "CloudGround-Uptime/") {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(int(code.Load()))
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	s.uptimeClient = &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
	return srv
}

func uptimeAlerts(t *testing.T, s *Server) []Alert {
	t.Helper()
	var out []Alert
	for _, a := range tableAlerts(t, s) {
		if a.Kind == "uptime" {
			out = append(out, a)
		}
	}
	return out
}

// #32: a monitored site that fails two checks in a row is an alert of its
// own site, kept while the outage lasts, sent to the webhook, and cleared
// when the site answers again; one failure is not an outage, a 404 is an
// answer, and a site not monitored is never checked.
func TestUptimeAlerts(t *testing.T) {
	s, _, _ := testServer(t)
	var code atomic.Int32
	code.Store(http.StatusOK)
	uptimeTarget(t, s, &code)
	site := seedActiveSite(t, s, "watched.example.com", state.SiteStatic, "", 0)
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Config.Uptime = state.UptimeCheck{Enabled: true, Path: "/health"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other := seedActiveSite(t, s, "unwatched.example.com", state.SiteStatic, "", 0)
	ctx := context.Background()
	evaluate := func() {
		t.Helper()
		if err := s.evaluateAlerts(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Before any round, the uptime alerts are left alone.
	if _, ready := s.uptimeConditions(nil); ready {
		t.Fatal("ready before a round")
	}
	s.uptimeRound(ctx)
	if r, ok := s.uptime.get(site.ID); !ok || !r.Up || r.Status != 200 {
		t.Fatalf("first check = %+v %v", r, ok)
	}
	if _, ok := s.uptime.get(other.ID); ok {
		t.Fatal("a site not monitored was checked")
	}

	code.Store(http.StatusNotFound)
	s.uptimeRound(ctx)
	if r, _ := s.uptime.get(site.ID); !r.Up {
		t.Fatalf("a 404 is an answer: %+v", r)
	}

	code.Store(http.StatusBadGateway)
	s.uptimeRound(ctx)
	evaluate()
	if a := uptimeAlerts(t, s); len(a) != 0 {
		t.Fatalf("one failure is an alert: %+v", a)
	}
	if !s.uptimeRound(ctx) {
		t.Fatal("the second failure did not ask for an evaluation")
	}
	evaluate()
	a := uptimeAlerts(t, s)
	if len(a) != 1 || a[0].SiteID != site.ID || a[0].Level != "error" || !strings.Contains(a[0].Message, "HTTP 502") {
		t.Fatalf("alerts after two failures = %+v", a)
	}
	first := a[0].ID
	s.uptimeRound(ctx)
	evaluate()
	if a := uptimeAlerts(t, s); len(a) != 1 || a[0].ID != first {
		t.Fatalf("a lasting outage changed its alert: %+v (was %s)", a, first)
	}

	// It goes out through the webhook, in words.
	got := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		got <- fmt.Sprint(m["text"])
	}))
	defer hook.Close()
	s.webhookClient = hook.Client()
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, hook.URL); err != nil {
		t.Fatal(err)
	}
	s.notifyAlerts(ctx)
	select {
	case text := <-got:
		if !strings.Contains(text, "watched.example.com") || !strings.Contains(text, "HTTP 502") {
			t.Fatalf("webhook text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook delivered")
	}

	code.Store(http.StatusOK)
	if !s.uptimeRound(ctx) {
		t.Fatal("the recovery did not ask for an evaluation")
	}
	evaluate()
	if a := uptimeAlerts(t, s); len(a) != 0 {
		t.Fatalf("alerts after the recovery = %+v", a)
	}
}

// The route turns the check on and sets its path, refuses a path that is
// not plain, and shows the last result.
func TestUptimeRoute(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	var code atomic.Int32
	code.Store(http.StatusOK)
	uptimeTarget(t, s, &code)
	site := seedActiveSite(t, s, "route.example.com", state.SiteStatic, "", 0)
	path := fmt.Sprintf("/api/sites/%d/uptime", site.ID)

	resp, body := admin.do("PUT", path, map[string]any{"enabled": true, "path": "/status"})
	var v uptimeView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || !v.Enabled || v.URL != "https://route.example.com/status" {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []map[string]any{{}, {"path": "status"}, {"path": "/a?b=1"}, {"path": "/a b"}} {
		if resp, body := admin.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, body)
		}
	}
	s.uptimeRound(context.Background())
	resp, body = admin.do("GET", path, nil)
	v = uptimeView{}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || v.Last == nil || !v.Last.Up || v.Path != "/status" {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	if resp, body := admin.do("PUT", path, map[string]any{"enabled": false}); resp.StatusCode != http.StatusOK || strings.Contains(string(body), `"last"`) {
		t.Fatalf("off = %d %s", resp.StatusCode, body)
	}
}
