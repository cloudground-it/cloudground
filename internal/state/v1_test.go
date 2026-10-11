package state

import (
	"errors"
	"testing"
	"time"
)

func newSite(t *testing.T, s *Store, domain string) Site {
	t.Helper()
	site, err := s.CreateSite(Site{Domain: domain, Type: SiteStatic, Profile: ProfileBalanced, Engine: EngineNginx, SystemUser: "cg-x", RootPath: "/srv/sites/" + domain, Status: SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

func TestSecretRoundTripAndDeletion(t *testing.T) {
	s := testStore(t)
	if v, err := s.Secret(SecretCloudflareToken); err != nil || v != "" {
		t.Fatalf("absent secret = %q, %v; want empty, nil", v, err)
	}
	if err := s.SetSecret(SecretCloudflareToken, "tok•en"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Secret(SecretCloudflareToken); err != nil || v != "tok•en" {
		t.Fatalf("round trip = %q, %v", v, err)
	}
	var sealed []byte
	s.db.QueryRow(`SELECT sealed FROM secrets WHERE name=?`, SecretCloudflareToken).Scan(&sealed)
	if len(sealed) == 0 || string(sealed) == "tok•en" {
		t.Fatalf("stored value is not sealed: %q", sealed)
	}
	// A sealed value moved under another name does not open.
	if _, err := s.db.Exec(`INSERT INTO secrets (name, sealed, updated_at) VALUES (?, ?, '')`, SecretBackupPassword, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret(SecretBackupPassword); err == nil {
		t.Fatal("a sealed value opened under another name")
	}
	if err := s.SetSecret(SecretCloudflareToken, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Secret(SecretCloudflareToken); v != "" {
		t.Fatalf("SetSecret empty did not delete: %q", v)
	}
	if err := s.DeleteSecret(SecretBackupPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecretsWithPrefix(""); err == nil {
		t.Fatal("an empty prefix was accepted")
	}
}

func TestSiteSecretsFollowTheSite(t *testing.T) {
	s := testStore(t)
	if err := s.SetSecret(SiteSecret(999, SiteSecretDBPassword), "x"); err == nil {
		t.Fatal("a secret of a site that does not exist was stored")
	}
	if err := s.SetSecret("site.abc.db_password", "x"); err == nil {
		t.Fatal("a malformed site secret name was stored")
	}
	// Ten sites, so site 1's prefix is also the start of site 10's id.
	var sites []Site
	for i := 0; i < 10; i++ {
		sites = append(sites, newSite(t, s, string(rune('a'+i))+".example.com"))
	}
	one, ten := sites[0], sites[9]
	for _, site := range []Site{one, ten} {
		for _, k := range []string{SiteSecretDBPassword, SiteSecretConnectorToken} {
			if err := s.SetSecret(SiteSecret(site.ID, k), k+"-of-"+site.Domain); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetCertError(site.ID, "boom"); err != nil {
			t.Fatal(err)
		}
		if err := s.RaiseAlert(Alert{Key: "task:" + site.Domain, Kind: "task", SiteID: site.ID, Level: "error", Message: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSecretsWithPrefix(SiteSecretPrefix(one.ID)); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Secret(SiteSecret(one.ID, SiteSecretDBPassword)); v != "" {
		t.Fatalf("site 1's secret survived its prefix delete: %q", v)
	}
	if v, _ := s.Secret(SiteSecret(ten.ID, SiteSecretDBPassword)); v == "" {
		t.Fatal("deleting site 1's secrets took site 10's")
	}
	if err := s.DeleteSite(ten.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, q := range []string{
		`SELECT COUNT(*) FROM secrets WHERE name LIKE 'site.10.%'`,
		`SELECT COUNT(*) FROM site_status WHERE site_id=10`,
		`SELECT COUNT(*) FROM alerts WHERE site_id=10`,
	} {
		if err := s.db.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s = %d, %v after deleting the site", q, n, err)
		}
	}
	if a, err := s.GetAlert(AlertID("task:"+one.Domain, "")); err != nil || a.SiteID != one.ID {
		t.Errorf("another site's alert went with the deleted site: %+v, %v", a, err)
	}
	if err := s.DeleteSite(ten.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a deleted site = %v, want ErrNotFound", err)
	}
}

func TestPanelSettingsDefaultsAndUpdate(t *testing.T) {
	s := testStore(t)
	ps, err := s.PanelSettings()
	if err != nil {
		t.Fatal(err)
	}
	if ps != (PanelSettings{UpdateCheck: true, UpdateChannel: "stable", VulnerabilityCheck: true}) {
		t.Fatalf("defaults = %+v", ps)
	}
	if ps.EffectiveProbeTarget() != DefaultProbeTarget {
		t.Fatalf("effective probe target = %q", ps.EffectiveProbeTarget())
	}
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error {
		p.ACMEEmail, p.Require2FAAdmin = "a@example.com", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error {
		p.ACMEEmail = "lost@example.com"
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("fn error = %v, want it returned", err)
	}
	ps, _ = s.PanelSettings()
	if ps.ACMEEmail != "a@example.com" || !ps.Require2FAAdmin || !ps.UpdateCheck {
		t.Fatalf("after update = %+v", ps)
	}
}

func TestCertStateRoundTrip(t *testing.T) {
	s := testStore(t)
	site := newSite(t, s, "cert.example.com")
	exp := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.SetCertExpiry(site.ID, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCertError(site.ID, "rate limited"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCertExpiry(site.ID, exp); err != nil { // an upsert keeps the error
		t.Fatal(err)
	}
	c, err := s.CertStateOf(site.ID)
	if err != nil || !c.Expiry.Equal(exp) || c.Error != "rate limited" {
		t.Fatalf("cert state = %+v, %v", c, err)
	}
	if c, err := s.CertStateOf(site.ID + 1); err != nil || !c.Expiry.IsZero() || c.Error != "" {
		t.Fatalf("unknown site = %+v, %v", c, err)
	}
}

func TestGenerations(t *testing.T) {
	s := testStore(t)
	site := newSite(t, s, "gen.example.com")
	if site.DesiredGeneration != 1 || site.AppliedGeneration != 0 {
		t.Fatalf("new site at %d/%d, want 1/0", site.DesiredGeneration, site.AppliedGeneration)
	}
	if err := s.MarkApplied(site.ID, 1, nil); err != nil {
		t.Fatal(err)
	}

	// A status change is not a desired-configuration change.
	got, err := s.MutateSite(site.ID, func(st *Site) error { st.Status = SiteError; return nil })
	if err != nil || got.DesiredGeneration != 1 {
		t.Fatalf("status change moved desired to %d (%v)", got.DesiredGeneration, err)
	}
	// Neither is a no-op, nor a write to the generation fields themselves.
	got, _ = s.MutateSite(site.ID, func(st *Site) error {
		st.DesiredGeneration, st.AppliedGeneration, st.LastApplyError = 99, 99, "forged"
		return nil
	})
	if got.DesiredGeneration != 1 || got.AppliedGeneration != 1 || got.LastApplyError != "" {
		t.Fatalf("fn wrote the generation fields: %+v", got)
	}

	got, _ = s.MutateSite(site.ID, func(st *Site) error { st.Config.CacheTTLSec = 60; return nil })
	if got.DesiredGeneration != 2 || got.AppliedGeneration != 1 {
		t.Fatalf("config change: %d/%d, want 2/1", got.DesiredGeneration, got.AppliedGeneration)
	}

	// A failure records why and leaves applied where it was.
	if err := s.MarkApplied(site.ID, 2, errors.New("php-fpm8.9 not installed")); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSite(site.ID)
	if got.AppliedGeneration != 1 || got.LastApplyError != "php-fpm8.9 not installed" {
		t.Fatalf("after a failure: %+v", got)
	}
	// Success catches up and clears the error.
	s.MarkApplied(site.ID, 2, nil)
	got, _ = s.GetSite(site.ID)
	if got.AppliedGeneration != 2 || got.LastApplyError != "" {
		t.Fatalf("after success: %d %q", got.AppliedGeneration, got.LastApplyError)
	}
	// A late report of an older generation changes nothing.
	s.MarkApplied(site.ID, 1, errors.New("stale"))
	s.MarkApplied(site.ID, 1, nil)
	got, _ = s.GetSite(site.ID)
	if got.AppliedGeneration != 2 || got.LastApplyError != "" {
		t.Fatalf("a stale report rolled the record back: %d %q", got.AppliedGeneration, got.LastApplyError)
	}

	// A change the agent already made carries applied along when caught up...
	got, _ = s.MutateSiteApplied(site.ID, func(st *Site) error { st.Config.SFTPEnabled = true; return nil })
	if got.DesiredGeneration != 3 || got.AppliedGeneration != 3 {
		t.Fatalf("applied-directly change when caught up: %d/%d, want 3/3", got.DesiredGeneration, got.AppliedGeneration)
	}
	// ...and not when something else is still pending.
	s.MutateSite(site.ID, func(st *Site) error { st.Config.CacheTTLSec = 120; return nil })
	got, _ = s.MutateSiteApplied(site.ID, func(st *Site) error { st.Config.SFTPEnabled = false; return nil })
	if got.DesiredGeneration != 5 || got.AppliedGeneration != 3 {
		t.Fatalf("applied-directly change while behind: %d/%d, want 5/3", got.DesiredGeneration, got.AppliedGeneration)
	}
	if err := s.MarkApplied(site.ID+100, 1, nil); err != nil {
		t.Fatalf("MarkApplied on a deleted site = %v, want nil", err)
	}
}
