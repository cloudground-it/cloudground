//go:build e2e

package e2e

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// updatePlugin is a plugin the section installs at 1.0 and offers a 2.0
// of: the package is a local zip whose path the option
// cg_e2e_update_package holds (WP_Upgrader takes a local file as it is).
// Both the broken and the healthy 2.0 are made here; nothing is fetched.
const updatePlugin = "cg-e2e-update"

// updatePluginV1 offers the update through the transient wp-cli reads.
const updatePluginV1 = `<?php
/*
Plugin Name: CG e2e update
Version: 1.0
*/
add_filter('site_transient_update_plugins', function ($t) {
	$pkg = get_option('cg_e2e_update_package');
	if (!$pkg) {
		return $t;
	}
	if (!is_object($t)) {
		$t = new stdClass();
	}
	if (!isset($t->response) || !is_array($t->response)) {
		$t->response = array();
	}
	$t->response['cg-e2e-update/cg-e2e-update.php'] = (object) array(
		'id' => 'cg-e2e-update', 'slug' => 'cg-e2e-update', 'plugin' => 'cg-e2e-update/cg-e2e-update.php',
		'new_version' => '2.0', 'package' => $pkg, 'url' => '',
	);
	return $t;
});
`

// updatePluginV2Broken takes every page down (wp-cli still loads, so
// nothing but the web sees it).
const updatePluginV2Broken = `<?php
/*
Plugin Name: CG e2e update
Version: 2.0
*/
if (!defined('WP_CLI')) {
	throw new Error('cg-e2e: this update breaks the site');
}
`

const updatePluginV2Healthy = `<?php
/*
Plugin Name: CG e2e update
Version: 2.0
*/
`

// pluginZip is a plugin package: <slug>/<slug>.php.
func pluginZip(php string) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(updatePlugin + "/" + updatePlugin + ".php")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write([]byte(php)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// wpUpdates (#20): an update runs behind a tested snapshot; one that breaks
// the site is rolled back from it, and a healthy one is kept. The update
// schedule is stored.
func (h *harness) wpUpdates(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		_, sb := h.api.get("/api/settings")
		prevRepo := jsonField(sb, "backup_repository")
		if prevRepo == "" {
			c, b := h.api.post(f.api("/wp/update"), `{"what":"plugins"}`)
			h.check(t, "#20: an update with no backup repository is refused", fmt.Sprintf("%d %v", c, strings.Contains(string(b), "backup_repository")), "412 true")
		}
		brepo := fmt.Sprintf("/var/backups/cloudground/e2e-upd-%d", h.pid)
		raw := make([]byte, 18)
		_, _ = rand.Read(raw)
		pw := fmt.Sprintf("%x", raw)
		restore := h.undo("restore the backup repository setting", func() {
			h.api.relogin()
			if prevRepo == "" {
				h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
			} else {
				h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prevRepo)
			}
			_ = os.RemoveAll(brepo)
		})
		defer restore()
		setBody, _ := json.Marshal(map[string]string{"backup_repository": brepo, "backup_password": pw})
		h.check(t, "#20 control: backup repository accepted", code(h.api.put("/api/settings", setBody)), "200")

		// The plugin at 1.0 and the two packages of its 2.0.
		dir := f.fp + "/wp-content/plugins/" + updatePlugin
		uploads := f.fp + "/wp-content/uploads"
		h.as(f.fsu, "mkdir", "-p", dir, uploads)
		h.mustWriteAs(t, dir+"/"+updatePlugin+".php", updatePluginV1, f.fsu)
		broken, healthy := uploads+"/cg-e2e-update-broken.zip", uploads+"/cg-e2e-update-healthy.zip"
		for path, php := range map[string]string{broken: updatePluginV2Broken, healthy: updatePluginV2Healthy} {
			z, err := pluginZip(php)
			if err != nil {
				t.Logf("fixture: zip: %v", err)
			}
			h.mustWriteAs(t, path, string(z), f.fsu)
		}
		cleanup := h.undo("remove "+updatePlugin, func() {
			h.wp(f.fsu, f.fp, "plugin", "delete", updatePlugin)
			h.wp(f.fsu, f.fp, "option", "delete", "cg_e2e_update_package")
			h.remove(dir, broken, healthy)
		})
		defer cleanup()
		h.wp(f.fsu, f.fp, "plugin", "activate", updatePlugin)
		h.wp(f.fsu, f.fp, "option", "update", "cg_e2e_update_package", broken)
		version := func() string {
			return h.wp(f.fsu, f.fp, "plugin", "get", updatePlugin, "--field=version").Trim()
		}
		h.check(t, "#20 control: the e2e plugin is at 1.0 and offers 2.0",
			version()+" "+h.wp(f.fsu, f.fp, "plugin", "list", "--name="+updatePlugin, "--field=update").Trim(), "1.0 available")
		h.check(t, "#20 control: the site answers before the update", h.get(f.fs, "/").CodeStr(), "200")

		update := func() jobTask {
			c, b := h.api.post(f.api("/wp/update"), `{"what":"plugins"}`)
			if c != 202 {
				return jobTask{Status: fmt.Sprintf("HTTP %d: %s", c, head(b, 200))}
			}
			id := taskID(b)
			h.api.waitTask(id, 15*time.Minute)
			return h.api.task(id)
		}

		// A broken update: measured broken, restored from its snapshot.
		before := len(f.backupsList())
		tk := update()
		h.assert(t, "#20: an update that breaks the site fails, rolled back", tk.Status == "failed" && strings.Contains(tk.Error, "rolled back to snapshot"),
			"#20: task %s: %s", tk.Status, tk.Error)
		h.assert(t, "#20: the health check saw the broken pages (HTTP 500)", strings.Contains(tk.Error, "HTTP 500"), "#20: %s", tk.Error)
		h.check(t, "#20: after the rollback the plugin is at 1.0 again", version(), "1.0")
		h.check(t, "#20: after the rollback the site answers", h.get(f.fs, "/").CodeStr(), "200")
		rows := f.backupsList()
		pre, ok := newest(rows, func(r backupRow) bool { return r.Kind == "pre-update" })
		h.assert(t, "#20: the update was preceded by a restore-tested pre-update snapshot",
			ok && pre.VerifyStatus == "passed" && len(rows) > before && strings.Contains(tk.Error, pre.SnapshotID),
			"#20: newest pre-update %+v (found %v), %d backups before, %d after; task error %q", pre, ok, before, len(rows), tk.Error)

		// A healthy update is kept.
		h.wp(f.fsu, f.fp, "option", "update", "cg_e2e_update_package", healthy)
		tk = update()
		h.check(t, "#20: a healthy update succeeds", tk.Status+" "+tk.Error, "succeeded ")
		h.check(t, "#20: the healthy update is kept (2.0)", version(), "2.0")
		h.check(t, "#20: the site answers after the update", h.get(f.fs, "/").CodeStr(), "200")

		// The schedule.
		c, b := h.api.put(f.api("/wp/updates"), `{"enabled":true,"schedule":"weekly","hour":3,"what":"plugins"}`)
		h.check(t, "#20: the update schedule is stored", fmt.Sprintf("%d %s %s %s", c, jsonField(b, "schedule"), jsonField(b, "hour"), jsonField(b, "what")), "200 weekly 3 plugins")
		h.check(t, "#20: the update schedule is turned off again", code(h.api.put(f.api("/wp/updates"), `{"enabled":false}`)), "200")
	})
}
