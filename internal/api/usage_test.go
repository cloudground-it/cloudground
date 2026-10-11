package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// SPEC §11.5: rates come from the previous sample when it is recent and the
// counters grew; the disk use is carried between two walks.
func TestUsageSample(t *testing.T) {
	now := time.Now()
	prev := state.UsageSample{SiteID: 3, SampledAt: now.Add(-300 * time.Second), CPUUsec: 1_000_000, IOReadBytes: 0, IOWriteBytes: 3000,
		DiskBytes: 5 << 20, DiskInodes: 9}
	u := rpc.SiteUsage{ID: 3, Running: true, CPUUsec: 151_000_000, IOReadBytes: 30000, IOWriteBytes: 3000, MemoryBytes: 7, DiskBytes: -1, DiskInodes: -1}
	got := usageSample(u, prev, now)
	if got.CPUCores != 0.5 || got.IOReadBps != 100 || got.IOWriteBps != 0 || got.DiskBytes != 5<<20 || got.DiskInodes != 9 || got.MemBytes != 7 {
		t.Errorf("sample %+v", got)
	}
	// A restarted slice (counters went back) or a long gap: no rate.
	for _, p := range []state.UsageSample{
		{SiteID: 3, SampledAt: now.Add(-300 * time.Second), CPUUsec: 999_000_000},
		{SiteID: 3, SampledAt: now.Add(-time.Hour), CPUUsec: 1},
	} {
		if got := usageSample(u, p, now); got.CPUCores != 0 || got.IOReadBps != 0 {
			t.Errorf("after %+v: %+v", p, got)
		}
	}
}

func usageRun(site int64, start time.Time, cores []float64, mem []int64) []state.UsageSample {
	var out []state.UsageSample
	for i := range cores {
		out = append(out, state.UsageSample{SiteID: site, SampledAt: start.Add(time.Duration(i) * 5 * time.Minute),
			CPUCores: cores[i], MemBytes: mem[i], MemMaxBytes: 1000, DiskBytes: 300 << 20})
	}
	return out
}

// SPEC §11.5: three samples in a row over the threshold are an alert whose
// id lasts as long as the run; one dip ends it; Off, the quota and the disk
// threshold are honoured.
func TestUsageConditions(t *testing.T) {
	start := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	site := state.Site{ID: 3, Status: state.SiteActive}
	busy := map[int64][]state.UsageSample{3: usageRun(3, start, []float64{0.1, 1.9, 1.9, 1.95}, []int64{100, 950, 950, 990})}
	set := usageConditions([]state.Site{site}, busy, 2)
	if len(set) != 2 || set[0].Key != "usage.cpu:site:3" || set[1].Key != "usage.memory:site:3" {
		t.Fatalf("conditions %+v", set)
	}
	if set[0].Fingerprint != start.Add(5*time.Minute).Format(time.RFC3339) || set[0].Text.Args[0] != "98" {
		t.Errorf("cpu alert %+v", set[0])
	}
	// One sample longer: the same run, the same fingerprint.
	longer := map[int64][]state.UsageSample{3: usageRun(3, start, []float64{0.1, 1.9, 1.9, 1.95, 1.9}, []int64{100, 950, 950, 990, 990})}
	if again := usageConditions([]state.Site{site}, longer, 2); again[0].Fingerprint != set[0].Fingerprint {
		t.Errorf("the run's alert changed id: %s then %s", set[0].Fingerprint, again[0].Fingerprint)
	}
	// Two samples are a spike, not an alert.
	short := map[int64][]state.UsageSample{3: usageRun(3, start, []float64{0.1, 0.1, 1.9, 1.9}, []int64{1, 1, 990, 990})}
	if set := usageConditions([]state.Site{site}, short, 2); len(set) != 0 {
		t.Errorf("a ten-minute spike alerted: %+v", set)
	}
	// With a quota of half a CPU, 0.48 cores is 96 % of the allowance.
	quota := site
	quota.Config.Resources.CPUQuotaPct = 50
	q := map[int64][]state.UsageSample{3: usageRun(3, start, []float64{0.48, 0.48, 0.48}, []int64{1, 1, 1})}
	if set := usageConditions([]state.Site{quota}, q, 2); len(set) != 1 || set[0].Key != "usage.cpu:site:3" {
		t.Errorf("quota: %+v", set)
	}
	if set := usageConditions([]state.Site{site}, q, 2); len(set) != 0 {
		t.Errorf("0.48 of 2 CPUs alerted: %+v", set)
	}
	// Off, and the disk threshold.
	off := site
	off.Config.Resources.Alerts.Off = true
	if set := usageConditions([]state.Site{off}, busy, 2); len(set) != 0 {
		t.Errorf("alerts off but %+v", set)
	}
	disk := site
	disk.Config.Resources.Alerts = state.UsageAlerts{CPUPct: 100, MemoryPct: 100, DiskMB: 200}
	if set := usageConditions([]state.Site{disk}, short, 2); len(set) != 1 || set[0].Key != "usage.disk:site:3" || set[0].Text.Args[0] != "300" {
		t.Errorf("disk: %+v", set)
	}
}

// #19: the limits are an administrator's; values out of bounds are refused
// field by field; the thresholds alone change no file.
func TestPatchLimitsAndUsageAlerts(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "limits.example.com", state.SiteStatic, "", 0)
	resp, body := admin.do("POST", "/api/users", map[string]any{
		"email": "op@example.com", "password": "operator-password!", "role": "operator", "site_ids": []int64{site.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d %s", resp.StatusCode, body)
	}
	op := &client{t: t, base: ts.URL}
	if resp, body := op.do("POST", "/api/login", map[string]string{"email": "op@example.com", "password": "operator-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %s", resp.StatusCode, body)
	}
	code, a := patchSite(t, op, site.ID, map[string]any{"memory_max_mb": 4096, "cpu_quota_pct": 400, "tasks_max": 4096})
	if code != http.StatusForbidden || len(a.Fields) != 3 {
		t.Fatalf("operator raising limits = %d %+v", code, a)
	}
	code, a = patchSite(t, admin, site.ID, map[string]any{"memory_max_mb": 64, "cpu_quota_pct": 5, "tasks_max": 1,
		"usage_alerts": map[string]any{"cpu_pct": 120}})
	if code != http.StatusBadRequest || len(a.Fields) != 4 {
		t.Fatalf("out of bounds = %d %+v", code, a)
	}
	if code, a = patchSite(t, admin, site.ID, map[string]any{"memory_max_mb": 256, "cpu_quota_pct": 50, "tasks_max": 64}); code != http.StatusAccepted {
		t.Fatalf("admin = %d %+v", code, a)
	}
	waitForTasks(t, s)
	got, _ := s.Store.GetSite(site.ID)
	if r := got.Config.Resources; r.MemoryMaxMB != 256 || r.CPUQuotaPct != 50 || r.TasksMax != 64 || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("resources %+v, applied %d desired %d", r, got.AppliedGeneration, got.DesiredGeneration)
	}

	renders := countCalls(agent, agentrpc.ApplySiteConfig.Name())
	if code, a = patchSite(t, op, site.ID, map[string]any{"usage_alerts": map[string]any{"cpu_pct": 80, "disk_mb": 500}}); code != http.StatusAccepted {
		t.Fatalf("operator thresholds = %d %+v", code, a)
	}
	waitForTasks(t, s)
	got, _ = s.Store.GetSite(site.ID)
	if got.Config.Resources.Alerts != (state.UsageAlerts{CPUPct: 80, DiskMB: 500}) || got.AppliedGeneration != got.DesiredGeneration {
		t.Fatalf("alerts %+v, applied %d desired %d", got.Config.Resources.Alerts, got.AppliedGeneration, got.DesiredGeneration)
	}
	if countCalls(agent, agentrpc.ApplySiteConfig.Name()) != renders {
		t.Error("a threshold change rendered the site")
	}
}

// SPEC §11.5: the usage page's answer, by range.
func TestSiteUsageRoute(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "usage.example.com", state.SitePHP, "", 0)
	now := time.Now()
	if err := s.Store.RecordUsage([]state.UsageSample{
		{SiteID: site.ID, SampledAt: now.Add(-10 * time.Minute), CPUCores: 0.5, MemBytes: 100 << 20, MemMaxBytes: 1000 << 20, DiskBytes: -1, DiskInodes: -1},
		{SiteID: site.ID, SampledAt: now.Add(-5 * time.Minute), CPUCores: 1, MemBytes: 200 << 20, MemMaxBytes: 1000 << 20, DiskBytes: 7 << 20, DiskInodes: 5},
	}); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Range  string `json:"range"`
		CPUs   int    `json:"cpus"`
		Points []struct {
			CPUPct float64 `json:"cpu_pct"`
			MemPct float64 `json:"mem_pct"`
		} `json:"points"`
		Latest *state.UsageSample `json:"latest"`
	}
	resp, body := c.do("GET", "/api/sites/"+strconv.FormatInt(site.ID, 10)+"/usage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Range != "24h" || len(v.Points) != 2 || v.Points[1].MemPct != 20 || v.Latest == nil || v.Latest.DiskBytes != 7<<20 {
		t.Fatalf("24h = %s", body)
	}
	if want := 100 / float64(v.CPUs); v.Points[1].CPUPct < want-0.1 || v.Points[1].CPUPct > want+0.1 {
		t.Errorf("one core of %d CPUs read as %v %%", v.CPUs, v.Points[1].CPUPct)
	}
	resp, body = c.do("GET", "/api/sites/"+strconv.FormatInt(site.ID, 10)+"/usage?range=30d", nil)
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || v.Range != "30d" || len(v.Points) == 0 {
		t.Fatalf("30d = %d %s", resp.StatusCode, body)
	}
	if resp, _ := c.do("GET", "/api/sites/"+strconv.FormatInt(site.ID, 10)+"/usage?range=1y", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("range 1y = %d", resp.StatusCode)
	}
}
