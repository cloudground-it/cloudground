package state

import "testing"

// The metric history keeps the newest 288 samples, oldest first, however
// many are recorded or asked for.
func TestMetricHistoryKeepsTheNewest288(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 300; i++ {
		if err := s.RecordMetric(MetricSample{CPUHeadroomPct: i, MemHeadroomPct: 50, DiskHeadroomPct: 40, Load1: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListMetrics(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 288 {
		t.Fatalf("%d samples kept, want 288", len(got))
	}
	for i, m := range got {
		if want := 300 - 288 + i; m.CPUHeadroomPct != want {
			t.Fatalf("sample %d is #%d, want #%d (newest kept, oldest first)", i, m.CPUHeadroomPct, want)
		}
	}
}

// A site's releases, backups and the drift of its files go through their
// lifecycles; deleting the site takes its releases and backups with it.
func TestSiteRecordsLifecycleAndCascade(t *testing.T) {
	s := testStore(t)
	site := seedSite(t, s, "records.example.com", SitePHP, SiteActive, 0)

	d, err := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: "20260901-090000",
		Path: site.RootPath + "/releases/20260901-090000", Status: DeployCreated})
	if err != nil {
		t.Fatal(err)
	}
	// A built release goes live through promoting (the state machine, R-4.3).
	for _, step := range []struct {
		to    DeploymentStatus
		guard string
	}{{DeployPromoting, ""}, {DeployPromoted, `{"verdict":"pass"}`}} {
		if _, err := s.MoveDeployment(d.ID, nil, step.to, DeploymentUpdate{GuardJSON: step.guard}); err != nil {
			t.Fatalf("move to %s: %v", step.to, err)
		}
	}
	live, err := s.GetDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != DeployPromoted || live.PromotedAt == nil || live.GuardJSON != `{"verdict":"pass"}` {
		t.Fatalf("promoted release = %+v", live)
	}

	b, err := s.CreateBackup(Backup{SiteID: site.ID, SnapshotID: "0a1b2c3d", Repository: "/var/backups/repo", Kind: "full", SizeBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackupVerified(b.ID, VerifyPassed, 180000); err != nil {
		t.Fatal(err)
	}
	backups, err := s.ListBackups(site.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0].VerifyStatus != VerifyPassed || backups[0].RestoreEstMS != 180000 || backups[0].VerifiedAt == nil {
		t.Fatalf("backups = %+v", backups)
	}

	vhost := "/etc/nginx/sites-enabled/records.example.com.conf"
	if err := s.RecordManagedFile(vhost, "cafe"); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.ManagedFiles(); files[vhost] != "cafe" {
		t.Fatalf("managed files = %v", files)
	}
	ev, err := s.CreateDriftEvent(DriftEvent{Path: vhost, ExpectedHash: "cafe", ActualHash: "beef"})
	if err != nil {
		t.Fatal(err)
	}
	if open, _ := s.ListDriftEvents(false); len(open) != 1 || open[0].Status != DriftOpen {
		t.Fatalf("open drift = %+v", open)
	}
	if err := s.ResolveDriftEvent(ev.ID, DriftRestored); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.ListDriftEvents(false); len(open) != 0 {
		t.Fatalf("%d drift events still open after the resolve", len(open))
	}
	if all, _ := s.ListDriftEvents(true); len(all) != 1 || all[0].Status != DriftRestored || all[0].ResolvedAt == nil {
		t.Fatalf("resolved drift = %+v", all)
	}

	if err := s.DeleteSite(site.ID); err != nil {
		t.Fatal(err)
	}
	if deps, _ := s.ListDeployments(site.ID, 10); len(deps) != 0 {
		t.Errorf("%d releases outlived their site", len(deps))
	}
	if left, _ := s.ListBackups(site.ID, 10); len(left) != 0 {
		t.Errorf("%d backups outlived their site", len(left))
	}
}
