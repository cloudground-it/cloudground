package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// After a render, the site's tracked files are what it wrote: a file it no
// longer writes (the shared pool file, once the site has its own PHP-FPM
// master) is dropped, files only deploys write (the connector) stay, and
// the files of a site that no longer exists are pruned, so none of them
// shows as drift forever. A slice drop-in belongs to its own site only.
func TestTrackedFilesFollowTheRenders(t *testing.T) {
	s, _, _ := testServer(t)
	site := state.Site{ID: 7, Domain: "a.example.com", SystemUser: "cg-site-7", RootPath: "/srv/sites/a.example.com"}
	const (
		sharedPool = "/etc/php/8.5/fpm/pool.d/cg-site-7.conf"
		unit       = "/etc/systemd/system/cloudground-php-cg-site-7.service"
		dropIn     = "/etc/systemd/system/cloudground-php-cg-site-7.service.d/cloudground-slice.conf"
	)
	connector := site.RootPath + "/current/wp-content/mu-plugins/cloudground-connector.php"
	orphans := []string{
		"/etc/systemd/system/cloudground-php-cg-site-99.service",
		"/etc/systemd/system/cloudground-php-cg-site-99.service.d/cloudground-slice.conf",
		"/etc/nginx/cloudground-auth/gone.example.com.htpasswd",
	}
	for _, p := range append([]string{sharedPool, connector}, orphans...) {
		if err := s.Store.RecordManagedFile(p, "x"); err != nil {
			t.Fatal(err)
		}
	}
	s.recordRendered(site, []rpc.ManagedFile{
		{Path: "/etc/nginx/sites-enabled/a.example.com.conf", SHA256: "v"},
		{Path: unit, SHA256: "u"},
		{Path: dropIn, SHA256: "d"},
	})
	s.pruneOrphanManagedFiles([]state.Site{site})

	tracked, err := s.Store.ManagedFiles()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{sharedPool: false, connector: true, unit: true, dropIn: true} {
		if _, ok := tracked[path]; ok != want {
			t.Errorf("%s tracked = %v, want %v", path, ok, want)
		}
	}
	for path := range tracked {
		if strings.Contains(path, "cg-site-99") || strings.Contains(path, "gone.example.com") {
			t.Errorf("a deleted site's file is still tracked: %s", path)
		}
	}
	if !siteOwnsPath(site, dropIn) {
		t.Error("the drop-in is not matched to its site")
	}
	if siteOwnsPath(state.Site{SystemUser: "cg-site-70", Domain: "b.example.com"}, dropIn) {
		t.Error("site 70 claims site 7's drop-in")
	}
}

// An open drift event closes by itself when its file matches what the panel
// wrote again, or is no longer tracked.
func TestDriftClosesWhenResolvedElsewhere(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	matchesAgain := "/etc/nginx/sites-enabled/a.example.com.conf"
	untracked := "/etc/systemd/system/cloudground-php-cg-site-99.service"
	s.Store.RecordManagedFile(matchesAgain, "v1")
	s.Store.CreateDriftEvent(state.DriftEvent{Path: matchesAgain, ExpectedHash: "v1", ActualHash: "edited"})
	s.Store.CreateDriftEvent(state.DriftEvent{Path: untracked, ExpectedHash: "u", ActualHash: ""})
	agent.setResult(agentrpc.CheckDrift.Name(), rpc.DriftResult{})
	resp, body := admin.do("GET", "/api/system/drift", nil)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("drift = %d %s, want none open", resp.StatusCode, body)
	}
	if open, _ := s.Store.ListDriftEvents(false); len(open) != 0 {
		t.Fatalf("%d events left open", len(open))
	}
}
