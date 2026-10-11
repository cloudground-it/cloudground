package api

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// testCapabilities describe the fake agent's machine: the installer's
// layout, four PHP versions, and the services an install has.
var testCapabilities = rpc.Capabilities{
	PHPVersions: []string{"8.5", "8.4", "8.3", "8.2"},
	Services: []rpc.ServiceCapability{
		{Name: "nginx", Unit: "nginx", Listed: true, Restartable: true},
		{Name: "mariadb", Unit: "mariadb", Listed: true, Restartable: true},
		{Name: "redis", Unit: "redis-server", Listed: true, Restartable: true},
		{Name: "cloudground-api", Unit: "cloudground-api", Restartable: true},
	},
	Paths: rpc.HostPaths{SitesRoot: "/srv/sites", LogRoot: "/var/log/cloudground", MySQLSocket: "/run/mysqld/mysqld.sock"},
}

// testServer is a panel on a fresh store (with its secret key) and a fake
// agent, served over HTTP with insecure cookies. The agent answers a site
// creation with one vhost file, a purge with two removals, and the
// capabilities above; everything else succeeds with no result. When the
// test ends the queue drains first, then the HTTP server and the store
// close.
func testServer(t *testing.T) (*Server, *fakeAgent, *httptest.Server) {
	t.Helper()
	return testServerAt(t, filepath.Join(t.TempDir(), "state.db"))
}

// testServerAt is testServer on the database at path, for a test that
// opens the file itself as well.
func testServerAt(t *testing.T, path string) (*Server, *fakeAgent, *httptest.Server) {
	t.Helper()
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	created := rpc.CreateSiteResult{
		SystemUser: "cg-site-1",
		RootPath:   "/srv/sites/example.com",
		Files:      []rpc.ManagedFile{{Path: "/etc/nginx/sites-enabled/example.com.conf", SHA256: "abc"}},
	}
	agent := &fakeAgent{
		results: map[string]any{
			agentrpc.CreateSite.Name():   created,
			agentrpc.PurgeCache.Name():   rpc.PurgeResult{Removed: 2},
			agentrpc.Capabilities.Name(): testCapabilities,
		},
		errs: map[string]error{},
	}
	srv := &Server{Store: store, Agent: agent, Log: slog.Default(), InsecureCookies: true}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = NewHTTPServer(srv.Routes()) // the panel's own timeouts
	ts.Start()
	t.Cleanup(ts.Close)
	// Cleanups run last-registered first: the jobs end before the HTTP
	// server and the store they use go away.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.DrainJobs(ctx)
	})
	return srv, agent, ts
}

// setBackupRepo configures backups straight in the store: the repository
// in the panel settings, its password as a secret.
func setBackupRepo(t *testing.T, s *Server, repo, password string) {
	t.Helper()
	_, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.BackupRepository = repo
		return nil
	})
	if err == nil {
		err = s.Store.SetSecret(state.SecretBackupPassword, password)
	}
	if err != nil {
		t.Fatal(err)
	}
}
