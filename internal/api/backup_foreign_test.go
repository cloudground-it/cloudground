package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const foreignSnap = "f00df00d11112222333344445555666677778888999900001111222233334444"

// foreignWP is a WordPress backup of shop.example.com taken on old-box,
// with its description: a primary and a second database.
func foreignWP() rpc.ForeignSnapshotResult {
	return rpc.ForeignSnapshotResult{
		ID: foreignSnap, Domain: "shop.example.com", Hostname: "old-box", Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Databases: []string{"site_12", "shop_extra"},
		Manifest: &rpc.SiteManifest{Format: 1, Domain: "shop.example.com", Aliases: []string{"www.shop.example.com"},
			Type: state.SiteWordPress, PHPVersion: "8.3", Cache: rpc.ManifestCache{Enabled: true, ObjectCache: true},
			Databases: []rpc.ManifestDatabase{{Name: "site_12", Engine: "mariadb", Primary: true}, {Name: "shop_extra", Engine: "mariadb"}}},
	}
}

func foreignSetup(t *testing.T) (*Server, *fakeAgent, *client) {
	t.Helper()
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	setBackupRepo(t, s, "/var/backups/cloudground/r", "pw")
	agent.mu.Lock()
	agent.results[agentrpc.ForeignSnapshot.Name()] = foreignWP()
	agent.results[agentrpc.RunBackup.Name()] = rpc.BackupResult{SnapshotID: "abcdef0123456789", Databases: []string{}}
	agent.results[agentrpc.VerifyBackup.Name()] = rpc.VerifyResult{Passed: true}
	agent.mu.Unlock()
	return s, agent, c
}

// §5.10: the refusals, before a site is made.
func TestForeignRestoreRefusals(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	post := func(body map[string]any) (int, string) {
		resp, b := c.do("POST", "/api/backups/foreign/restore", body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(map[string]any{"snapshot": foreignSnap, "confirm": "shop.example.com"}); code != http.StatusPreconditionFailed {
		t.Fatalf("backups off = %d %s", code, body)
	}
	setBackupRepo(t, s, "/var/backups/cloudground/r", "pw")
	agent.mu.Lock()
	noManifest := foreignWP()
	noManifest.Manifest = nil
	agent.results[agentrpc.ForeignSnapshot.Name()] = noManifest
	agent.mu.Unlock()
	seedActiveSite(t, s, "taken.example.com", state.SiteStatic, "", 0)
	for _, tc := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"snapshot": "nope", "confirm": "shop.example.com"}, "invalid snapshot id"},
		{map[string]any{"snapshot": foreignSnap, "confirm": "other.example.com"}, "type the site's domain"},
		{map[string]any{"snapshot": foreignSnap, "confirm": "shop.example.com"}, "give its type"},
		{map[string]any{"snapshot": foreignSnap, "confirm": "shop.example.com", "type": "proxy"}, "reverse-proxy"},
	} {
		code, body := post(tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("restore %v = %d %s, want 400 %q", tc.body, code, body, tc.want)
		}
	}
	agent.mu.Lock()
	taken := foreignWP()
	taken.Domain, taken.Manifest.Domain = "taken.example.com", "taken.example.com"
	agent.results[agentrpc.ForeignSnapshot.Name()] = taken
	agent.mu.Unlock()
	if code, body := post(map[string]any{"snapshot": foreignSnap, "confirm": "taken.example.com"}); code != http.StatusConflict {
		t.Fatalf("a domain this panel has = %d %s, want 409", code, body)
	}
	if agent.called(agentrpc.RestoreSnapshot.Name()) {
		t.Fatal("a refused restore reached the agent's restore")
	}
}

// §5.10: the list leaves out every domain this panel has.
func TestForeignBackupsList(t *testing.T) {
	s, agent, c := foreignSetup(t)
	seedActiveSite(t, s, "here.example.com", state.SiteStatic, "", 0)
	agent.mu.Lock()
	agent.results[agentrpc.RepositorySites.Name()] = rpc.RepositorySitesResult{Sites: []rpc.RepoSite{
		{Domain: "here.example.com", Hostname: "old-box", Snapshot: "aaaaaaaa11111111", Snapshots: 2},
		{Domain: "shop.example.com", Hostname: "old-box", Snapshot: foreignSnap, Snapshots: 5, Manifest: true},
	}}
	agent.mu.Unlock()
	resp, body := c.do("GET", "/api/backups/foreign", nil)
	var out struct {
		Sites []foreignSite `json:"sites"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || len(out.Sites) != 1 || out.Sites[0].Domain != "shop.example.com" || !out.Sites[0].Manifest {
		t.Fatalf("foreign = %d %s", resp.StatusCode, body)
	}
}

// §5.10: the site made from the description, its other database made, the
// backup restored with the foreign host and the primary mapped, then a
// backup of it here; audited.
func TestForeignRestoreCreatesAndRestores(t *testing.T) {
	s, agent, c := foreignSetup(t)
	resp, body := c.do("POST", "/api/backups/foreign/restore", map[string]any{"snapshot": foreignSnap, "confirm": "shop.example.com", "php_version": "8.4"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d %s", resp.StatusCode, body)
	}
	if j := waitTaskEnd(t, s, taskOf(t, body).ID); j.Status != state.JobSucceeded {
		t.Fatalf("backup.import = %s %q", j.Status, j.Error)
	}
	site, err := s.Store.GetSiteByDomain("shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if site.Type != state.SiteWordPress || site.PHPVersion != "8.4" || !slices.Equal(site.Aliases, []string{"www.shop.example.com"}) ||
		!site.Config.CacheEnabled || !site.Config.ObjectCache || site.Status != state.SiteActive {
		t.Fatalf("site = %+v", site)
	}
	p, _ := agent.lastParams[agentrpc.RestoreSnapshot.Name()].(rpc.RestoreParams)
	if p.Foreign == nil || p.Foreign.Hostname != "old-box" || p.Foreign.Databases["site_12"] != site.Config.Database.Name ||
		p.Foreign.DBPassword == "" || p.Foreign.ConnectorToken == "" || p.SnapshotID != foreignSnap || p.Mode != "full" {
		t.Fatalf("RestoreSnapshot = %+v (foreign %+v)", p, p.Foreign)
	}
	if db, _ := agent.lastParams[agentrpc.CreateDatabase.Name()].(rpc.SiteDatabaseParams); db.Name != "shop_extra" {
		t.Fatalf("the backup's other database was not made: %+v", db)
	}
	list, _ := s.Store.ListBackups(site.ID, 10)
	if len(list) != 1 || list[0].VerifyStatus != state.VerifyPassed {
		t.Fatalf("no backup of the restored site here: %+v", list)
	}
	events, _ := s.Store.ListAuditEvents(30)
	var asked, done bool
	for _, e := range events {
		asked = asked || e.Action == "backup.import" && strings.Contains(e.Detail, "old-box")
		done = done || e.Action == "backup.imported"
	}
	if !asked || !done {
		t.Fatalf("audit: %+v", events)
	}
}

// §5.10 step 3: a restore that fails keeps the site, as provisioned.
func TestForeignRestoreFailureKeepsTheSite(t *testing.T) {
	s, agent, c := foreignSetup(t)
	agent.mu.Lock()
	agent.errs[agentrpc.RestoreSnapshot.Name()] = agentrpc.Invalid("the snapshot does not contain the site's files")
	agent.mu.Unlock()
	_, body := c.do("POST", "/api/backups/foreign/restore", map[string]any{"snapshot": foreignSnap, "confirm": "shop.example.com", "php_version": "8.4"})
	j := waitTaskEnd(t, s, taskOf(t, body).ID)
	if j.Status != state.JobFailed || !strings.Contains(j.Error, "delete it to try again") {
		t.Fatalf("backup.import = %s %q", j.Status, j.Error)
	}
	if _, err := s.Store.GetSiteByDomain("shop.example.com"); err != nil {
		t.Fatalf("the site went: %v", err)
	}
}
