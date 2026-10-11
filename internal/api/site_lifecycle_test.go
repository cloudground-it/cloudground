package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A WordPress create asks the agent, ends active with the panel's own
// names for the system user and the database, and records the files the
// agent wrote; the same domain again conflicts and a malformed one is
// refused.
func TestCreateWordPressSite(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	req := wordpressSite("example.com")
	req["title"] = "Example"
	site := createViaAPI(t, s, admin, req)
	if !agent.called(agentrpc.CreateSite.Name()) {
		t.Fatal("the agent was not asked to create the site")
	}
	if site.Status != state.SiteActive {
		jobs, _ := s.Store.ListJobs(5, false)
		t.Fatalf("status %s after the create; jobs %+v", site.Status, jobs)
	}
	if want := fmt.Sprintf("cg-site-%d", site.ID); site.SystemUser != want {
		t.Errorf("system user %q, want %q", site.SystemUser, want)
	}
	if want := fmt.Sprintf("site_%d", site.ID); site.Config.Database.Name != want {
		t.Errorf("database %q, want %q", site.Config.Database.Name, want)
	}
	if files, _ := s.Store.ManagedFiles(); files["/etc/nginx/sites-enabled/example.com.conf"] != "abc" {
		t.Errorf("managed files %v: the vhost the agent wrote is not recorded", files)
	}
	for _, c := range []struct {
		domain string
		want   int
	}{{"example.com", http.StatusConflict}, {"bad domain!", http.StatusBadRequest}} {
		if resp, body := admin.do("POST", "/api/sites", staticSite(c.domain)); resp.StatusCode != c.want {
			t.Errorf("create %q = %d %s, want %d", c.domain, resp.StatusCode, body, c.want)
		}
	}
}

// A delete the agent cannot finish keeps the record, in status error.
func TestFailedDeleteKeepsTheRecord(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("keep.example.com"))
	agent.setErr(agentrpc.DeleteSite.Name(), errors.New("simulated cleanup failure"))
	if resp, body := admin.do("DELETE", fmt.Sprintf("/api/sites/%d", site.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	kept, err := s.Store.GetSite(site.ID)
	if err != nil {
		t.Fatalf("the failed delete removed the record: %v", err)
	}
	if kept.Status != state.SiteError {
		t.Fatalf("status %s, want error", kept.Status)
	}
}

// A create the agent fails leaves no record, so the same domain can be
// created again.
func TestFailedCreateCanBeRetried(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	agent.setErr(agentrpc.CreateSite.Name(), errors.New("simulated provisioning failure"))
	if resp, body := admin.do("POST", "/api/sites", staticSite("retry.example.com")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if _, err := s.Store.GetSiteByDomain("retry.example.com"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("the failed create left a record: %v", err)
	}
	agent.setErr(agentrpc.CreateSite.Name(), nil)
	createViaAPI(t, s, admin, staticSite("retry.example.com"))
}

// PHP settings the agent fails to apply (a php-fpm unit missing on this
// host) are put back as they were; a later change that applies is kept.
func TestPHPSettingsPutBackWhenTheApplyFails(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, map[string]any{"domain": "phpback.example.com", "type": "php", "php_version": "8.4"})
	patch := func(body map[string]any) {
		t.Helper()
		if resp, out := admin.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), body); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("PATCH php %v = %d %s", body, resp.StatusCode, out)
		}
		waitForTasks(t, s)
	}

	agent.setErr(agentrpc.ApplySiteConfig.Name(), errors.New("systemctl reload php8.3-fpm: unit not found"))
	patch(map[string]any{"php_version": "8.3", "memory_limit_mb": 512})
	after, err := s.Store.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PHPVersion != "8.4" || after.Config.PHP.MemoryLimitMB != 0 {
		t.Fatalf("after a failed apply: php %s, memory_limit_mb %d; want 8.4 and 0 put back", after.PHPVersion, after.Config.PHP.MemoryLimitMB)
	}

	agent.setErr(agentrpc.ApplySiteConfig.Name(), nil)
	patch(map[string]any{"php_version": "8.3"})
	if after, _ = s.Store.GetSite(site.ID); after.PHPVersion != "8.3" {
		t.Fatalf("php %s after an apply that worked, want 8.3", after.PHPVersion)
	}
}

// A static site refuses object_cache (a WordPress setting), and a settings
// change the agent fails to apply is put back.
func TestSiteConfigPutBackWhenTheApplyFails(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("configback.example.com"))
	path := fmt.Sprintf("/api/sites/%d", site.ID)
	if resp, body := admin.do("PATCH", path, map[string]any{"object_cache": true}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH object_cache on a static site = %d %s, want 400", resp.StatusCode, body)
	}
	was := site.Config.CacheEnabled
	agent.setErr(agentrpc.ApplySiteConfig.Name(), errors.New("simulated render failure"))
	if resp, body := admin.do("PATCH", path, map[string]any{"cache_enabled": !was}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH cache_enabled = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if after, _ := s.Store.GetSite(site.ID); after.Config.CacheEnabled != was {
		t.Fatalf("cache_enabled %v after a failed apply, want %v put back", after.Config.CacheEnabled, was)
	}
}

// A database import over the live site must name the site's domain.
func TestDatabaseImportNeedsTheDomain(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, wordpressSite("dbimport.example.com"))
	resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/database/import", site.ID),
		map[string]string{"path": "shared/import.sql", "confirm": "other.example.com"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("import confirmed with another domain = %d %s, want 400", resp.StatusCode, body)
	}
}

// The database page shows the agent's typed table list as rows of name,
// row count and size in MiB. The console's mode follows allow_writes: the
// agent's read-only account, not a prefix check here, decides what reads.
func TestDatabasePageAndConsoleMode(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, wordpressSite("dbpage.example.com"))
	db := site.Config.Database.Name
	agent.setResult(agentrpc.DBTables.Name(), rpc.DBTablesResult{Database: db,
		Tables: []rpc.DBTable{{Name: "wp_options", Rows: 12, DataBytes: 3 << 20}}})
	resp, body := admin.do("GET", fmt.Sprintf("/api/sites/%d/database", site.ID), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"rows":[["wp_options","12","3.00"]]`) {
		t.Fatalf("database page = %d %s", resp.StatusCode, body)
	}
	// A statement that starts like a read and writes.
	const sneaky = "with t as (select 1) delete from x"
	for _, writes := range []bool{false, true} {
		resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/database/query", site.ID), map[string]any{"sql": sneaky, "allow_writes": writes})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query (allow_writes %v) = %d %s", writes, resp.StatusCode, body)
		}
		sent, _ := agent.last(agentrpc.DBQuery.Name()).(rpc.DBQueryParams)
		if sent.ReadOnly == writes || sent.Database != db {
			t.Fatalf("allow_writes %v sent %+v", writes, sent)
		}
	}
}
