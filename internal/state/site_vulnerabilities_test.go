package state

import "testing"

// A complete check replaces the last; a failed one keeps its findings and
// notes its error; a deleted site takes its row; the panel's switch is on
// by default.
func TestVulnReports(t *testing.T) {
	s := testStore(t)
	a := seedSite(t, s, "a.example.com", SiteWordPress, SiteActive, 0)
	if _, ok, err := s.SiteVulnReport(a.ID); ok || err != nil {
		t.Fatalf("a report before any check: %v %v", ok, err)
	}
	f := VulnFinding{ID: "x1", Component: "plugin", Slug: "akismet", Version: "5.0", Title: "XSS", Severity: "high", FixedIn: "5.1"}
	if err := s.RecordVulnCheck(VulnReport{SiteID: a.ID, Core: "6.8.3", Components: 4, Findings: []VulnFinding{f}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVulnCheck(VulnReport{SiteID: 9999}); err != nil {
		t.Fatal(err) // no such site: skipped
	}
	r, ok, err := s.SiteVulnReport(a.ID)
	if err != nil || !ok || r.Core != "6.8.3" || r.Components != 4 || len(r.Findings) != 1 || r.Findings[0] != f || r.CheckedAt.IsZero() || r.Error != "" {
		t.Fatalf("report %+v %v %v", r, ok, err)
	}
	if err := s.RecordVulnCheckError(a.ID, "the database did not answer"); err != nil {
		t.Fatal(err)
	}
	r2, _, _ := s.SiteVulnReport(a.ID)
	if r2.Error != "the database did not answer" || len(r2.Findings) != 1 || !r2.CheckedAt.Equal(r.CheckedAt) || r2.AttemptedAt.Before(r.AttemptedAt) {
		t.Errorf("after a failure: %+v", r2)
	}
	if err := s.RecordVulnCheck(VulnReport{SiteID: a.ID, Core: "6.8.4", Components: 4}); err != nil {
		t.Fatal(err)
	}
	all, err := s.VulnReports()
	if err != nil || len(all) != 1 || len(all[a.ID].Findings) != 0 || all[a.ID].Error != "" || all[a.ID].Core != "6.8.4" {
		t.Errorf("all: %+v %v", all, err)
	}
	if err := s.DeleteSite(a.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.VulnReports(); len(all) != 0 {
		t.Errorf("a deleted site's report stayed: %+v", all)
	}
	p, err := s.PanelSettings()
	if err != nil || !p.VulnerabilityCheck {
		t.Errorf("the check is not on by default: %+v %v", p, err)
	}
}
