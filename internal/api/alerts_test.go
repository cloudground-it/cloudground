package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func certAlert(t *testing.T, alerts []Alert) (Alert, bool) {
	t.Helper()
	for _, a := range alerts {
		if a.Kind == "certificate" {
			return a, true
		}
	}
	return Alert{}, false
}

// tableAlerts is what the alerts table holds now, as an admin reading
// English sees it.
func tableAlerts(t *testing.T, s *Server) []Alert {
	t.Helper()
	list, err := s.Store.ListAlerts()
	if err != nil {
		t.Fatal(err)
	}
	out := []Alert{}
	for _, a := range list {
		out = append(out, alertView(a, "en"))
	}
	return out
}

// evaluateAt runs the evaluator as the alerts.notify job and the watcher
// do, at the time now.
func evaluateAt(t *testing.T, s *Server, now time.Time) {
	t.Helper()
	if err := s.evaluateAlertsAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}

// An expiring certificate's alert keeps its id while the day count in its
// message goes down (it used to hash the message, so a dismissal lasted a
// day and the webhook announced it again every day), and gets a new one
// when it escalates to an error.
func TestAlertIDIsStableAcrossDays(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := activeSite(t, s, "exp.example.com")
	now := time.Now()
	if err := s.Store.SetCertExpiry(site.ID, now.Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	evaluateAt(t, s, now)
	today, ok := certAlert(t, tableAlerts(t, s))
	if !ok {
		t.Fatal("no alert for a certificate 10 days from expiry")
	}
	evaluateAt(t, s, now.Add(48*time.Hour))
	later, ok := certAlert(t, tableAlerts(t, s))
	if !ok {
		t.Fatal("the alert went away two days later")
	}
	if today.Message == later.Message {
		t.Fatalf("precondition: the message should count down, got %q both days", today.Message)
	}
	if today.ID != later.ID {
		t.Fatalf("id changed with the day count: %s (%q) -> %s (%q)", today.ID, today.Message, later.ID, later.Message)
	}

	if resp, body := c.do("POST", "/api/system/alerts/"+today.ID+"/dismiss", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss = %d %s", resp.StatusCode, body)
	}
	if resp, _ := c.do("POST", "/api/system/alerts/"+today.ID+"/dismiss", map[string]string{}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dismissing it again = %d, want 404", resp.StatusCode)
	}
	evaluateAt(t, s, now.Add(72*time.Hour))
	if a, ok := certAlert(t, tableAlerts(t, s)); ok {
		t.Fatalf("a dismissed alert came back while its condition lasted: %+v", a)
	}
	evaluateAt(t, s, now.Add(8*24*time.Hour))
	if a, ok := certAlert(t, tableAlerts(t, s)); !ok || a.Level != "error" || a.ID == today.ID {
		t.Fatalf("escalation to error was not reported as a new alert: %+v (found %v)", a, ok)
	}
	// A renewed certificate clears it.
	if err := s.Store.SetCertExpiry(site.ID, now.Add(90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	evaluateAt(t, s, now.Add(8*24*time.Hour))
	if a, ok := certAlert(t, tableAlerts(t, s)); ok {
		t.Fatalf("a renewed certificate is still an alert: %+v", a)
	}
}

// The webhook sends an alert once per window, by its id, whatever its
// wording does meanwhile.
func TestWebhookDedupesByAlertID(t *testing.T) {
	s, _, _ := testServer(t)
	var posts atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1) }))
	defer hook.Close()
	site := activeSite(t, s, "hook.example.com")
	if err := s.Store.SetCertExpiry(site.ID, time.Now().Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.webhookClient = hook.Client() // the guarded client refuses loopback
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, hook.URL); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.runAlertsNotify(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("%d posts, want 1", n)
	}
	a, ok := certAlert(t, tableAlerts(t, s))
	if !ok {
		t.Fatal("no certificate alert")
	}
	if got, err := s.Store.GetAlert(a.ID); err != nil || got.NotifiedAt == nil {
		t.Fatalf("notification not recorded on the alert (%+v, %v)", got, err)
	}
	evaluateAt(t, s, time.Now().Add(26*time.Hour)) // a day less in the message
	s.notifyAlerts(ctx)
	if n := posts.Load(); n != 1 {
		t.Fatalf("the same alert was sent again within the window: %d posts", n)
	}
}

// GET /api/system/alerts reads the table: it asks the agent nothing, and
// shows an operator its own sites' alerts and no server-wide one.
func TestAlertsAreReadFromTheTable(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	mine, other := activeSite(t, s, "mine.example.com"), activeSite(t, s, "other.example.com")
	for _, site := range []state.Site{mine, other} {
		if err := s.Store.SetCertError(site.ID, "rate limited\nsecond line"); err != nil {
			t.Fatal(err)
		}
	}
	agent.mu.Lock()
	agent.results[agentrpc.ServiceStatus.Name()] = rpc.ServiceStatusResult{Services: []rpc.ServiceInfo{
		{Name: "nginx", State: "failed", Detail: "exit 1"}, {Name: "mariadb", State: "running"},
	}}
	agent.mu.Unlock()
	evaluateAt(t, s, time.Now())

	agent.mu.Lock()
	agent.calls = nil
	agent.mu.Unlock()
	var all []Alert
	resp, body := admin.do("GET", "/api/system/alerts", nil)
	if err := json.Unmarshal(body, &all); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("alerts = %d %s", resp.StatusCode, body)
	}
	if agent.called(agentrpc.ServiceStatus.Name()) {
		t.Fatal("reading the alerts asked the agent")
	}
	kinds := map[string]int{}
	for _, a := range all {
		kinds[a.Kind]++
	}
	if kinds["certificate"] != 2 || kinds["service"] != 1 || len(all) != 3 {
		t.Fatalf("admin sees %+v", all)
	}
	if all[0].Level != "error" || all[0].Message == "" || all[0].Text == "" {
		t.Fatalf("first alert %+v", all[0])
	}

	op, err := s.Store.CreateUser("op@example.com", "x", roleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetUserSites(op.ID, []int64{mine.ID}); err != nil {
		t.Fatal(err)
	}
	opc := &client{t: t, base: ts.URL, cookies: []*http.Cookie{{Name: insecureSessionName, Value: sessionFor(t, s, op.ID)}}}
	var seen []Alert
	_, body = opc.do("GET", "/api/system/alerts", nil)
	if err := json.Unmarshal(body, &seen); err != nil || len(seen) != 1 || seen[0].SiteID != mine.ID || seen[0].Site != mine.Domain ||
		seen[0].Message != "certificate renewal failed: rate limited" {
		t.Fatalf("operator sees %s", body)
	}
	// An alert it cannot see is not there to dismiss.
	for _, a := range all {
		if a.SiteID == mine.ID {
			continue
		}
		if resp, _ := opc.do("POST", "/api/system/alerts/"+a.ID+"/dismiss", map[string]string{}); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("operator dismissing %s alert = %d, want 404", a.Kind, resp.StatusCode)
		}
	}

	// A service that runs again clears its alert; an agent that does not
	// answer leaves the service alerts as they were.
	agent.mu.Lock()
	agent.errs[agentrpc.ServiceStatus.Name()] = errors.New("agent down")
	agent.mu.Unlock()
	if err := s.evaluateAlerts(context.Background()); err == nil {
		t.Fatal("the evaluator did not report the agent's failure")
	}
	if n := countKind(tableAlerts(t, s), "service"); n != 1 {
		t.Fatalf("service alerts after a failed read = %d, want the one kept", n)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ServiceStatus.Name())
	agent.results[agentrpc.ServiceStatus.Name()] = rpc.ServiceStatusResult{Services: []rpc.ServiceInfo{{Name: "nginx", State: "running"}}}
	agent.mu.Unlock()
	evaluateAt(t, s, time.Now())
	if n := countKind(tableAlerts(t, s), "service"); n != 0 {
		t.Fatalf("service alerts after nginx came back = %d", n)
	}
}

func countKind(list []Alert, kind string) int {
	n := 0
	for _, a := range list {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

// A failed task is an alert for a day, with its error in the reader's
// language when it has a key. One of a site deleted since (a creation that
// failed and was rolled back) is a server-wide alert named by the domain the
// job recorded: admins must see a failed creation.
func TestTaskAlerts(t *testing.T) {
	now := time.Now()
	list := []state.Job{
		{ID: 1, Kind: "wp.update", SiteID: 3, Status: state.JobFailed, Error: "disk full\ndetail", CreatedAt: now.Add(-time.Hour)},
		{ID: 2, Kind: "wp.update", Status: state.JobFailed, Error: "old", CreatedAt: now.Add(-25 * time.Hour)},
		{ID: 3, Kind: "metrics.sample", Status: state.JobFailed, Quiet: true, CreatedAt: now},
		{ID: 4, Kind: "site.create", Status: state.JobSucceeded, CreatedAt: now},
		{ID: 5, Kind: "migration.import", SiteID: 3, Status: state.JobFailed, Error: "too large",
			ErrorKey: "migration.tooLargeSize", ErrorArgs: []string{"1 GB"}, CreatedAt: now},
		{ID: 6, Kind: "site.create", SiteID: 9, Status: state.JobFailed, Error: "no", CreatedAt: now,
			Payload: `{"site_id":9,"domain":"lost.example.com"}`},
		{ID: 7, Kind: "site.create", SiteID: 8, Status: state.JobFailed, Error: "no", CreatedAt: now, Payload: `{}`},
	}
	got := taskConditions(list, map[int64]bool{3: true}, now)
	if len(got) != 4 || got[0].Key != "task:1" || got[0].Message != "WordPress update failed: disk full" || got[0].SiteID != 3 || got[1].Key != "task:5" {
		t.Fatalf("conditions = %+v", got)
	}
	if g := got[2]; g.SiteID != 0 || g.Subject != "lost.example.com" {
		t.Fatalf("a failed task of a deleted site = %+v, want no site and its domain", g)
	}
	if g := got[3]; g.SiteID != 0 || g.Subject != "site #8" {
		t.Fatalf("a failed task of a deleted site with no recorded domain = %+v", g)
	}
	a := state.Alert{Message: got[1].Message, Text: got[1].Text}
	if en, it := alertMessage(a, "en"), alertMessage(a, "it"); en == it || en == "" {
		t.Fatalf("the task's keyed error is not translated: en %q, it %q", en, it)
	}

	// Through the queue: the job records its site's domain, the site goes,
	// the alert stays, for admins.
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := activeSite(t, s, "rolled.example.com")
	if _, err := s.startJob(jobReq{kind: "site.create", siteID: site.ID, locks: siteLocks(site.ID)}, func(context.Context, state.Site, func(int, string)) error {
		return errors.New("provisioning failed")
	}); err != nil {
		t.Fatal(err)
	}
	waitForTasks(t, s)
	if err := s.Store.DeleteSite(site.ID); err != nil {
		t.Fatal(err)
	}
	evaluateAt(t, s, time.Now())
	var seen []Alert
	_, body := admin.do("GET", "/api/system/alerts", nil)
	_ = json.Unmarshal(body, &seen)
	if len(seen) != 1 || seen[0].Kind != "task" || seen[0].SiteID != 0 || seen[0].Site != "rolled.example.com" {
		t.Fatalf("admin sees %s, want the failed creation named by its domain", body)
	}
	op, err := s.Store.CreateUser("op@example.com", "x", roleOperator)
	if err != nil {
		t.Fatal(err)
	}
	opc := &client{t: t, base: ts.URL, cookies: []*http.Cookie{{Name: insecureSessionName, Value: sessionFor(t, s, op.ID)}}}
	if _, body := opc.do("GET", "/api/system/alerts", nil); string(body) != "[]\n" {
		t.Fatalf("an operator sees a server-wide alert: %s", body)
	}
}
