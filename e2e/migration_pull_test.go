//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// migrationPull (#82): a site pulled from another host over SFTP, the other
// host being a second site of this box (its chrooted SFTP account, with a
// password). The dry run measures it and names the host's key; the pull,
// with that key pinned, copies it and imports it into the first site as a
// new release (behind a passed pre-migration snapshot when backups are
// configured); the password is in no task, no audit line and nothing is
// left in the site's tmp/.
func (h *harness) migrationPull(t *testing.T) {
	src := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("pull-src")))
	dst := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("pull-dst")))
	if h.api.waitActive(src, 4*time.Minute) != "active" || h.api.waitActive(dst, 4*time.Minute) != "active" {
		h.fail(t, "control: the section's two sites provision", "sites %d %d", src, dst)
		return
	}
	srcSite, dstSite := h.api.site(src), h.api.site(dst)
	_, rawDst := h.api.get(fmt.Sprintf("/api/sites/%d", dst))
	dstRoot := jsonField(rawDst, "root_path")
	write := func(name, content string) string {
		return code(h.api.post(fmt.Sprintf("/api/sites/%d/files/write", src), map[string]string{"path": "current/" + name, "content": content}))
	}
	h.check(t, "control: the old site has an index", write("index.html", "pulled from the old host\n"), "200")
	h.check(t, "control: and a file in a folder", write("assets/app.css", "body{}\n"), "200")
	password := fmt.Sprintf("e2e-pull-%d-Secret!", h.pid)
	h.check(t, "control: the old site's SFTP takes a password",
		code(h.api.post(fmt.Sprintf("/api/sites/%d/sftp", src), map[string]any{"enable": true, "password": password})), "200")
	port, _ := strconv.Atoi(h.sshPort())
	body := map[string]any{"host": "127.0.0.1", "port": port, "user": srcSite.SystemUser, "password": password, "path": "/current"}

	st, eb := h.api.post(fmt.Sprintf("/api/sites/%d/migration/pull/estimate", dst), body)
	var est struct {
		Files   int    `json:"files"`
		Bytes   int64  `json:"bytes"`
		HostKey string `json:"host_key"`
		Fits    bool   `json:"fits"`
	}
	_ = json.Unmarshal(eb, &est)
	h.assert(t, "#82: the dry run measures the old site and names its host key", st == 200 && est.Files >= 2 && est.Bytes > 0 && strings.HasPrefix(est.HostKey, "SHA256:") && est.Fits,
		"HTTP %d %s", st, head(eb, 300))
	bad := map[string]any{}
	for k, v := range body {
		bad[k] = v
	}
	bad["password"] = "not-the-password"
	st, _ = h.api.post(fmt.Sprintf("/api/sites/%d/migration/pull/estimate", dst), bad)
	h.check(t, "#82: a wrong password is refused", fmt.Sprint(st), "400")
	if est.HostKey == "" {
		return
	}

	pull := map[string]any{"confirm": dstSite.Domain, "host_key": est.HostKey}
	for k, v := range body {
		pull[k] = v
	}
	st, pb := h.api.post(fmt.Sprintf("/api/sites/%d/migration/pull", dst), pull)
	task := pullTaskID(pb)
	h.check(t, "#82: the pull is accepted", fmt.Sprint(st), "202")
	h.check(t, "#82: the pull and its import succeed", h.api.waitTask(task, 10*time.Minute), "succeeded")
	got, _ := os.ReadFile(filepath.Join(dstRoot, "current", "index.html"))
	h.check(t, "#82: the site serves the pulled files from a new release", string(got), "pulled from the old host\n")
	css, _ := os.ReadFile(filepath.Join(dstRoot, "current", "assets", "app.css"))
	h.check(t, "#82: folders come along", string(css), "body{}\n")
	_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", task))
	_, audit := h.api.get("/api/audit")
	h.assert(t, "#82: the password is in neither the task nor the audit log", !contains(tb, password) && !contains(audit, password), "the password was found")
	h.assert(t, "#82: the pull is audited", contains(audit, "migration.pull"), "no migration.pull")
	h.assert(t, "#82: the task names the host key it connected to", contains(tb, est.HostKey), "task: %s", head(tb, 400))
	left, _ := filepath.Glob(filepath.Join(dstRoot, "tmp", "cg-pull-*"))
	h.assert(t, "#82: nothing pulled is left in the site's tmp/", len(left) == 0, "left: %v", left)
	_, sb := h.api.get("/api/settings")
	if jsonField(sb, "backup_repository") != "" {
		h.assert(t, "#82: a restore-tested pre-migration snapshot was taken first", h.pullPassedBackup(dst, "pre-migration") != 0, "no passed pre-migration backup")
	}
}

// pullTaskID reads {"task": {"id": …}}.
func pullTaskID(b []byte) int64 {
	var v struct {
		Task struct {
			ID int64 `json:"id"`
		} `json:"task"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Task.ID
}

// pullPassedBackup is the id of the site's newest passed backup of kind, 0
// for none.
func (h *harness) pullPassedBackup(site int64, kind string) int64 {
	_, b := h.api.get(fmt.Sprintf("/api/sites/%d/backups", site))
	var list []struct {
		ID           int64  `json:"id"`
		Kind         string `json:"kind"`
		VerifyStatus string `json:"verify_status"`
	}
	_ = json.Unmarshal(b, &list)
	for _, x := range list {
		if x.Kind == kind && x.VerifyStatus == "passed" {
			return x.ID
		}
	}
	return 0
}
