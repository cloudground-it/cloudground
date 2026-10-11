package state

import (
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// Each committed change publishes its event, with the site it is about;
// a write that changes nothing (the same certificate expiry again) does not.
func TestStorePublishesChanges(t *testing.T) {
	s := testStore(t)
	bus := events.New(0)
	s.SetEvents(bus)
	sub, _, _ := bus.Subscribe("", 256)
	defer sub.Close()
	expect := func(typ events.Type, siteID int64) {
		t.Helper()
		select {
		case ev := <-sub.C():
			if ev.Type != typ || ev.SiteID != siteID {
				t.Fatalf("got %s about %d, want %s about %d", ev.Type, ev.SiteID, typ, siteID)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s", typ)
		}
	}
	none := func(what string) {
		t.Helper()
		select {
		case ev := <-sub.C():
			t.Fatalf("%s published %s", what, ev.Type)
		default:
		}
	}

	site, err := s.CreateSite(Site{Domain: "ev.example.com", Type: SiteStatic, Status: SiteProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	expect(events.SiteChanged, site.ID)
	s.MutateSite(site.ID, func(x *Site) error { x.Status = SiteActive; return nil })
	expect(events.SiteChanged, site.ID)
	s.MarkApplied(site.ID, site.DesiredGeneration, nil)
	expect(events.SiteChanged, site.ID)

	d, _ := s.CreateDeployment(Deployment{SiteID: site.ID, ReleaseID: "r1", Status: DeployCreated})
	expect(events.DeploymentChanged, site.ID)
	if _, err := s.MoveDeployment(d.ID, nil, DeployPromoting, DeploymentUpdate{}); err != nil {
		t.Fatal(err)
	}
	expect(events.DeploymentChanged, site.ID)
	if _, err := s.MoveDeployment(9999, nil, DeployPromoted, DeploymentUpdate{}); err != ErrNotFound {
		t.Fatalf("missing deployment: %v", err)
	}

	b, _ := s.CreateBackup(Backup{SiteID: site.ID, SnapshotID: "abcdef12", Repository: "/r", Kind: "full"})
	expect(events.BackupFinished, site.ID)
	s.SetBackupVerified(b.ID, VerifyPassed, 10)
	expect(events.BackupVerified, site.ID)
	if n, _ := s.DeleteBackupsBySnapshot(site.ID, "/r", []string{"abcdef12"}); n != 1 {
		t.Fatal("backup not removed")
	}
	expect(events.BackupRemoved, site.ID)

	j, _ := s.CreateCronJob(CronJob{SiteID: site.ID, Schedule: "* * * * *", Command: "true", Enabled: true})
	expect(events.CronChanged, site.ID)
	s.UpdateCronRun(j.ID, "ok")
	expect(events.CronChanged, site.ID)
	if err := s.DeleteCronJob(j.ID); err != nil {
		t.Fatal(err)
	}
	expect(events.CronChanged, site.ID)
	if err := s.DeleteCronJob(j.ID); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}

	exp := time.Now().Add(60 * 24 * time.Hour)
	s.SetCertExpiry(site.ID, exp)
	expect(events.CertChanged, site.ID)
	s.SetCertExpiry(site.ID, exp)
	none("the same certificate expiry")
	s.SetCertError(site.ID, "")
	none("clearing an error that was never set")
	s.SetCertError(site.ID, "boom")
	expect(events.CertChanged, site.ID)

	dr, _ := s.CreateDriftEvent(DriftEvent{Path: "/etc/x", ExpectedHash: "a", ActualHash: "b"})
	expect(events.DriftChanged, 0)
	s.ResolveDriftEvent(dr.ID, DriftAccepted)
	expect(events.DriftChanged, 0)

	s.RecordMetric(MetricSample{CPUHeadroomPct: 10})
	expect(events.MetricsSample, 0)
	s.UpdatePanelSettings(func(p *PanelSettings) error { p.ProbeTarget = "https://127.0.0.9"; return nil })
	expect(events.SettingsChanged, 0)

	u, _ := s.CreateUser("ev@example.com", "h", "operator")
	expect(events.UserChanged, 0)
	expect(events.SessionRevoked, 0)
	s.SetUserSites(u.ID, []int64{site.ID})
	expect(events.UserChanged, 0)
	expect(events.SessionRevoked, 0)
	s.CreateSession("tok", u.ID, time.Hour)
	none("a new session")
	s.DeleteSession("tok")
	expect(events.SessionRevoked, 0)

	if err := s.DeleteSite(site.ID); err != nil {
		t.Fatal(err)
	}
	expect(events.SiteDeleted, site.ID)
	if err := s.DeleteSite(site.ID); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
	none("deleting a site that is gone")
}
