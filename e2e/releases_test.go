//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// releases (R-3.2): deploys started together get releases of their own, a
// rollback never lands on a release that was built but not promoted, a new
// release is the site user's and published whole, and its assets are served
// precompressed by gzip_static.
func (f *feat) releases(t *testing.T) {
	h := f.h
	root := "/srv/sites/" + f.fs
	before, _ := os.Readlink(root + "/current")

	// W3: provisioning's release is listed (it had no record: the list
	// never showed it, and after a rollback to it no row was live).
	_, lb := h.api.get(f.api("/deployments"))
	var listed []struct {
		ReleaseID string `json:"release_id"`
		Status    string `json:"status"`
	}
	_ = json.Unmarshal(lb, &listed)
	initial := ""
	for _, d := range listed {
		if d.ReleaseID == "initial" {
			initial = d.Status
		}
	}
	h.assert(t, "W3: the Releases list has provisioning's release", initial != "", "deployments: %s", head(lb, 400))
	if before == "releases/initial" {
		h.check(t, "W3: provisioning's release is listed live while it serves", initial, "promoted")
	}

	// Two deploys of the live release, started together. Release IDs were
	// the time to the second: the second deploy of a second collided with
	// the first and failed.
	type started struct {
		task int64
		id   string
	}
	var got [2]started
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, b := h.api.post(f.api("/deployments"), map[string]string{"source_dir": root + "/current"})
			got[i] = started{taskID(b), jsonField(b, "release_id")}
		}()
	}
	wg.Wait()
	for i, s := range got {
		h.check(t, fmt.Sprintf("R-3.2: deploy %d of two started together succeeds", i+1), h.api.waitTask(s.task, 300*time.Second), "succeeded")
	}
	h.assert(t, "R-3.2: two deploys started together get distinct releases", got[0].id != "" && got[0].id != got[1].id,
		"R-3.2: release ids %q and %q", got[0].id, got[1].id)
	for _, s := range got {
		h.assert(t, "R-3.2: each deploy built its own release", s.id != "" && exists(root+"/releases/"+s.id),
			"R-3.2: releases/%s is missing", s.id)
	}
	builds, _ := filepath.Glob(root + "/releases/.build-*")
	h.assert(t, "R-3.2: no build directory is left behind", len(builds) == 0, "R-3.2: left behind: %v", builds)
	if got[1].id == "" {
		return
	}
	newest := root + "/releases/" + got[1].id
	o, _ := statOwnerMode(newest)
	h.check(t, "R-3.2: a new release belongs to the site user", o, f.fsu)
	o, _ = statOwnerMode(newest + "/wp-content/mu-plugins/cloudground-connector.php")
	h.check(t, "R-3.2: the connector in a new release belongs to the site user", o, f.fsu)
	h.check(t, "R-3.2: a deploy does not change the live release", readlink(root+"/current"), before)

	// A rollback goes to the newest release older than the live one. It
	// used to take the newest release that was not live: one of the two
	// just built, never promoted.
	_, b := h.api.post(f.api("/rollback"), `{}`)
	if st := h.api.waitTask(taskID(b), 120*time.Second); st == "succeeded" {
		after := readlink(root + "/current")
		h.assert(t, "R-3.2: a rollback never lands on a release built after the live one",
			after != "releases/"+got[0].id && after != "releases/"+got[1].id && after != before,
			"R-3.2: rollback from %s went to %s", before, after)
	} else {
		t.Logf("rollback from %s: %s (no older release)", before, st)
	}

	// Promote the newest copy: the site serves what it served before.
	var deps []struct {
		ID        int64  `json:"id"`
		ReleaseID string `json:"release_id"`
	}
	_, b = h.api.get(f.api("/deployments"))
	json.Unmarshal(b, &deps)
	var depID int64
	for _, d := range deps {
		if d.ReleaseID == got[1].id {
			depID = d.ID
		}
	}
	_, b = h.api.post(fmt.Sprintf("/api/deployments/%d/promote", depID), `{}`)
	h.check(t, "R-3.2: the new release is promoted", h.api.waitTask(taskID(b), 120*time.Second), "succeeded")
	h.check(t, "R-3.2: current names the promoted release", readlink(root+"/current"), "releases/"+got[1].id)
	code, fatal := h.serves(f.fs, "/")
	h.check(t, "R-3.2: the promoted release serves", code+" "+fatal, "200 no")

	// gzip_static serves the .gz the deploy wrote, byte for byte, rather
	// than compressing per request.
	asset := "/wp-includes/css/dashicons.min.css"
	gz, err := os.ReadFile(newest + asset + ".gz")
	h.assert(t, "R-3.2: the deploy precompressed a stylesheet", err == nil, "R-3.2: %s.gz: %v", asset, err)
	r := h.get(f.fs, asset, "Accept-Encoding", "gzip")
	h.assert(t, "R-3.2: gzip_static serves the precompressed stylesheet",
		err == nil && strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") && bytes.Equal(r.Body, gz),
		"R-3.2: %s %s: Content-Encoding %q, %d bytes, .gz is %d bytes", r.CodeStr(), asset, r.Header.Get("Content-Encoding"), len(r.Body), len(gz))

	// Promotion keeps five releases older than the live one.
	ents, _ := os.ReadDir(root + "/releases")
	older := 0
	for _, e := range ents {
		if e.Name() < got[1].id && !strings.HasPrefix(e.Name(), ".") || e.Name() == "initial" {
			older++
		}
	}
	h.assert(t, "R-3.2: at most five releases older than the live one are kept", older <= 5, "R-3.2: %d older releases: %v", older, ents)
}

// releaseFromSharedFolder (W3): the Releases tab's folder hint names a
// place the site user can write over SFTP. It suggested deploy/build at the
// site's root, which is root's (the SFTP chroot) and refuses a mkdir;
// shared/ is the site user's, and a release builds from a folder there.
func (f *feat) releaseFromSharedFolder(t *testing.T) {
	h := f.h
	root := "/srv/sites/" + f.fs
	r := h.as(f.fsu, "mkdir", root+"/cg-e2e-deploy")
	h.assert(t, "W3 control: the site user cannot make a folder at the site's root", !r.OK(), "%s", r)
	h.remove(root + "/cg-e2e-deploy")
	dir := root + "/shared/cg-e2e-build"
	h.undo("remove the e2e build folder", func() { h.remove(dir) })
	r = h.as(f.fsu, "mkdir", "-p", dir)
	h.assert(t, "W3: the site user makes a folder under shared/, as the hint says", r.OK(), "%s", r)
	if !r.OK() {
		return
	}
	h.mustWriteAs(t, dir+"/index.html", fmt.Sprintf("build-%d\n", h.pid), f.fsu)
	_, b := h.api.post(f.api("/deployments"), map[string]string{"source_dir": dir})
	id := jsonField(b, "release_id")
	h.check(t, "W3: a release builds from that folder", h.api.waitTask(taskID(b), 300*time.Second), "succeeded")
	h.check(t, "W3: the release holds the folder's files", readTrim(root+"/releases/"+id+"/index.html"), fmt.Sprintf("build-%d", h.pid))
}

func readlink(p string) string {
	l, _ := os.Readlink(p)
	return l
}
