package api

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// blockOn makes the fake agent hold every call to method until release is
// called, and returns a channel that receives once per call as it starts.
func blockOn(agent *fakeAgent, method string) (entered chan struct{}, release func()) {
	entered = make(chan struct{}, 8)
	gate := make(chan struct{})
	var once sync.Once
	agent.setHook(func(m string) {
		if m == method {
			entered <- struct{}{}
			<-gate
		}
	})
	return entered, func() { once.Do(func() { close(gate) }) }
}

func createWPSite(t *testing.T, s *Server, c *client, domain string) state.Site {
	t.Helper()
	resp, body := c.do("POST", "/api/sites", map[string]any{
		"domain": domain, "type": "wordpress", "title": "Race",
		"admin_email": "owner@" + domain, "admin_user": "owner", "admin_password": "wordpress-admin-pass",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create site = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain(domain)
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// R-0.5: the object-cache task wrote back the whole site as the request had
// read it, so an SFTP change saved while the agent was installing the
// drop-in was undone. Both changes must survive, whatever the interleaving.
func TestObjectCacheAndSFTPTogglesBothPersist(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "race.example.com")

	entered, release := blockOn(agent, agentrpc.WPObjectCache.Name())
	defer release()
	resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]bool{"object_cache": true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("object cache = %d %s", resp.StatusCode, body)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the object-cache task never reached the agent")
	}
	// The SFTP change runs while the object-cache task is inside the agent.
	sftpDone := make(chan int, 1)
	go func() {
		sc := &client{t: t, base: c.base, cookies: c.cookies}
		resp, _ := sc.do("POST", fmt.Sprintf("/api/sites/%d/sftp", site.ID), map[string]any{"enable": true, "password": "a-long-sftp-password"})
		sftpDone <- resp.StatusCode
	}()
	time.Sleep(100 * time.Millisecond) // let the SFTP request finish first if nothing stops it
	release()
	select {
	case code := <-sftpDone:
		if code != http.StatusOK {
			t.Fatalf("sftp = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the SFTP request never completed")
	}
	waitForTasks(t, s)

	got, err := s.Store.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Config.ObjectCache || !got.Config.SFTPEnabled {
		t.Fatalf("a concurrent change was lost: object_cache=%v sftp_enabled=%v, want both true", got.Config.ObjectCache, got.Config.SFTPEnabled)
	}
}

// R-0.5: a failed config apply put the whole site back as it was before the
// request, undoing changes other requests had saved meanwhile. Only the
// request's own fields go back.
func TestFailedConfigRevertKeepsOtherChanges(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "revert.example.com")
	cacheBefore := site.Config.CacheEnabled

	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = fmt.Errorf("simulated render failure")
	agent.mu.Unlock()
	entered, release := blockOn(agent, agentrpc.ApplySiteConfig.Name())
	defer release()
	resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"cache_enabled": !cacheBefore})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update config = %d %s", resp.StatusCode, body)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the config task never reached the agent")
	}
	sftpDone := make(chan int, 1)
	go func() {
		sc := &client{t: t, base: c.base, cookies: c.cookies}
		resp, _ := sc.do("POST", fmt.Sprintf("/api/sites/%d/sftp", site.ID), map[string]any{"enable": true, "password": "a-long-sftp-password"})
		sftpDone <- resp.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)
	release()
	if code := <-sftpDone; code != http.StatusOK {
		t.Fatalf("sftp = %d", code)
	}
	waitForTasks(t, s)

	got, err := s.Store.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.CacheEnabled != cacheBefore {
		t.Errorf("cache_enabled = %v after a failed apply, want reverted to %v", got.Config.CacheEnabled, cacheBefore)
	}
	if !got.Config.SFTPEnabled {
		t.Error("the revert of a failed config apply undid an SFTP change saved meanwhile")
	}
}

// A config apply renders the site as stored when the agent is called, not the
// copy its request read: otherwise the last render, which is what the server
// runs, misses a change another request saved in between.
func TestConfigApplyRendersCurrentSite(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "fresh.example.com")

	entered, release := blockOn(agent, agentrpc.WPObjectCache.Name())
	defer release()
	resp, body := c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]bool{"object_cache": true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("object cache = %d %s", resp.StatusCode, body)
	}
	<-entered
	resp, body = c.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"hsts": false, "cache_ttl_sec": 600})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update config = %d %s", resp.StatusCode, body)
	}
	release()
	waitForTasks(t, s)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	var last *rpc.SiteRef
	for _, call := range agent.history {
		if call.method == agentrpc.ApplySiteConfig.Name() {
			ref := call.params.(rpc.SiteRef)
			last = &ref
		}
	}
	if last == nil {
		t.Fatal("the config change was never applied")
	}
	if !last.Site.Config.ObjectCache || last.Site.Config.CacheTTLSec != 600 {
		t.Fatalf("the last render had object_cache=%v cache_ttl_sec=%d, want true and 600", last.Site.Config.ObjectCache, last.Site.Config.CacheTTLSec)
	}
}
