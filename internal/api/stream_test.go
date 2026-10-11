package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// sseEvent is one frame read off /api/events.
type sseEvent struct {
	ID     string
	Type   string          `json:"type"`
	SiteID int64           `json:"site_id"`
	Data   json.RawMessage `json:"data"`
}

// sseStream reads a live /api/events response in the background.
type sseStream struct {
	t      *testing.T
	resp   *http.Response
	events chan sseEvent
	done   chan struct{} // closed when the server ends the stream
	cancel context.CancelFunc
}

func openStream(t *testing.T, base, cookie string, hdr ...string) *sseStream {
	t.Helper()
	return openStreamPath(t, base, "/api/events", cookie, hdr...)
}

func openStreamPath(t *testing.T, base, path, cookie string, hdr ...string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	req.AddCookie(&http.Cookie{Name: insecureSessionName, Value: cookie})
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /api/events: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	st := &sseStream{t: t, resp: resp, events: make(chan sseEvent, 1024), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(st.done)
		rd := bufio.NewReader(resp.Body)
		var ev sseEvent
		var data string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if data != "" {
					if json.Unmarshal([]byte(data), &ev) == nil {
						st.events <- ev
					}
				}
				ev, data = sseEvent{}, ""
			case strings.HasPrefix(line, "id: "):
				ev.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	t.Cleanup(st.close)
	return st
}

func (st *sseStream) close() {
	st.cancel()
	st.resp.Body.Close()
}

// next returns the next event that is not a ping.
func (st *sseStream) next(timeout time.Duration) (sseEvent, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-st.events:
			if ev.Type == "ping" {
				continue
			}
			return ev, true
		case <-deadline:
			return sseEvent{}, false
		}
	}
}

// until reads events until one satisfies match, returning everything read
// on the way (the match last).
func (st *sseStream) until(timeout time.Duration, match func(sseEvent) bool) []sseEvent {
	st.t.Helper()
	var seen []sseEvent
	deadline := time.Now().Add(timeout)
	for {
		ev, ok := st.next(time.Until(deadline))
		if !ok {
			st.t.Fatalf("no matching event within %v; read %+v", timeout, seen)
		}
		seen = append(seen, ev)
		if match(ev) {
			return seen
		}
	}
}

func isType(typ string) func(sseEvent) bool { return func(e sseEvent) bool { return e.Type == typ } }

// sessionFor signs a user in directly in the store.
func sessionFor(t *testing.T, s *Server, userID int64) string {
	t.Helper()
	tok := randomToken(32)
	if err := s.Store.CreateSession(tok, userID, time.Hour); err != nil {
		t.Fatal(err)
	}
	return tok
}

func adminSession(t *testing.T, s *Server, ts *httptest.Server) (*client, string) {
	t.Helper()
	c := setupAdmin(t, s, ts)
	for _, ck := range c.cookies {
		if ck.Name == insecureSessionName {
			return c, ck.Value
		}
	}
	t.Fatal("no admin session cookie")
	return nil, ""
}

func mkSite(t *testing.T, s *Server, domain string, stagingOf int64) state.Site {
	t.Helper()
	site, err := s.Store.CreateSite(state.Site{
		Domain: domain, Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		SystemUser: "cg-" + strings.ReplaceAll(domain, ".", "-"), RootPath: "/srv/sites/" + domain,
		Status: state.SiteActive, StagingOf: stagingOf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// A task's lifecycle reaches the stream: created, progress with its
// percentage and message, finished with the outcome.
func TestEventsStreamTaskProgress(t *testing.T) {
	s, _, ts := testServer(t)
	_, tok := adminSession(t, s, ts)
	site := mkSite(t, s, "task.example.com", 0)
	st := openStream(t, ts.URL, tok)

	release := make(chan struct{})
	task, err := s.startJob(jobReq{kind: "cache.warm", siteID: site.ID}, func(ctx context.Context, _ state.Site, progress func(int, string)) error {
		progress(42, "halfway there")
		<-release
		return errors.New("it broke")
	})
	if err != nil {
		t.Fatal(err)
	}
	created := st.until(2*time.Second, isType("task.created"))
	last := created[len(created)-1]
	var tk jobs.Task
	_ = json.Unmarshal(last.Data, &tk)
	if tk.ID != task.ID || tk.Kind != "cache.warm" || tk.Status != "pending" || last.SiteID != site.ID || last.ID == "" {
		t.Fatalf("task.created: %+v %s", last, last.Data)
	}
	st.until(2*time.Second, func(e sseEvent) bool {
		return e.Type == "task.progress" && strings.Contains(string(e.Data), `"progress":42`) && strings.Contains(string(e.Data), "halfway there")
	})
	close(release)
	fin := st.until(2*time.Second, isType("task.finished"))
	var done map[string]any
	_ = json.Unmarshal(fin[len(fin)-1].Data, &done)
	if done["status"] != "failed" || done["error"] != "it broke" || done["progress"] != float64(42) {
		t.Fatalf("task.finished: %s", fin[len(fin)-1].Data)
	}
}

// What each role's stream carries (SPEC.md §5): an operator sees its own
// sites and their staging copies and nothing else; admins and read-only
// accounts see everything, server-wide events included.
func TestEventsAreFilteredPerUser(t *testing.T) {
	s, _, ts := testServer(t)
	adminC, adminTok := adminSession(t, s, ts)
	own := mkSite(t, s, "own.example.com", 0)
	other := mkSite(t, s, "other.example.com", 0)
	hash, _ := hashPassword("unused-password")
	op, _ := s.Store.CreateUser("op@example.com", hash, "operator")
	ro, _ := s.Store.CreateUser("ro@example.com", hash, "readonly")
	if err := s.Store.SetUserSites(op.ID, []int64{own.ID}); err != nil {
		t.Fatal(err)
	}
	opSt := openStream(t, ts.URL, sessionFor(t, s, op.ID))
	roSt := openStream(t, ts.URL, sessionFor(t, s, ro.ID))
	adSt := openStream(t, ts.URL, adminTok)

	// Through the API, as an admin: a new site, a settings save.
	resp, body := adminC.do("POST", "/api/sites", map[string]any{"domain": "example.com", "type": "static"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create site: %d %s", resp.StatusCode, body)
	}
	var created struct {
		Site state.Site `json:"site"`
	}
	_ = json.Unmarshal(body, &created)
	waitForTasks(t, s)
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.ProbeTarget = "https://127.0.0.2"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.MutateSite(other.ID, func(x *state.Site) error { x.Status = state.SiteError; return nil }); err != nil {
		t.Fatal(err)
	}
	s.Store.RecordMetric(state.MetricSample{CPUHeadroomPct: 50})
	if _, err := s.startJob(jobReq{kind: "panel.certificate"}, func(context.Context, state.Site, func(int, string)) error { return nil }); err != nil {
		t.Fatal(err)
	}
	waitForTasks(t, s)
	// A staging copy of the operator's site is the operator's.
	staging := mkSite(t, s, "staging.own.example.com", own.ID)
	if err := s.Store.UpdateUserProfile(ro.ID, "Read", "Only"); err != nil {
		t.Fatal(err)
	}
	// The marker: the last event, on the operator's own site.
	if _, err := s.Store.MutateSite(own.ID, func(x *state.Site) error { x.Status = state.SiteError; return nil }); err != nil {
		t.Fatal(err)
	}
	marker := func(e sseEvent) bool {
		return e.Type == "site.changed" && e.SiteID == own.ID && strings.Contains(string(e.Data), `"reason":"updated"`)
	}

	seen := opSt.until(3*time.Second, marker)
	sawStaging := false
	for _, e := range seen {
		if e.SiteID != own.ID && e.SiteID != staging.ID {
			t.Errorf("operator received %s about site %d: %s", e.Type, e.SiteID, e.Data)
		}
		if e.SiteID == staging.ID && e.Type == "site.changed" {
			sawStaging = true
		}
	}
	if !sawStaging {
		t.Error("operator did not receive its staging copy's creation")
	}

	for name, st := range map[string]*sseStream{"readonly": roSt, "admin": adSt} {
		types := map[string]bool{}
		sites := map[int64]bool{}
		for _, e := range st.until(3*time.Second, marker) {
			types[e.Type] = true
			sites[e.SiteID] = true
		}
		for _, want := range []string{"site.changed", "settings.changed", "metrics.sample", "task.created", "task.finished", "user.changed"} {
			if !types[want] {
				t.Errorf("%s did not receive %s", name, want)
			}
		}
		if !sites[created.Site.ID] || !sites[other.ID] || !sites[0] {
			t.Errorf("%s missed a site's or the server's events: %v", name, sites)
		}
	}

	// Taking the site away stops its events at once.
	if err := s.Store.SetUserSites(op.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.MutateSite(own.ID, func(x *state.Site) error { x.Status = state.SiteActive; return nil }); err != nil {
		t.Fatal(err)
	}
	if ev, ok := opSt.next(500 * time.Millisecond); ok {
		t.Errorf("operator still receives events after losing the site: %+v %s", ev, ev.Data)
	}
}

// An operator learns that its own site was deleted; nobody else's deletion.
func TestEventsSiteDeletedReachesItsOperator(t *testing.T) {
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	own := mkSite(t, s, "own.example.com", 0)
	other := mkSite(t, s, "other.example.com", 0)
	hash, _ := hashPassword("unused-password")
	op, _ := s.Store.CreateUser("op@example.com", hash, "operator")
	s.Store.SetUserSites(op.ID, []int64{own.ID})
	st := openStream(t, ts.URL, sessionFor(t, s, op.ID))
	if err := s.Store.DeleteSite(other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.DeleteSite(own.ID); err != nil {
		t.Fatal(err)
	}
	ev, ok := st.next(2 * time.Second)
	if !ok || ev.Type != "site.deleted" || ev.SiteID != own.ID {
		t.Fatalf("got %+v (ok=%v), want site.deleted of %d", ev, ok, own.ID)
	}
}

// Last-Event-ID resumes from the ring; an unknown ID gets a resync.
func TestEventsResume(t *testing.T) {
	s, _, ts := testServer(t)
	_, tok := adminSession(t, s, ts)
	site := mkSite(t, s, "resume.example.com", 0)

	st := openStream(t, ts.URL, tok)
	s.Store.MutateSite(site.ID, func(x *state.Site) error { x.Status = state.SiteError; return nil })
	first := st.until(2*time.Second, isType("site.changed"))
	lastID := first[len(first)-1].ID
	st.close()

	s.Store.MutateSite(site.ID, func(x *state.Site) error { x.Status = state.SiteActive; return nil })
	s.Store.SetCertError(site.ID, "boom")

	for _, how := range []string{"header", "query"} {
		var re *sseStream
		if how == "header" {
			re = openStream(t, ts.URL, tok, "Last-Event-ID: "+lastID)
		} else {
			re = openStreamPath(t, ts.URL, "/api/events?last_event_id="+lastID, tok)
		}
		a, _ := re.next(2 * time.Second)
		b, _ := re.next(2 * time.Second)
		if a.Type != "site.changed" || !strings.Contains(string(a.Data), `"status":"active"`) || b.Type != "cert.changed" {
			t.Fatalf("%s resume: got %+v then %+v", how, a, b)
		}
		re.close()
	}

	for _, id := range []string{"deadbeef.1", "garbage", lastID[:strings.Index(lastID, ".")] + ".999999"} {
		re := openStream(t, ts.URL, tok, "Last-Event-ID: "+id)
		if ev, ok := re.next(2 * time.Second); !ok || ev.Type != "resync" {
			t.Fatalf("resume from %q: got %+v, want resync", id, ev)
		}
		re.close()
	}
}

// The stream outlives the http.Server's read and write timeouts (the
// panel's are 30 s and 10 min) and carries heartbeats.
func TestEventsStreamOutlivesServerTimeouts(t *testing.T) {
	s, _, _ := testServer(t)
	s.heartbeat = 150 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Routes(), ReadTimeout: 300 * time.Millisecond, WriteTimeout: 400 * time.Millisecond}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	base := "http://" + ln.Addr().String()
	// Signed in through the store: hashing a password under -race takes
	// longer than this server's write timeout.
	admin, err := s.Store.CreateUser("admin@example.com", "unused", "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok := sessionFor(t, s, admin.ID)

	st := openStream(t, base, tok)
	pings := 0
	deadline := time.After(1500 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-st.events:
			if ev.Type == "ping" {
				pings++
			}
		case <-st.done:
			t.Fatalf("the stream ended after %d pings", pings)
		case <-deadline:
			break loop
		}
	}
	if pings < 5 {
		t.Fatalf("%d heartbeats in 1.5 s, want at least 5", pings)
	}
	s.Events.Publish(events.SettingsChanged, 0, nil)
	if _, ok := st.next(time.Second); !ok {
		t.Fatal("no event after the server's timeouts had passed")
	}
}

// Only the panel's own pages may open the stream.
func TestEventsRefuseCrossOrigin(t *testing.T) {
	s, _, ts := testServer(t)
	_, tok := adminSession(t, s, ts)
	for _, hdr := range [][2]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-site"},
		{"Origin", "https://evil.example.com"},
		{"Origin", "null"},
	} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
		req.AddCookie(&http.Cookie{Name: insecureSessionName, Value: tok})
		req.Header.Set(hdr[0], hdr[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %s answered %d, want 403", hdr[0], hdr[1], resp.StatusCode)
		}
	}
	// The panel's own page.
	openStream(t, ts.URL, tok, "Sec-Fetch-Site: same-origin", "Origin: "+ts.URL).close()
}

// Ending the session ends the stream at once, without waiting for a
// heartbeat.
func TestEventsEndWithTheSession(t *testing.T) {
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	hash, _ := hashPassword("unused-password")
	ro, _ := s.Store.CreateUser("ro@example.com", hash, "readonly")
	tok := sessionFor(t, s, ro.ID)
	st := openStream(t, ts.URL, tok)
	if err := s.Store.DeleteSession(tok); err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream outlived its session")
	}

	// Disabling the account: the same.
	tok = sessionFor(t, s, ro.ID)
	st = openStream(t, ts.URL, tok)
	if err := s.Store.UpdateUserAccess(ro.ID, "readonly", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream outlived its disabled account")
	}
}

// A subscriber that cannot keep up is dropped by the bus; the stream answers
// with resync and goes on.
func TestEventsSlowStreamGetsResync(t *testing.T) {
	s, _, ts := testServer(t)
	_, tok := adminSession(t, s, ts)
	// A stream that is not read: the bus drops it once its buffer and the
	// socket's are full.
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	req.AddCookie(&http.Cookie{Name: insecureSessionName, Value: tok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	big := strings.Repeat("x", 4096)
	for i := 0; i < 20000 && s.Events.Subscribers() > 0; i++ {
		s.Events.Publish(events.SettingsChanged, 0, big)
	}
	// Now read: somewhere in the stream there is a resync.
	rd := bufio.NewReader(resp.Body)
	found := make(chan bool, 1)
	go func() {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				found <- false
				return
			}
			if strings.Contains(line, `"type":"resync"`) {
				found <- true
				return
			}
		}
	}()
	go func() {
		// Keep publishing small events so the stream has something after
		// the resync.
		for i := 0; i < 50; i++ {
			s.Events.Publish(events.SettingsChanged, 0, nil)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatal("stream ended without a resync")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no resync after the subscriber fell behind")
	}
}

// The alert watcher turns the computed alert set into alert.raised and
// alert.cleared, with the alert's site.
func TestEventsAlertRaisedAndCleared(t *testing.T) {
	s, _, ts := testServer(t)
	adminC, tok := adminSession(t, s, ts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.watchAlerts(ctx)
	time.Sleep(50 * time.Millisecond) // baseline computed
	site := mkSite(t, s, "alert.example.com", 0)
	st := openStream(t, ts.URL, tok)

	if _, err := s.startJob(jobReq{kind: "wp.update", siteID: site.ID}, func(context.Context, state.Site, func(int, string)) error { return errors.New("disk full") }); err != nil {
		t.Fatal(err)
	}
	raised := st.until(3*time.Second, isType("alert.raised"))
	ev := raised[len(raised)-1]
	var a map[string]any
	_ = json.Unmarshal(ev.Data, &a)
	if ev.SiteID != site.ID || a["kind"] != "task" || !strings.Contains(fmt.Sprint(a["message"]), "disk full") {
		t.Fatalf("alert.raised: %+v %s", ev, ev.Data)
	}
	resp, body := adminC.do("POST", "/api/system/alerts/"+a["id"].(string)+"/dismiss", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss: %d %s", resp.StatusCode, body)
	}
	cleared := st.until(3*time.Second, isType("alert.cleared"))
	if c := cleared[len(cleared)-1]; c.SiteID != site.ID || !strings.Contains(string(c.Data), a["id"].(string)) {
		t.Fatalf("alert.cleared: %+v %s", c, c.Data)
	}
}

// The live monitor reads the agent once for every browser, only while a
// server-wide stream is open, and reports a service's change.
func TestEventsLiveMonitor(t *testing.T) {
	s, agent, ts := testServer(t)
	_, tok := adminSession(t, s, ts)
	s.monitorEvery = 50 * time.Millisecond
	since := time.Now().Add(-time.Hour)
	agent.mu.Lock()
	agent.results[agentrpc.SystemStatus.Name()] = rpc.SystemStatusResult{CPUCount: 4, CPUHeadroomPct: 77}
	agent.results[agentrpc.ServiceStatus.Name()] = rpc.ServiceStatusResult{Services: []rpc.ServiceInfo{{Name: "nginx", State: "running", Active: true, Since: since}}}
	agent.mu.Unlock()
	statusCalls := func() int {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		n := 0
		for _, c := range agent.calls {
			if c == agentrpc.SystemStatus.Name() {
				n++
			}
		}
		return n
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.liveMonitor(ctx)

	// An operator's stream alone does not start it.
	op := mkOperator(t, s)
	opSt := openStream(t, ts.URL, sessionFor(t, s, op.ID))
	time.Sleep(300 * time.Millisecond)
	if n := statusCalls(); n != 0 {
		t.Fatalf("the monitor asked the agent %d times with only an operator watching", n)
	}
	opSt.close()

	st := openStream(t, ts.URL, tok)
	got := st.until(2*time.Second, isType("system.status"))
	if !strings.Contains(string(got[len(got)-1].Data), `"cpu_headroom_pct":77`) {
		t.Fatalf("system.status: %s", got[len(got)-1].Data)
	}
	agent.mu.Lock()
	agent.results[agentrpc.ServiceStatus.Name()] = rpc.ServiceStatusResult{Services: []rpc.ServiceInfo{{Name: "nginx", State: "running", Active: true, Since: time.Now()}}}
	agent.mu.Unlock()
	ch := st.until(2*time.Second, isType("service.changed"))
	if !strings.Contains(string(ch[len(ch)-1].Data), `"nginx"`) {
		t.Fatalf("service.changed: %s", ch[len(ch)-1].Data)
	}
	st.close()
	// Nobody watching: no more agent calls.
	time.Sleep(150 * time.Millisecond)
	n := statusCalls()
	time.Sleep(300 * time.Millisecond)
	if m := statusCalls(); m != n {
		t.Fatalf("the monitor kept asking the agent with nobody watching (%d → %d)", n, m)
	}
}

func mkOperator(t *testing.T, s *Server) state.User {
	t.Helper()
	hash, _ := hashPassword("unused-password")
	op, err := s.Store.CreateUser("op"+strconv.FormatInt(time.Now().UnixNano(), 36)+"@example.com", hash, "operator")
	if err != nil {
		t.Fatal(err)
	}
	return op
}
