package state

import (
	"errors"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// An alert is one row per condition: the same occurrence seen again keeps
// its id and its dismissal, a new occurrence under the same key replaces it
// and shows again, and a sync clears what is gone, each with its event.
func TestAlertLifecycle(t *testing.T) {
	s := testStore(t)
	bus := events.New(0)
	s.SetEvents(bus)
	site := newSite(t, s, "al.example.com")
	sub, _, _ := bus.Subscribe("", 64)
	defer sub.Close()
	next := func(want events.Type) events.Event {
		t.Helper()
		for {
			select {
			case ev := <-sub.C():
				if ev.Type == want {
					return ev
				}
			case <-time.After(time.Second):
				t.Fatalf("no %s", want)
			}
		}
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case ev := <-sub.C():
			if ev.Type == events.AlertRaised || ev.Type == events.AlertCleared {
				t.Fatalf("%s published %s", what, ev.Type)
			}
		default:
		}
	}
	expiry := func(days int, fp string) Alert {
		return Alert{Key: "certificate.expiry:site:1", Kind: "certificate", SiteID: site.ID, Level: "warning", Fingerprint: fp,
			Message: "expires", Text: AlertText{Key: "alert.certExpires", Counted: true, Count: days}}
	}

	if err := s.SyncAlerts("certificate", []Alert{expiry(10, "2026-12-01|warning")}); err != nil {
		t.Fatal(err)
	}
	raised := next(events.AlertRaised)
	if raised.SiteID != site.ID {
		t.Fatalf("raised about site %d", raised.SiteID)
	}
	list, _ := s.ListAlerts()
	if len(list) != 1 || list[0].Site != site.Domain || !list[0].Text.Counted || list[0].Text.Count != 10 {
		t.Fatalf("alerts = %+v", list)
	}
	first := list[0].ID
	if first != AlertID("certificate.expiry:site:1", "2026-12-01|warning") {
		t.Fatalf("id %s is not the hash of key and fingerprint", first)
	}

	// The same occurrence a day later: same id, new count, no event.
	if err := s.SyncAlerts("certificate", []Alert{expiry(9, "2026-12-01|warning")}); err != nil {
		t.Fatal(err)
	}
	quiet("a refresh")
	if list, _ := s.ListAlerts(); len(list) != 1 || list[0].ID != first || list[0].Text.Count != 9 {
		t.Fatalf("after a refresh: %+v", list)
	}

	if err := s.DismissAlert(first); err != nil {
		t.Fatal(err)
	}
	if ev := next(events.AlertCleared); ev.SiteID != site.ID {
		t.Fatalf("cleared about site %d", ev.SiteID)
	}
	if err := s.DismissAlert(first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second dismissal = %v, want ErrNotFound", err)
	}
	if err := s.SyncAlerts("certificate", []Alert{expiry(8, "2026-12-01|warning")}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListAlerts(); len(list) != 0 {
		t.Fatalf("a dismissed alert came back while its condition lasted: %+v", list)
	}

	// Escalation is a new occurrence: a new id, shown again.
	if err := s.SyncAlerts("certificate", []Alert{expiry(2, "2026-12-01|error")}); err != nil {
		t.Fatal(err)
	}
	next(events.AlertRaised)
	list, _ = s.ListAlerts()
	if len(list) != 1 || list[0].ID == first {
		t.Fatalf("escalation: %+v", list)
	}

	// Another kind's sync leaves this one alone; its own empty sync clears it.
	if err := s.SyncAlerts("drift", nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListAlerts(); len(list) != 1 {
		t.Fatalf("a drift sync touched a certificate alert: %+v", list)
	}
	if err := s.SyncAlerts("certificate", nil); err != nil {
		t.Fatal(err)
	}
	next(events.AlertCleared)
	if list, _ := s.ListAlerts(); len(list) != 0 {
		t.Fatalf("a cleared condition is still listed: %+v", list)
	}
}

// Errors come first, then the newest; an alert about a site that is gone is
// not raised; notification is due once per window.
func TestAlertOrderSitesAndNotification(t *testing.T) {
	s := testStore(t)
	site := newSite(t, s, "n.example.com")
	for _, a := range []Alert{
		{Key: "drift", Kind: "drift", Level: "warning", Message: "drift"},
		{Key: "service:nginx", Kind: "service", Level: "error", Fingerprint: "failed", Message: "nginx"},
		{Key: "task:7", Kind: "task", SiteID: site.ID, Level: "error", Message: "task",
			Text: AlertText{Key: "alert.taskFailed", Args: []string{"wp update", "boom"}, DetailKey: "k", DetailArgs: []string{"a"}}},
		{Key: "task:8", Kind: "task", SiteID: site.ID + 100, Level: "error", Message: "gone"},
	} {
		if err := s.RaiseAlert(a); err != nil {
			t.Fatalf("%s: %v", a.Key, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	list, err := s.ListAlerts()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, a := range list {
		keys = append(keys, a.Key)
	}
	if len(keys) != 3 || keys[0] != "task:7" || keys[1] != "service:nginx" || keys[2] != "drift" {
		t.Fatalf("order = %v, want task:7, service:nginx, drift (and no alert about a missing site)", keys)
	}
	if a := list[0]; a.SiteID != site.ID || a.Site != site.Domain || len(a.Text.Args) != 2 || a.Text.DetailKey != "k" || a.Text.Counted {
		t.Fatalf("task alert read back as %+v", a)
	}

	due, _ := s.AlertsToNotify(time.Now().Add(-6 * time.Hour))
	if len(due) != 3 {
		t.Fatalf("due = %d, want 3", len(due))
	}
	if err := s.MarkAlertsNotified([]string{due[0].ID, due[1].ID}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.AlertsToNotify(time.Now().Add(-6 * time.Hour)); len(due) != 1 || due[0].Key != "drift" {
		t.Fatalf("due after notifying two = %+v", due)
	}
	if due, _ := s.AlertsToNotify(time.Now().Add(time.Minute)); len(due) != 3 {
		t.Fatalf("due once the window passed = %d, want 3", len(due))
	}
	if err := s.ClearAlert("service:nginx"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAlert("service:nginx"); err != nil {
		t.Fatalf("clearing an absent alert: %v", err)
	}
	if _, err := s.GetAlert(AlertID("service:nginx", "failed")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared alert still readable: %v", err)
	}
}

// A connector token is found by its hash, only as a connector token, and a
// token sealed before the index existed is indexed at the next Open.
func TestSiteForConnectorToken(t *testing.T) {
	s := testStore(t)
	a, b := newSite(t, s, "a.example.com"), newSite(t, s, "b.example.com")
	for _, x := range []struct {
		site Site
		kind string
		v    string
	}{{a, SiteSecretConnectorToken, "tok-a"}, {b, SiteSecretConnectorToken, "tok-b"}, {b, SiteSecretDBPassword, "tok-db"}} {
		if err := s.SetSecret(SiteSecret(x.site.ID, x.kind), x.v); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.SiteForConnectorToken("tok-b"); err != nil || got.ID != b.ID {
		t.Fatalf("tok-b = %d, %v", got.ID, err)
	}
	for _, bad := range []string{"", "tok-c", "tok-db"} {
		if _, err := s.SiteForConnectorToken(bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q = %v, want ErrNotFound", bad, err)
		}
	}
	var indexed int
	s.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE lookup IS NOT NULL`).Scan(&indexed)
	if indexed != 2 {
		t.Fatalf("%d secrets indexed, want the two connector tokens", indexed)
	}
	// A new token replaces the old one in the index.
	if err := s.SetSecret(SiteSecret(a.ID, SiteSecretConnectorToken), "tok-a2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SiteForConnectorToken("tok-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the replaced token still opens: %v", err)
	}
	// As a database from before migration 19 has it.
	if _, err := s.db.Exec(`UPDATE secrets SET lookup = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SiteForConnectorToken("tok-a2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("precondition: an unindexed token is not found")
	}
	if err := s.indexConnectorTokens(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SiteForConnectorToken("tok-a2"); err != nil || got.ID != a.ID {
		t.Fatalf("after indexing: %d, %v", got.ID, err)
	}
	if err := s.indexConnectorTokens(); err != nil {
		t.Fatalf("a second pass: %v", err)
	}
}

// The store keeps a day of samples and returns them oldest first.
func TestMetricsKeepADay(t *testing.T) {
	s := testStore(t)
	if list, err := s.ListMetrics(0); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty = %v, %v; want an empty, non-nil list", list, err)
	}
	for i := 1; i <= metricsKept+12; i++ {
		if err := s.RecordMetric(MetricSample{CPUHeadroomPct: i}); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM metric_samples`).Scan(&rows)
	if rows != metricsKept {
		t.Fatalf("%d rows kept, want %d", rows, metricsKept)
	}
	list, _ := s.ListMetrics(0)
	if len(list) != metricsKept || list[0].CPUHeadroomPct != 13 || list[len(list)-1].CPUHeadroomPct != metricsKept+12 {
		t.Fatalf("got %d samples from %d to %d", len(list), list[0].CPUHeadroomPct, list[len(list)-1].CPUHeadroomPct)
	}
	if last3, _ := s.ListMetrics(3); len(last3) != 3 || last3[0].CPUHeadroomPct != metricsKept+10 {
		t.Fatalf("newest three = %+v", last3)
	}
}

// Right after the upgrade to the alerts table the evaluator re-raises what
// the old panel already announced: those rows count as notified, so the
// first notify does not repeat them; once the first evaluation ended, a new
// alert is due as usual. A fresh panel has no such window.
func TestAlertsAfterTheUpgradeCountAsNotified(t *testing.T) {
	s := testStore(t)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key=?`, metaAlertsUpgraded).Scan(&n)
	if n != 0 {
		t.Fatal("a fresh panel is marked as upgraded")
	}
	if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, '1')`, metaAlertsUpgraded); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseAlert(Alert{Key: "drift", Kind: "drift", Level: "warning", Message: "old"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.AlertsToNotify(time.Now().Add(-time.Hour)); len(due) != 0 {
		t.Fatalf("an alert re-raised by the upgrade is due: %+v", due)
	}
	if err := s.EndAlertsUpgrade(); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseAlert(Alert{Key: "service:nginx", Kind: "service", Level: "error", Message: "new"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.AlertsToNotify(time.Now().Add(-time.Hour)); len(due) != 1 || due[0].Key != "service:nginx" {
		t.Fatalf("due after the upgrade window = %+v", due)
	}
}

// An upgraded panel (it has accounts) is marked by migration 19.
func TestMigration19MarksAnUpgrade(t *testing.T) {
	f := buildLegacy(t)
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key=?`, metaAlertsUpgraded).Scan(&n)
	if n != 1 {
		t.Fatal("an upgraded panel is not marked")
	}
}

// Two legacy sites with one connector token (never minted so) must not stop
// the panel from opening: the first keeps the index, the second is
// unindexed and its purges are refused.
func TestDuplicateLegacyConnectorTokenOpens(t *testing.T) {
	f := buildLegacy(t)
	old, err := openAtVersion(f.path, versionStateV1-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`UPDATE settings SET value='live-conn-Jj34' WHERE key=?`, SiteSecret(f.stg, SiteSecretConnectorToken)); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(f.path)
	if err != nil {
		t.Fatalf("open with a duplicate legacy token: %v", err)
	}
	defer s.Close()
	site, err := s.SiteForConnectorToken("live-conn-Jj34")
	if err != nil || (site.ID != f.live && site.ID != f.stg) {
		t.Fatalf("the token = %d, %v", site.ID, err)
	}
	for _, id := range []int64{f.live, f.stg} {
		if v, err := s.Secret(SiteSecret(id, SiteSecretConnectorToken)); err != nil || v != "live-conn-Jj34" {
			t.Fatalf("site %d's token = %q, %v: both are kept", id, v, err)
		}
	}
}
