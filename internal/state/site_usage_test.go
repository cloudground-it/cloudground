package state

import (
	"testing"
	"time"
)

// Samples are kept for a day and folded into hours kept for 30 days; a
// deleted site takes its rows with it.
func TestSiteUsageWindows(t *testing.T) {
	s := testStore(t)
	a := seedSite(t, s, "a.example.com", SitePHP, SiteActive, 0)
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour)
	samples := []UsageSample{
		{SiteID: a.ID, SampledAt: now.Add(-40 * 24 * time.Hour), CPUCores: 9, MemBytes: 9, DiskBytes: -1, DiskInodes: -1},
		{SiteID: a.ID, SampledAt: now.Add(-25 * time.Hour), CPUCores: 2, MemBytes: 200, DiskBytes: 5000, DiskInodes: 50},
		{SiteID: a.ID, SampledAt: hour.Add(time.Second), CPUCores: 0.5, MemBytes: 100, MemMaxBytes: 1000, DiskBytes: -1, DiskInodes: -1},
		{SiteID: a.ID, SampledAt: hour.Add(2 * time.Second), CPUCores: 1.5, MemBytes: 300, MemMaxBytes: 1000, DiskBytes: 7000, DiskInodes: 70},
		{SiteID: 9999, SampledAt: now, DiskBytes: -1, DiskInodes: -1}, // no such site: skipped
	}
	if err := s.RecordUsage(samples); err != nil {
		t.Fatal(err)
	}
	day, err := s.SiteUsageSince(a.ID, now.Add(-UsageSampleKeep))
	if err != nil || len(day) != 2 || day[0].CPUCores != 0.5 || day[1].DiskBytes != 7000 {
		t.Fatalf("the day's samples: %+v, %v", day, err)
	}
	hours, err := s.SiteUsageHours(a.ID, now.Add(-UsageHourKeep))
	if err != nil || len(hours) != 2 {
		t.Fatalf("hours: %+v, %v", hours, err)
	}
	h := hours[1]
	if h.CPUCores != 1 || h.CPUCoresMax != 1.5 || h.MemBytes != 200 || h.MemBytesMax != 300 || h.DiskBytes != 7000 {
		t.Errorf("the current hour: %+v", h)
	}
	latest, _ := s.LatestUsage()
	if latest[a.ID].MemBytes != 300 {
		t.Errorf("latest: %+v", latest)
	}
	if err := s.DeleteSite(a.ID); err != nil {
		t.Fatal(err)
	}
	if day, _ := s.SiteUsageSince(a.ID, time.Time{}); len(day) != 0 {
		t.Errorf("a deleted site's samples stayed: %+v", day)
	}
	if hours, _ := s.SiteUsageHours(a.ID, time.Time{}); len(hours) != 0 {
		t.Errorf("a deleted site's hours stayed: %+v", hours)
	}
}
