package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A certificate call for a site that was deleted meanwhile fails instead of
// rendering its vhost back, and never runs certbot for it.
func TestCertificateAfterDeleteDoesNotResurrect(t *testing.T) {
	a, _ := testAgent(t)
	acme := a.ACME.(*host.FakeACME)
	site := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: site}); err != nil {
		t.Fatal(err)
	}
	cert, key := selfSigned(t, site.Domain)
	if _, err := rpcCall(a, agentrpc.CustomCert, rpc.CustomCertParams{Site: site, Fullchain: cert, Key: key}); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("a custom certificate was installed for a deleted site: %v", err)
	}
	if _, err := rpcCall(a, agentrpc.IssueCert, rpc.CertificateParams{Site: site, Email: "ops@example.com", Wildcard: true,
		CloudflareToken: strings.Repeat("a", 40)}); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("a certificate was issued for a deleted site: %v", err)
	}
	if len(acme.Requests()) != 0 {
		t.Fatal("certbot ran for a deleted site")
	}
	if _, err := os.Stat(filepath.Join(a.Paths.NginxSites, site.Domain+".conf")); !os.IsNotExist(err) {
		t.Fatal("the deleted site's vhost came back")
	}
}

// A deletion that arrives while certbot runs (minutes, in real life) waits
// for the issuance, and the site's vhost is gone once both are done.
func TestIssueRacingDeleteDoesNotResurrect(t *testing.T) {
	a, _ := testAgent(t)
	site := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	var once sync.Once
	a.ACME.(*host.FakeACME).OnCertOnly = func(context.Context, host.CertRequest) error {
		once.Do(func() {
			go func() {
				_, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: site})
				deleted <- err
			}()
			// Give the deletion every chance to finish while certbot runs.
			select {
			case err := <-deleted:
				deleted <- err
			case <-time.After(300 * time.Millisecond):
			}
		})
		return nil
	}
	if _, err := rpcCall(a, agentrpc.IssueCert, rpc.CertificateParams{Site: site, Email: "ops@example.com", Wildcard: true,
		CloudflareToken: strings.Repeat("a", 40)}); err != nil {
		t.Fatalf("issuance for a live site failed: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Paths.NginxSites, site.Domain+".conf")); !os.IsNotExist(err) {
		t.Fatal("an issuance racing a deletion put the vhost back")
	}
}

// A custom certificate waits for the site lock, and a site deleted while it
// waited is refused rather than rendered back.
func TestCustomCertificateWaitsForTheSiteLock(t *testing.T) {
	a, _ := testAgent(t)
	site := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	cert, key := selfSigned(t, site.Domain)
	unlock := a.lockSite(site.Domain)
	done := make(chan error, 1)
	go func() {
		_, err := rpcCall(a, agentrpc.CustomCert, rpc.CustomCertParams{Site: site, Fullchain: cert, Key: key})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("CustomCertificate ran without the site lock (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	// What DeleteSite does under the lock.
	vhost := filepath.Join(a.Paths.NginxSites, site.Domain+".conf")
	os.Remove(vhost)
	os.RemoveAll(site.RootPath)
	unlock()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("a site deleted while the certificate waited was not refused: %v", err)
	}
	if _, err := os.Stat(vhost); !os.IsNotExist(err) {
		t.Fatal("the deleted site's vhost came back")
	}
}

// Every other handler that writes a site's tree or configuration refuses a
// site that is gone, instead of recreating part of it.
func TestSiteHandlersRefuseADeletedSite(t *testing.T) {
	a, _ := testAgent(t)
	site := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: site}); err != nil {
		t.Fatal(err)
	}
	stg := site
	stg.ID, stg.Domain, stg.SystemUser, stg.StagingOf = 4, "staging.docs.example.com", "cg-site-4", site.ID
	stg.RootPath = filepath.Join(a.Paths.SitesRoot, stg.Domain)
	dbSite, dbStg := site, stg
	dbSite.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_3", User: "site_3"}
	dbStg.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_4", User: "site_4"}
	rel := "20260101-000000"
	for name, call := range map[string]func() error{
		"DeployRelease": func() error {
			_, err := rpcCall(a, agentrpc.DeployRelease, rpc.DeployParams{Site: site, SourceDir: filepath.Join(site.RootPath, "current"), ReleaseID: rel})
			return err
		},
		"PromoteRelease": func() error {
			_, err := rpcCall(a, agentrpc.PromoteRelease, rpc.ReleaseParams{Site: site, ReleaseID: rel})
			return err
		},
		"RollbackRelease": func() error {
			_, err := rpcCall(a, agentrpc.RollbackRelease, rpc.ReleaseParams{Site: site})
			return err
		},
		"WPObjectCache": func() error {
			_, err := rpcCall(a, agentrpc.WPObjectCache, rpc.WPParams{Site: site, Enable: true})
			return err
		},
		// An update rewrites the live release's files (R-3.8).
		"WPUpdate": func() error {
			_, err := rpcCall(a, agentrpc.WPUpdate, rpc.WPParams{Site: site, What: "plugins"})
			return err
		},
		"ImportMigration": func() error {
			_, err := rpcCall(a, agentrpc.ImportMigration, rpc.MigrationParams{Site: site, ReleaseID: rel, Archive: "shared/site.tar.gz"})
			return err
		},
		"CreateStaging": func() error {
			_, err := rpcCall(a, agentrpc.CreateStaging, rpc.StagingParams{Production: site, Staging: stg})
			return err
		},
		"SyncStagingDatabase": func() error {
			_, err := rpcCall(a, agentrpc.SyncStagingDB, rpc.SyncStagingDBParams{Production: dbSite, Staging: dbStg, Tables: []string{"wp_options"}})
			return err
		},
		"RestoreSnapshot": func() error {
			_, err := rpcCall(a, agentrpc.RestoreSnapshot, rpc.RestoreParams{Site: site, SnapshotID: "abcdef1234567890"})
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "no longer exists") {
			t.Errorf("%s on a deleted site: %v", name, err)
		}
	}
	for _, p := range []string{site.RootPath, stg.RootPath, filepath.Join(a.Paths.NginxSites, site.Domain+".conf"), filepath.Join(a.Paths.NginxSites, stg.Domain+".conf")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after calls on a deleted site", p)
		}
	}
}
