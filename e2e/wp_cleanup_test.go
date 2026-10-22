//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// wpCleanup (#78): on a WordPress site with a post that has many revisions
// and expired transients in the database, a dry run counts them and deletes
// nothing; a cleanup (the schedule on, then "run") keeps the newest N
// revisions of the post and deletes the expired transients, not the post nor
// its live content; the first cleanup is behind a restore-tested
// pre-cleanup snapshot and the second takes no other; the result is in the
// site's cleanup.log and in the log viewer's "cleanup" source.
func (h *harness) wpCleanup(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-clean-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	_, sb := h.api.get("/api/settings")
	prev := jsonField(sb, "backup_repository")
	defer h.undo("restore the backup repository setting (#78)", func() {
		h.api.relogin()
		if prev == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prev)
		}
		_ = os.RemoveAll(brepo)
	})()
	body, _ := json.Marshal(map[string]string{"backup_repository": brepo, "backup_password": bpw})
	if code(h.api.put("/api/settings", body)) != "200" {
		h.fail(t, "#78 control: the backup repository is accepted", "PUT /api/settings refused %s", brepo)
		return
	}
	path := fmt.Sprintf("/api/sites/%d/wp/cleanup", w.id)
	defer h.undo("turn the cleanup schedule off (#78)", func() { h.api.put(path, map[string]any{"enabled": false, "keep_revisions": 5}) })()

	// A post with many revisions, and expired transients in the database
	// (options added directly: with an object cache on, wp transient set
	// would not write them there).
	postID := h.wp(w.user, w.path, "post", "create", "--post_title=cg78", "--post_status=publish", "--post_content=v0", "--porcelain").Trim()
	for i := 1; i <= 8; i++ {
		h.wp(w.user, w.path, "post", "update", postID, fmt.Sprintf("--post_content=v%d", i))
	}
	for i := 1; i <= 3; i++ {
		h.wp(w.user, w.path, "option", "add", fmt.Sprintf("_transient_timeout_cg78x%d", i), "1", "--autoload=no")
		h.wp(w.user, w.path, "option", "add", fmt.Sprintf("_transient_cg78x%d", i), "x", "--autoload=no")
	}
	sql := func(q string) string {
		return h.wp(w.user, w.path, "db", "query", q, "--skip-column-names").Trim()
	}
	prefix := h.wp(w.user, w.path, "db", "prefix").Trim()
	count := func() (int, string) {
		var n int
		fmt.Sscan(sql(fmt.Sprintf("SELECT COUNT(*) FROM %sposts WHERE post_type='revision' AND post_parent=%s", prefix, postID)), &n)
		return n, sql(fmt.Sprintf("SELECT COUNT(*) FROM %soptions WHERE option_name LIKE '\\_transient\\_cg78x%%'", prefix))
	}
	before, transBefore := count()
	h.assert(t, "#78 control: the post has many revisions", before >= 7, "revisions of post %s: %d", postID, before)
	h.check(t, "#78 control: the expired transients are in the database", transBefore, "3")

	var v struct {
		Enabled      bool    `json:"enabled"`
		Keep         int     `json:"keep_revisions"`
		LastRun      *string `json:"last_run"`
		NextRun      *string `json:"next_run"`
		BackupsReady bool    `json:"backups_ready"`
	}
	c, b := h.api.get(path)
	_ = json.Unmarshal(b, &v)
	h.assert(t, "#78: the schedule is off by default, with the repository ready", c == 200 && !v.Enabled && v.BackupsReady && v.Keep == 5, "%d %s", c, head(b, 300))

	c, _ = h.api.put(path, map[string]any{"keep_revisions": 0})
	h.check(t, "#78: keeping no revision is refused", fmt.Sprint(c), "400")
	c, _ = h.api.put(path, map[string]any{"keep_revisions": 3})
	h.check(t, "#78: keeping three revisions is accepted", fmt.Sprint(c), "200")

	c, b = h.api.post(path+"/preview", `{}`)
	var pre struct {
		Expired   int `json:"expired_transients"`
		Excess    int `json:"excess_revisions"`
		AutoCount int `json:"autoload_count"`
	}
	_ = json.Unmarshal(b, &pre)
	h.assert(t, "#78: the dry run counts the surplus revisions and the expired transients",
		c == 200 && pre.Excess >= before-3 && pre.Expired >= 3 && pre.AutoCount > 0, "%d %s", c, head(b, 400))
	now, transNow := count()
	h.assert(t, "#78: the dry run deleted nothing", now == before && transNow == transBefore, "revisions %d -> %d, transients %s -> %s", before, now, transBefore, transNow)

	c, b = h.api.put(path, map[string]any{"enabled": true})
	_ = json.Unmarshal(b, &v)
	h.assert(t, "#78: the schedule is on, with a next run", c == 200 && v.Enabled && v.NextRun != nil, "%d %s", c, head(b, 300))

	c, b = h.api.post(path+"/run", `{}`)
	h.check(t, "#78: a cleanup now is accepted", fmt.Sprint(c), "202")
	h.check(t, "#78: the first cleanup, snapshot included, succeeds", h.api.waitTask(taskID(b), 10*time.Minute), "succeeded")
	after, transAfter := count()
	h.check(t, "#78: the post keeps its newest three revisions", fmt.Sprint(after), "3")
	h.check(t, "#78: the expired transients are gone", transAfter, "0")
	h.check(t, "#78: the post itself is untouched", sql(fmt.Sprintf("SELECT post_content FROM %sposts WHERE ID=%s", prefix, postID)), "v8")

	var rows []backupRow
	_, lb := h.api.get(fmt.Sprintf("/api/sites/%d/backups", w.id))
	_ = json.Unmarshal(lb, &rows)
	snap, found := newest(rows, func(r backupRow) bool { return r.Kind == "pre-cleanup" })
	h.assert(t, "#78: the first cleanup took a pre-cleanup snapshot", found, "backups: %s", head(lb, 600))
	h.check(t, "#78: and it passed its restore test", snap.VerifyStatus, "passed")

	logPath := "/var/log/cloudground/" + w.dom + "/cleanup.log"
	h.assert(t, "#78: the result is in the site's cleanup log", strings.Contains(readTrim(logPath), "cleanup:"), "%s: %q", logPath, readTrim(logPath))
	c, b = h.api.get(fmt.Sprintf("/api/sites/%d/logs?source=cleanup", w.id))
	h.assert(t, "#78: the log viewer reads it", c == 200 && strings.Contains(string(b), "cleanup:"), "%d %s", c, head(b, 300))

	// The second cleanup has no snapshot of its own.
	_, b = h.api.post(path+"/run", `{}`)
	h.check(t, "#78: a second cleanup succeeds", h.api.waitTask(taskID(b), 10*time.Minute), "succeeded")
	n := 0
	_, lb = h.api.get(fmt.Sprintf("/api/sites/%d/backups", w.id))
	rows = nil
	_ = json.Unmarshal(lb, &rows)
	for _, r := range rows {
		if r.Kind == "pre-cleanup" {
			n++
		}
	}
	h.check(t, "#78: and takes no second pre-cleanup snapshot", fmt.Sprint(n), "1")
	_, b = h.api.get(path)
	_ = json.Unmarshal(b, &v)
	h.assert(t, "#78: the last run is recorded", v.LastRun != nil, "%s", head(b, 300))
}
