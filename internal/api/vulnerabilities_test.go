package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Versions order as PHP's version_compare orders them, which is how
// WordPress and the database read them.
func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"5.3.1", "5.3.2", -1},
		{"5.3.10", "5.3.9", 1},
		{"6.4", "6.4.0", -1},
		{"6.4.1", "6.4.1", 0},
		{"6.9-RC1", "6.9", -1},
		{"6.9-beta2", "6.9-RC1", -1},
		{"6.9-alpha", "6.9-beta", -1},
		{"1.0pl1", "1.0", 1},
		{"2.0.0-dev", "2.0.0-alpha", -1},
		{"10.0", "9.9.9", 1},
		{"1.2a", "1.2", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareVersions(c.b, c.a); got != -c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

// The ranges of the database's operators, and what a record says is the
// fix.
func TestVulnRecordAffects(t *testing.T) {
	for _, c := range []struct {
		r    vulnRecord
		v    string
		want bool
	}{
		{vulnRecord{maxV: "5.3.2", maxOp: "lt"}, "5.3.1", true},
		{vulnRecord{maxV: "5.3.2", maxOp: "lt"}, "5.3.2", false},
		{vulnRecord{maxV: "1.1.3", maxOp: "le"}, "1.1.3", true},
		{vulnRecord{minV: "2.3", minOp: "ge", maxV: "2.3.5", maxOp: "le"}, "2.2", false},
		{vulnRecord{minV: "2.3", minOp: "ge", maxV: "2.3.5", maxOp: "le"}, "2.3.1", true},
		{vulnRecord{maxV: "7.1.0", maxOp: "eq", unfixed: true}, "7.1.0", true},
		{vulnRecord{maxV: "7.1.0", maxOp: "eq", unfixed: true}, "7.1.1", false},
		{vulnRecord{minV: "3.0", minOp: "gt"}, "3.1", true},
		{vulnRecord{maxV: "9", maxOp: "neq"}, "1", false}, // unknown operator: no guess
		{vulnRecord{}, "1.0", false},                      // no range at all
		{vulnRecord{always: true}, "6.4.1", true},         // listed for the very version
	} {
		if got := c.r.affects(c.v); got != c.want {
			t.Errorf("%+v affects %s = %v, want %v", c.r, c.v, got, c.want)
		}
	}
	if f := (vulnRecord{maxV: "5.3.2", maxOp: "lt"}).fixedIn(); f != "5.3.2" {
		t.Errorf("fixed in %q", f)
	}
	if f := (vulnRecord{maxV: "5.3.2", maxOp: "le"}).fixedIn(); f != "" {
		t.Errorf("<= names no fix, got %q", f)
	}
	if f := (vulnRecord{maxV: "5.3.2", maxOp: "lt", unfixed: true}).fixedIn(); f != "" {
		t.Errorf("unfixed names no fix, got %q", f)
	}
}

// Answers in the database's shapes (trimmed from real ones, October 2026).
const (
	wpvPlugin = `{"error":0,"message":null,"data":{"name":"Contact Form 7","plugin":"contact-form-7","vulnerability":[
	{"uuid":"u-upload","name":"Contact Form 7 [contact-form-7] < 5.3.2","description":null,
	 "operator":{"min_version":null,"min_operator":null,"max_version":"5.3.2","max_operator":"lt","unfixed":"0","closed":"0"},
	 "source":[{"id":"CVE-2020-35489","name":"CVE-2020-35489","link":"https://www.cve.org/CVERecord?id=CVE-2020-35489"},
	           {"id":"c4ea","name":"Contact Form 7 &lt;= 5.3.1 - Arbitrary File Upload","link":"https://www.wordfence.com/threat-intel/x"}],
	 "impact":{"cvss":{"version":"3.1","score":"10.0","severity":"c"},"cvss3":{"severity":"critical"}}},
	{"uuid":"u-old","name":"Contact Form 7 [contact-form-7] < 5.0.4",
	 "operator":{"min_version":null,"min_operator":null,"max_version":"5.0.4","max_operator":"lt","unfixed":"0","closed":"0"},
	 "source":[{"id":"CVE-2018-20979","name":"CVE-2018-20979","link":"https://www.cve.org/x"}],"impact":[]},
	{"uuid":"u-range","name":"Contact Form 7 [contact-form-7] >= 5.0 - <= 5.4",
	 "operator":{"min_version":"5.0","min_operator":"ge","max_version":"5.4","max_operator":"le","unfixed":"1","closed":"0"},
	 "source":[{"id":"abc","name":"Contact Form 7 5.0-5.4 - Reflected XSS","link":"javascript:alert(1)"}],
	 "impact":{"cvss3":{"severity":"medium"}}}]},"updated":1791714983}`
	wpvCore = `{"error":0,"message":null,"data":{"core":"6.4.1","vulnerability":[
	{"uuid":"core-1","name":"6.4.1","description":null,"source":[{"id":"CVE-2024-31210","name":"CVE-2024-31210","link":"https://www.cve.org/y"}],
	 "impact":{"cvss":{"severity":"h"}}}]},"updated":1}`
	wpvUnknown = `{"error":0,"message":null,"data":{"name":null,"plugin":"nothing","link":null,"latest":null,"closed":null,"vulnerability":null},"updated":1}`
)

func TestParseVulnAnswer(t *testing.T) {
	list, err := parseVulnAnswer([]byte(wpvPlugin), false)
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	r := list[0]
	if r.title != "Contact Form 7 <= 5.3.1 - Arbitrary File Upload" || r.cve != "CVE-2020-35489" || r.severity != "critical" ||
		r.link != "https://www.wordfence.com/threat-intel/x" || r.maxV != "5.3.2" || r.maxOp != "lt" || r.unfixed {
		t.Errorf("first record %+v", r)
	}
	if list[1].severity != "" || list[1].title != "Contact Form 7 [contact-form-7] < 5.0.4" || list[1].cve != "CVE-2018-20979" {
		t.Errorf("a record without impact or a titled source: %+v", list[1])
	}
	if list[2].link != "" || !list[2].unfixed || list[2].severity != "medium" {
		t.Errorf("a link that is not https stays out; unfixed is read: %+v", list[2])
	}
	core, err := parseVulnAnswer([]byte(wpvCore), true)
	if err != nil || len(core) != 1 || !core[0].always || core[0].severity != "high" {
		t.Errorf("core %+v %v", core, err)
	}
	if none, err := parseVulnAnswer([]byte(wpvUnknown), false); err != nil || len(none) != 0 {
		t.Errorf("an unknown plugin %+v %v", none, err)
	}
	if _, err := parseVulnAnswer([]byte(`{"error":1,"message":"Nothing here.","data":null}`), false); err == nil {
		t.Error("an error answer read as no vulnerabilities")
	}
	if _, err := parseVulnAnswer([]byte(`<html>`), false); err == nil {
		t.Error("HTML read as no vulnerabilities")
	}
}

// fakeVulnDB serves the database's paths from a map and counts requests.
func fakeVulnDB(t *testing.T, s *Server, answers map[string]string) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "CloudGround/") {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		mu.Lock()
		body, ok := answers[r.URL.Path]
		mu.Unlock()
		if !ok {
			body = wpvUnknown
		}
		if body == "500" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	db := newVulnDB()
	db.base, db.client = ts.URL, ts.Client()
	s.vulnSource = db
	return &hits
}

// SPEC §11.6: the check reads the site's versions from the agent, looks up
// core, plugins and themes (not a must-use plugin or a drop-in), stores
// what affects the installed versions, and raises one alert for the site;
// a failure keeps what was found and records why; an update that fixes
// everything clears the alert.
func TestVulnCheckJob(t *testing.T) {
	s, agent, _ := testServer(t)
	hits := fakeVulnDB(t, s, map[string]string{
		"/plugin/contact-form-7/": wpvPlugin,
		"/core/6.4.1/":            wpvCore,
	})
	site := seedActiveSite(t, s, "vuln.example.com", state.SiteWordPress, "8.3", 0)
	agent.setResult(agentrpc.WPPlugins.Name(), rpc.WPPluginsResult{Core: "6.4.1",
		Plugins: []rpc.WPPlugin{
			{Name: "contact-form-7", Status: "inactive", Version: "5.3.1"},
			{Name: "akismet", Status: "active", Version: "5.3"},
			{Name: "cloudground-connector", Status: "must-use", Version: "1.0"},
			{Name: "object-cache.php", Status: "dropin", Version: ""},
		},
		Themes: []rpc.WPPlugin{{Name: "twentytwentyfour", Status: "active", Version: "1.2"}},
	})
	run := func() state.Job {
		return runJobNow(t, s, jobs.Spec{Kind: "wp.vulncheck", SiteID: site.ID, Locks: []string{vulnLock}, Payload: sitePayload{SiteID: site.ID}})
	}
	if j := run(); j.Status != state.JobSucceeded {
		t.Fatalf("check = %s %s", j.Status, j.Error)
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("%d lookups, want 4 (core, two plugins, one theme)", n)
	}
	rep, ok, err := s.Store.SiteVulnReport(site.ID)
	if err != nil || !ok || rep.Components != 4 || rep.Core != "6.4.1" || len(rep.Findings) != 3 {
		t.Fatalf("report %+v %v %v", rep, ok, err)
	}
	// Most severe first: the critical upload, the high core one, the medium XSS.
	f := rep.Findings
	if f[0].ID != "u-upload" || f[0].FixedIn != "5.3.2" || f[0].Slug != "contact-form-7" || f[0].Version != "5.3.1" ||
		f[1].Component != "core" || f[1].Version != "6.4.1" || f[2].ID != "u-range" || !f[2].Unfixed {
		t.Errorf("findings %+v", f)
	}
	if err := s.evaluateAlerts(t.Context()); err != nil {
		t.Fatal(err)
	}
	alerts := alertsOfKind(t, s, "vulnerability")
	if len(alerts) != 1 || alerts[0].Level != "error" || alerts[0].SiteID != site.ID ||
		!strings.Contains(alerts[0].Message, "3 known vulnerabilities") || !strings.Contains(alerts[0].Message, "contact-form-7 5.3.1") {
		t.Fatalf("alerts %+v", alerts)
	}
	first := alerts[0].ID

	// Answers are reused for six hours: a second check sends nothing.
	if j := run(); j.Status != state.JobSucceeded || hits.Load() != 4 {
		t.Errorf("second check = %s, %d lookups", j.Status, hits.Load())
	}
	_ = s.evaluateAlerts(t.Context())
	if a := alertsOfKind(t, s, "vulnerability"); len(a) != 1 || a[0].ID != first {
		t.Errorf("the same findings changed the alert: %+v", a)
	}

	// The database fails: the check fails, the findings stay, the error is
	// recorded.
	s.vulnSource.mu.Lock()
	s.vulnSource.cache = nil
	s.vulnSource.mu.Unlock()
	fakeVulnDB(t, s, map[string]string{"/plugin/akismet/": "500"})
	if j := run(); j.Status != state.JobFailed || !strings.Contains(j.Error, "500") {
		t.Errorf("a failed lookup = %s %q", j.Status, j.Error)
	}
	rep, _, _ = s.Store.SiteVulnReport(site.ID)
	if len(rep.Findings) != 3 || !strings.Contains(rep.Error, "500") {
		t.Errorf("after a failure: %+v", rep)
	}

	// Updated: nothing found, the alert is cleared.
	agent.setResult(agentrpc.WPPlugins.Name(), rpc.WPPluginsResult{Core: "6.8.3",
		Plugins: []rpc.WPPlugin{{Name: "contact-form-7", Status: "active", Version: "6.1"}}})
	fakeVulnDB(t, s, map[string]string{"/plugin/contact-form-7/": wpvPlugin})
	if j := run(); j.Status != state.JobSucceeded {
		t.Fatalf("check = %s %s", j.Status, j.Error)
	}
	_ = s.evaluateAlerts(t.Context())
	if a := alertsOfKind(t, s, "vulnerability"); len(a) != 0 {
		t.Errorf("fixed but still alerted: %+v", a)
	}

	// The agent cannot read the site: the check fails and says why.
	agent.setErr(agentrpc.WPPlugins.Name(), errors.New("wp plugin list: exit 255"))
	if j := run(); j.Status != state.JobFailed {
		t.Errorf("an unreadable site = %s", j.Status)
	}
}

func alertsOfKind(t *testing.T, s *Server, kind string) []state.Alert {
	t.Helper()
	list, err := s.Store.ListAlerts()
	if err != nil {
		t.Fatal(err)
	}
	var out []state.Alert
	for _, a := range list {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// The sweep queues each active WordPress site once a day, and nothing at
// all with the switch off.
func TestVulnSweep(t *testing.T) {
	s, agent, _ := testServer(t)
	fakeVulnDB(t, s, nil)
	agent.setResult(agentrpc.WPPlugins.Name(), rpc.WPPluginsResult{Core: "6.8.3"})
	wp := seedActiveSite(t, s, "wp.example.com", state.SiteWordPress, "8.3", 0)
	seedActiveSite(t, s, "static.example.com", state.SiteStatic, "", 0)
	sweep := func() { runJobNow(t, s, jobs.Spec{Kind: "security.sweep"}) }
	checks := func() int {
		n := 0
		list, _ := s.Store.ListJobs(100, true)
		for _, j := range list {
			if j.Kind == "wp.vulncheck" {
				n++
				if j.SiteID != wp.ID {
					t.Errorf("a check of site %d", j.SiteID)
				}
			}
		}
		return n
	}
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.VulnerabilityCheck = false; return nil }); err != nil {
		t.Fatal(err)
	}
	sweep()
	if n := checks(); n != 0 {
		t.Fatalf("switch off, %d checks", n)
	}
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error { p.VulnerabilityCheck = true; return nil }); err != nil {
		t.Fatal(err)
	}
	sweep()
	sweep()
	if n := checks(); n != 1 {
		t.Fatalf("%d checks after two sweeps, want 1", n)
	}
	if last, _ := s.Store.LastScheduleRun(vulnScheduleKey(wp.ID)); time.Since(last) > time.Minute {
		t.Errorf("the site's check was not recorded: %v", last)
	}
}

// GET and POST /api/sites/{id}/vulnerabilities: a WordPress site only; the
// check needs the switch on; an operator of another site sees nothing.
func TestVulnerabilityRoutes(t *testing.T) {
	s, agent, ts := testServer(t)
	fakeVulnDB(t, s, map[string]string{"/plugin/contact-form-7/": wpvPlugin})
	agent.setResult(agentrpc.WPPlugins.Name(), rpc.WPPluginsResult{Core: "6.8.3",
		Plugins: []rpc.WPPlugin{{Name: "contact-form-7", Status: "active", Version: "5.0.1"}}})
	admin := setupAdmin(t, s, ts)
	wp := seedActiveSite(t, s, "wp.example.com", state.SiteWordPress, "8.3", 0)
	static := seedActiveSite(t, s, "static.example.com", state.SiteStatic, "", 0)

	resp, body := admin.do("GET", "/api/sites/"+strconv.FormatInt(wp.ID, 10)+"/vulnerabilities", nil)
	var v struct {
		Enabled  bool                `json:"enabled"`
		Checked  bool                `json:"checked"`
		Findings []state.VulnFinding `json:"findings"`
		Source   map[string]string   `json:"source"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || !v.Enabled || v.Checked || v.Findings == nil || v.Source["name"] != vulnDBName {
		t.Fatalf("before a check = %d %s", resp.StatusCode, body)
	}
	if resp, body := admin.do("POST", "/api/sites/"+strconv.FormatInt(wp.ID, 10)+"/vulnerabilities/check", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("check = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	_, body = admin.do("GET", "/api/sites/"+strconv.FormatInt(wp.ID, 10)+"/vulnerabilities", nil)
	if json.Unmarshal(body, &v) != nil || !v.Checked || len(v.Findings) != 3 {
		t.Fatalf("after = %s", body)
	}
	if resp, _ := admin.do("GET", "/api/sites/"+strconv.FormatInt(static.ID, 10)+"/vulnerabilities", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a static site = %d", resp.StatusCode)
	}
	if resp, _ := admin.do("PUT", "/api/settings", map[string]string{"vulnerability_check": "0"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("turning the check off = %d", resp.StatusCode)
	}
	if resp, body := admin.do("POST", "/api/sites/"+strconv.FormatInt(wp.ID, 10)+"/vulnerabilities/check", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("check with the switch off = %d %s", resp.StatusCode, body)
	}
	if resp, _ := admin.do("PUT", "/api/settings", map[string]string{"vulnerability_check": "maybe"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad switch value = %d", resp.StatusCode)
	}
}
