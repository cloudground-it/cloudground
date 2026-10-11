//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// upgrade (R-6.5): a release-to-release update on the box, with releases
// built and signed by scripts/upgrade-bundle.sh (a throwaway key, an update
// origin on this machine's loopback) and served by this section. v0.1.0 is
// installed over the box's own build, a site created on it; `cgctl update`
// takes it to v0.2.0 and the panel's button to v0.2.1, each time with the
// site still served and still in the panel, the state snapshotted first; a
// downgrade is refused by both; v0.3.0, whose API exits at once, is rolled
// back to v0.2.1 with its state. At the end the box's own build is put back.
//
// It runs only with CG_E2E_UPGRADE_BUNDLE naming the unpacked bundle
// (docs/e2e.md), since it replaces the installed binaries.
func (h *harness) upgrade(t *testing.T) {
	bundle := os.Getenv("CG_E2E_UPGRADE_BUNDLE")
	if bundle == "" {
		t.Log("R-6.5: CG_E2E_UPGRADE_BUNDLE is not set: the upgrade section needs `make docker-upgrade-bundle` unpacked on the box (docs/e2e.md); skipped")
		return
	}
	origin, err := os.ReadFile(filepath.Join(bundle, "origin"))
	if err != nil {
		h.fail(t, "R-6.5: read the bundle", "%v", err)
		return
	}
	feed := &upgradeFeed{root: bundle}
	stopFeed, err := feed.serve(strings.TrimPrefix(strings.TrimSpace(string(origin)), "http://"))
	if err != nil {
		h.fail(t, "R-6.5: serve the local release feed", "%v", err)
		return
	}
	defer stopFeed()

	// The box's own build and floor come back at the end, whatever happens.
	saved, err := os.MkdirTemp("", "cg-e2e-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"panel-api", "panel-agent", "cgctl", "cg-site-shell"} {
		if data, err := os.ReadFile("/usr/local/bin/" + b); err == nil {
			_ = os.WriteFile(filepath.Join(saved, b), data, 0o755)
		}
	}
	const stateDir = "/var/lib/cloudground-agent"
	floor, floorErr := os.ReadFile(stateDir + "/highest-release")
	snapsBefore := map[string]bool{}
	for _, d := range listDir(stateDir + "/update-snapshots") {
		snapsBefore[d] = true
	}
	restore := h.undo("put the box's own build back", func() {
		r := h.run(filepath.Join(saved, "cgctl"), "install", "--from", saved, "--skip-packages")
		if !r.OK() {
			h.root.Logf("R-6.5 cleanup: reinstalling the box's build failed: %s", r)
		}
		if floorErr == nil {
			_ = os.WriteFile(stateDir+"/highest-release", floor, 0o600)
		} else {
			_ = os.Remove(stateDir + "/highest-release")
		}
		for _, d := range listDir(stateDir + "/update-snapshots") {
			if !snapsBefore[d] {
				_ = os.RemoveAll(filepath.Join(stateDir, "update-snapshots", d))
			}
		}
		os.RemoveAll(saved)
		h.api.relogin()
	})
	defer restore()
	_ = os.Remove(stateDir + "/highest-release") // the box's floor would refuse v0.1.0

	// v0.1.0 over the box's build, with the installer and the bundle's cgctl.
	feed.list("v0.1.0", "v0.2.0")
	r := h.run("env", "CLOUDGROUND_VERSION=v0.1.0", filepath.Join(bundle, "v0.1.0", "cgctl"), "install",
		"--release", filepath.Join(bundle, "v0.1.0"), "--skip-packages")
	h.assert(t, "R-6.5: v0.1.0 installs from its signed release", r.OK(), "%s", r)
	if !r.OK() {
		return
	}
	h.check(t, "R-6.5: v0.1.0 runs", h.runningVersion(), "v0.1.0")
	h.api.relogin()

	dom := h.dom("upgrade")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	if h.api.waitActive(id, 3*time.Minute) != "active" {
		h.fail(t, "R-6.5: a site on v0.1.0", "site %d did not become active", id)
		return
	}
	defer func() { h.api.relogin(); h.api.deleteSite(id); h.api.teardownGone(id) }()
	marker := fmt.Sprintf("cg-upgrade-%d", time.Now().UnixNano())
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.html", marker+"\n", h.api.site(id).SystemUser)
	siteOK := func(name string) {
		h.api.relogin()
		body := string(h.get(dom, "/").Body)
		h.assert(t, name+": the site still serves its own page", strings.Contains(body, marker), "body %q", clip(body))
		h.check(t, name+": the site is still in the panel, active", h.api.site(id).Status, "active")
	}
	siteOK("R-6.5 v0.1.0")

	// The shell: --check, then the update.
	r = h.run("/usr/local/bin/cgctl", "update", "--check")
	h.assert(t, "R-6.5: cgctl update --check offers v0.2.0", r.OK() && strings.Contains(r.Out, "update available: v0.1.0 -> v0.2.0"), "%s", r)
	h.check(t, "R-6.5: --check changes nothing", h.runningVersion(), "v0.1.0")
	r = h.run("/usr/local/bin/cgctl", "update")
	h.assert(t, "R-6.5: cgctl update takes v0.1.0 to v0.2.0", r.OK() && strings.Contains(r.Out, "updated: v0.1.0 -> v0.2.0"), "%s", r)
	h.check(t, "R-6.5: v0.2.0 runs", h.runningVersion(), "v0.2.0")
	h.check(t, "R-6.5: the floor is v0.2.0", readTrim(stateDir+"/highest-release"), "v0.2.0")
	snap := h.newSnapshot(stateDir, snapsBefore, "v0.1.0-to-v0.2.0")
	_, err = os.Stat(filepath.Join(snap, "state.db"))
	h.assert(t, "R-6.5: the state was snapshotted before v0.2.0 started", snap != "" && err == nil, "snapshot %q: %v", snap, err)
	siteOK("R-6.5 v0.2.0")

	r = h.run("/usr/local/bin/cgctl", "update", "--to", "v0.1.0")
	h.assert(t, "R-6.5: cgctl update refuses a downgrade", r.Code == 1 && strings.Contains(r.Err, "refusing to downgrade"), "%s", r)
	h.check(t, "R-6.5: a refused downgrade changes nothing", h.runningVersion(), "v0.2.0")

	// The installer of another release is not an update path: refused,
	// pointing to cgctl update; the installed release's installer repairs.
	r = h.run("env", "CLOUDGROUND_VERSION=v0.2.1", filepath.Join(bundle, "v0.2.1", "cgctl"), "install",
		"--release", filepath.Join(bundle, "v0.2.1"), "--skip-packages")
	h.assert(t, "R-6.5: the installer of another release refuses an installed panel", r.Code == 1 &&
		strings.Contains(r.Out+r.Err, "cgctl update --to v0.2.1"), "%s", r)
	h.check(t, "R-6.5: the refused install changes nothing", h.runningVersion(), "v0.2.0")
	r = h.run("env", "CLOUDGROUND_VERSION=v0.2.0", filepath.Join(bundle, "v0.2.0", "cgctl"), "install",
		"--release", filepath.Join(bundle, "v0.2.0"), "--skip-packages")
	h.assert(t, "R-6.5: the installed release's installer repairs", r.OK(), "%s", r)
	h.api.relogin()

	// The panel's button.
	feed.list("v0.1.0", "v0.2.0", "v0.2.1")
	since := time.Now()
	c, b := h.api.post("/api/panel/update", map[string]string{"version": "v0.2.1"})
	h.check(t, "R-6.5: the panel's update to v0.2.1 is accepted", fmt.Sprint(c), "200")
	res := h.waitUpdateResult(stateDir, since)
	h.assert(t, "R-6.5: the panel's update to v0.2.1 is healthy", res.OK && res.Version == "v0.2.1", "result %+v, answer %s", res, b)
	h.check(t, "R-6.5: v0.2.1 runs", h.runningVersion(), "v0.2.1")
	siteOK("R-6.5 v0.2.1")
	h.api.relogin()
	c, b = h.api.post("/api/panel/update", map[string]string{"version": "v0.2.0"})
	h.assert(t, "R-6.5: the panel refuses a downgrade", c == http.StatusConflict && strings.Contains(string(b), "downgrade"), "%d %s", c, b)

	// A release that does not come up is rolled back with its state.
	feed.list("v0.1.0", "v0.2.0", "v0.2.1", "v0.3.0")
	r = h.run("/usr/local/bin/cgctl", "update", "--timeout", "5m")
	h.assert(t, "R-6.5: an unhealthy v0.3.0 is rolled back", r.Code == 1 && strings.Contains(r.Err, "rolled back: v0.2.1 runs again"), "%s", r)
	res = h.waitUpdateResult(stateDir, time.Time{})
	h.assert(t, "R-6.5: the rollback put the state snapshot back", res.RolledBack && res.StateRestored && res.RestoredHealthy, "result %+v", res)
	h.check(t, "R-6.5: v0.2.1 runs again", h.runningVersion(), "v0.2.1")
	h.check(t, "R-6.5: a rolled-back release does not raise the floor", readTrim(stateDir+"/highest-release"), "v0.2.1")
	siteOK("R-6.5 after the rollback")
}

// upgradeFeed serves the bundle as a release origin: releases.atom listing
// the tags of list, and releases/download/<tag>/<file>.
type upgradeFeed struct {
	root string
	mu   sync.Mutex
	tags []string
}

func (f *upgradeFeed) list(tags ...string) {
	f.mu.Lock()
	f.tags = tags
	f.mu.Unlock()
}

func (f *upgradeFeed) serve(addr string) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases.atom", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom">`)
		for i := len(f.tags) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, `<entry><id>tag:e2e,2026:Repository/1/%[1]s</id><link rel="alternate" href="http://e2e/releases/tag/%[1]s"/><title>%[1]s</title></entry>`, f.tags[i])
		}
		b.WriteString(`</feed>`)
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/releases/download/")
		tag, file, ok := strings.Cut(rest, "/")
		if !ok || strings.ContainsAny(file, "/\\") || strings.Contains(tag, "..") || file == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(f.root, tag, file))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// runningVersion is the version the running agent reports.
func (h *harness) runningVersion() string {
	for i := 0; i < 30; i++ {
		if c, b, err := h.agentCall("Ping", "{}"); err == nil && c == 200 {
			var m struct {
				Result map[string]string `json:"result"`
			}
			if json.Unmarshal(b, &m) == nil && m.Result["version"] != "" {
				return m.Result["version"]
			}
			var flat map[string]string
			if json.Unmarshal(b, &flat) == nil && flat["version"] != "" {
				return flat["version"]
			}
		}
		time.Sleep(time.Second)
	}
	return "no answer"
}

type upgradeResult struct {
	Version         string `json:"version"`
	OK              bool   `json:"ok"`
	RolledBack      bool   `json:"rolled_back"`
	RestoredHealthy bool   `json:"restored_healthy"`
	StateRestored   bool   `json:"state_restored"`
	Started         string `json:"started"`
	Finished        string `json:"finished"`
	Error           string `json:"error"`
}

// waitUpdateResult waits (up to 5 minutes) for the guard's verdict on an
// update started after since.
func (h *harness) waitUpdateResult(stateDir string, since time.Time) upgradeResult {
	var r upgradeResult
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); time.Sleep(pollEvery) {
		b, err := os.ReadFile(stateDir + "/update-result.json")
		if err != nil || json.Unmarshal(b, &r) != nil || r.Finished == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, r.Started); err == nil && !at.Before(since.Add(-time.Second)) {
			return r
		}
	}
	return r
}

// newSnapshot is the snapshot directory made since before whose name ends
// with suffix ("" when none).
func (h *harness) newSnapshot(stateDir string, before map[string]bool, suffix string) string {
	for _, d := range listDir(stateDir + "/update-snapshots") {
		if !before[d] && strings.HasSuffix(d, suffix) {
			return filepath.Join(stateDir, "update-snapshots", d)
		}
	}
	return ""
}

func listDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
