package api

import (
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The renewal pass runs certbot for Let's Encrypt certificates near expiry,
// never for one the operator brought: that would obtain a certificate the
// vhost does not serve (the brought one takes precedence), every 12 hours.
func TestRenewalSkipsCustomCertificates(t *testing.T) {
	s, agent, _ := testServer(t)
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.ACMEEmail = "ops@example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, d := range []string{"le.example.com", "custom.example.com"} {
		if _, err := s.Store.CreateSite(state.Site{Domain: d, Type: state.SiteStatic, Profile: state.ProfileBalanced,
			Engine: state.EngineNginx, SystemUser: "cg-site-" + string(rune('1'+i)), RootPath: "/srv/sites/" + d, Status: state.SiteActive}); err != nil {
			t.Fatal(err)
		}
	}
	soon := time.Now().Add(5 * 24 * time.Hour)
	agent.results[agentrpc.CertStatus.Name()] = rpc.CertStatusResult{Certs: map[string]rpc.CertInfo{
		"le.example.com":     {Exists: true, NotAfter: soon},
		"custom.example.com": {Exists: true, NotAfter: soon, Custom: true},
	}}
	runJobNow(t, s, jobs.Spec{Kind: "certificate.sweep", Locks: []string{jobs.PanelLock}})
	var renewed []string
	agent.mu.Lock()
	for _, c := range agent.history {
		if c.method == agentrpc.IssueCert.Name() {
			renewed = append(renewed, c.params.(rpc.CertificateParams).Site.Domain)
		}
	}
	agent.mu.Unlock()
	if len(renewed) != 1 || renewed[0] != "le.example.com" {
		t.Fatalf("renewed %v, want only le.example.com", renewed)
	}
}
