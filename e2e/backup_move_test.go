//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupMove (#192, internal/api SPEC §5.9, ops/backup SPEC §5, §13): the
// backups of a site with a database, in a local repository A of the
// section's own, are moved to B by the panel (restic copy, which gives every
// snapshot a new ID); A is deleted, and the old records restore from B in
// every mode (full, files, database, one path), pass their restore test and
// download. B copied by hand to C (the same repository) is then moved with
// "moved". Refused, the setting and the records left where they were:
// another repository, a copy missing a recorded snapshot, and a copy whose
// check fails after restic copied. An API token and an operator are refused
// the route.
func (h *harness) backupMove(t *testing.T) {
	realRestic, err := exec.LookPath("restic")
	if err != nil {
		h.fail(t, "#192 fixture: restic is on PATH", "%v", err)
		return
	}
	base := fmt.Sprintf("/var/backups/cloudground/e2e-move-%d", h.pid)
	repo := func(name string) string { return base + "-" + name }
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	pw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	_, sb := h.api.get("/api/settings")
	prevRepo := jsonField(sb, "backup_repository")
	defer h.undo("put the backup repository setting back (#192)", func() {
		h.api.relogin()
		if prevRepo == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prevRepo)
		}
		for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
			_ = os.RemoveAll(repo(n))
		}
	})()
	setBody, _ := json.Marshal(map[string]string{"backup_repository": repo("a"), "backup_password": pw})
	if c := code(h.api.put("/api/settings", setBody)); c != "200" {
		h.fail(t, "#192 control: repository A is accepted", "HTTP %s", c)
		return
	}
	resticA := h.resticOn(realRestic, repo("a"), pw)

	dom := h.dom("bmove")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static","create_database":true}`, dom))
	if h.api.waitActive(id, 4*time.Minute) != "active" {
		h.fail(t, "#192 control: the section's site provisions", "site %d", id)
		return
	}
	defer func() { h.api.relogin(); h.api.deleteSite(id); h.api.teardownGone(id) }()
	site := h.api.site(id)
	dbName := site.Config.Database.Name
	marker := func(v string) {
		h.check(t, "#192 control: the marker file is written ("+v+")",
			code(h.api.post(fmt.Sprintf("/api/sites/%d/files/write", id), map[string]string{"path": "current/cg-move.txt", "content": v + "\n"})), "200")
		r := h.run("mariadb", dbName, "-e", "CREATE TABLE IF NOT EXISTS cg_move (v VARCHAR(16)); DELETE FROM cg_move; INSERT INTO cg_move VALUES ('"+v+"');")
		h.assert(t, "#192 control: the marker row is written ("+v+")", r.OK(), "%s", r)
	}
	file := func() string {
		b, _ := os.ReadFile(filepath.Join("/srv/sites", dom, "current", "cg-move.txt"))
		return strings.TrimSpace(string(b))
	}
	row := func() string {
		return strings.TrimSpace(h.run("mariadb", "-N", dbName, "-e", "SELECT v FROM cg_move").Out)
	}
	backup := func(v string) (int64, string) {
		marker(v)
		c, b := h.api.post(fmt.Sprintf("/api/sites/%d/backups", id), map[string]any{})
		if st := h.api.waitTask(taskIDOf(b), 10*time.Minute); st != "succeeded" {
			_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", taskIDOf(b)))
			h.fail(t, "#192 control: a backup in A passes its restore test ("+v+")", "task %s: HTTP %d %s; %s", st, c, head(b, 300), head(tb, 400))
			return 0, ""
		}
		rec := h.siteBackups(id)[0]
		return rec.ID, rec.SnapshotID
	}
	r1, snap1 := backup("v1")
	r2, snap2 := backup("v2")
	if r1 == 0 || r2 == 0 {
		return
	}

	// The route is an administrator's session's (§1.2.1).
	c, tb := h.api.post("/api/tokens", map[string]any{"name": fmt.Sprintf("e2e-move-%d", h.pid), "expires_in_days": 1})
	var tok struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(tb, &tok)
	if c == 201 && tok.Token != "" {
		c, _ = h.api.do("POST", "/api/backups/repository/move", map[string]string{"mode": "copy", "repository": repo("b")}, "Authorization: Bearer "+tok.Token)
		h.check(t, "#192: an API token cannot move the backups", fmt.Sprint(c), "403")
		h.api.del(fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID))
	} else {
		h.fail(t, "#192 control: an API token is made", "HTTP %d %s", c, head(tb, 200))
	}
	ope, opw := fmt.Sprintf("e2e-move-op-%d@example.com", h.pid), fmt.Sprintf("e2e-move-operator-%d", h.pid)
	_, ub := h.api.post("/api/users", fmt.Sprintf(`{"email":%q,"password":%q,"role":"operator","site_ids":[%d]}`, ope, opw, id))
	if opid := jsonField(ub, "id"); opid != "" {
		op := newAPIClient(h, ope, opw)
		op.login()
		h.check(t, "#192: an operator cannot move the backups",
			code(op.post("/api/backups/repository/move", map[string]string{"mode": "copy", "repository": repo("b")})), "403")
		h.api.del("/api/users/" + opid)
	} else {
		h.fail(t, "#192 control: an operator account is made", "%s", head(ub, 200))
	}

	// copy: A to B, by the panel.
	moveTo := func(name, mode string) (string, string) {
		_, b := h.api.post("/api/backups/repository/move", map[string]string{"mode": mode, "repository": repo(name)})
		tid := taskIDOf(b)
		st := h.api.waitTask(tid, 20*time.Minute)
		_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", tid))
		return st, jsonField(tb, "error")
	}
	moveStart := time.Now()
	st, msg := moveTo("b", "copy")
	h.check(t, "#192: the panel copies the backups from A to B", st, "succeeded")
	if st != "succeeded" {
		t.Logf("move: %s", msg)
		return
	}
	_, sb = h.api.get("/api/settings")
	h.check(t, "#192: after the move the setting names B", jsonField(sb, "backup_repository"), repo("b"))
	for _, rec := range h.siteBackups(id) {
		h.check(t, "#192: record "+rec.SnapshotID[:8]+" names B", rec.Repository, repo("b"))
	}
	resticB := h.resticOn(realRestic, repo("b"), pw)
	var copied []struct {
		ID       string `json:"id"`
		Original string `json:"original"`
	}
	lr := resticB("snapshots", "--json", "--no-lock")
	_ = json.Unmarshal([]byte(lr.Out), &copied)
	renamed := false
	for _, s := range copied {
		renamed = renamed || strings.HasPrefix(s.Original, snap1) && !strings.HasPrefix(s.ID, snap1)
	}
	h.assert(t, "#192: restic copy gave the backups new IDs in B (the old ones as original)", renamed, "B: %s", head([]byte(lr.Out), 400))
	// The site's newest backup when the move ran (the scheduler may have
	// taken one of its own since the two above) is tested again from B.
	var newest int64
	for _, rec := range h.siteBackups(id) {
		if at, err := time.Parse(time.RFC3339Nano, rec.CreatedAt); err == nil && at.Before(moveStart) && rec.ID > newest {
			newest = rec.ID
		}
	}
	h.assert(t, "#192: the newest backup is test-restored from B after the move", newest >= r2 && h.waitVerifiedAfter(id, newest, moveStart, 10*time.Minute), "record %d", newest)

	gone := os.RemoveAll(repo("a")) == nil && !resticA("cat", "config").OK()
	h.assert(t, "#192 control: A is deleted", gone, "A still opens")
	if !gone {
		return
	}

	// The old records restore from B in every mode.
	restore := func(name string, rec int64, body map[string]any) string {
		body["confirm"] = dom
		_, b := h.api.post(fmt.Sprintf("/api/backups/%d/restore", rec), body)
		st := h.api.waitTask(taskIDOf(b), 15*time.Minute)
		if st != "succeeded" {
			_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", taskIDOf(b)))
			t.Logf("%s: %s", name, head(tb, 400))
		}
		return st
	}
	marker("v3")
	h.check(t, "#192: a full restore of a moved backup succeeds", restore("full", r1, map[string]any{"mode": "full"}), "succeeded")
	h.check(t, "#192: the full restore brings the file back", file(), "v1")
	h.check(t, "#192: the full restore brings the database back", row(), "v1")
	marker("v3")
	h.check(t, "#192: a files restore of a moved backup succeeds", restore("files", r1, map[string]any{"mode": "files"}), "succeeded")
	h.check(t, "#192: the files restore brings the file back", file(), "v1")
	h.check(t, "#192: the files restore leaves the database", row(), "v3")
	h.check(t, "#192: a database restore of a moved backup succeeds", restore("database", r1, map[string]any{"mode": "database"}), "succeeded")
	h.check(t, "#192: the database restore brings the row back", row(), "v1")
	marker("v3")
	link, _ := os.Readlink(filepath.Join("/srv/sites", dom, "current"))
	rel := strings.TrimPrefix(strings.TrimPrefix(link, "/srv/sites/"+dom+"/"), "./")
	h.check(t, "#192: a one-path restore of a moved backup succeeds",
		restore("paths", r2, map[string]any{"mode": "files", "paths": []string{rel + "/cg-move.txt"}}), "succeeded")
	h.check(t, "#192: the one-path restore brings the file back", file(), "v2")
	_, vb := h.api.post(fmt.Sprintf("/api/backups/%d/verify", r1), map[string]any{})
	h.check(t, "#192: a moved backup passes its restore test from B", h.api.waitTask(taskIDOf(vb), 10*time.Minute), "succeeded")
	_, ab := h.api.post(fmt.Sprintf("/api/backups/%d/archive", r1), map[string]string{"part": "full"})
	h.check(t, "#192: a moved backup's download is prepared", h.api.waitTask(taskIDOf(ab), 10*time.Minute), "succeeded")
	_, gz := h.api.get(fmt.Sprintf("/api/backups/%d/archive?part=full", r1))
	got := archiveEntries(gz)
	h.assert(t, "#192: the download holds the moved backup's file and database",
		strings.HasSuffix(got["files/"+rel+"/cg-move.txt"], "v1\n") && strings.Contains(got["databases/"+dbName+".sql"], "v1"),
		"%d entries", len(got))

	// moved: B copied by hand to C, the same repository.
	r := h.run("cp", "-a", repo("b"), repo("c"))
	h.assert(t, "#192 control: B is copied by hand to C", r.OK(), "%s", r)
	// Through cgctl, with a password session (the route is a session's).
	conf := fmt.Sprintf("/root/cg-e2e-move-%d.env", h.pid)
	_ = os.WriteFile(conf, []byte(fmt.Sprintf("CLOUDGROUND_EMAIL=%s\nCLOUDGROUND_PASSWORD=%s\n", h.Email, h.PW)), 0o600)
	defer h.undo("remove the cgctl sign-in file (#192)", func() { _ = os.Remove(conf) })()
	r = h.run("env", "-u", "CLOUDGROUND_URL", "-u", "CLOUDGROUND_EMAIL", "-u", "CLOUDGROUND_PASSWORD", "-u", "CLOUDGROUND_TOKEN",
		"-u", "CLOUDGROUND_TOKEN_FILE", "CLOUDGROUND_CONFIG="+conf, cgctlBinary, "--wait", "--timeout", "20m",
		"backup", "move", repo("c"), "--already-copied")
	h.assert(t, "#192: cgctl backup move --already-copied takes a repository copied by hand", r.OK() && strings.Contains(r.Out, `"succeeded"`), "%s", r)
	if !r.OK() {
		return
	}
	for _, rec := range h.siteBackups(id) {
		h.check(t, "#192: record "+rec.SnapshotID[:8]+" names C", rec.Repository, repo("c"))
	}
	_ = os.RemoveAll(repo("b"))
	marker("v3")
	h.check(t, "#192: a backup moved twice restores from C", restore("files", r2, map[string]any{"mode": "files"}), "succeeded")
	h.check(t, "#192: the restore from C brings the file back", file(), "v2")

	// Refused, nothing moved.
	unmoved := func(name string) {
		_, sb := h.api.get("/api/settings")
		ok := jsonField(sb, "backup_repository") == repo("c")
		for _, rec := range h.siteBackups(id) {
			ok = ok && rec.Repository == repo("c")
		}
		h.assert(t, "#192: "+name+": the setting and every record stay on C", ok, "setting %s", jsonField(sb, "backup_repository"))
	}
	resticD := h.resticOn(realRestic, repo("d"), pw)
	h.assert(t, "#192 control: another repository D is made", resticD("init").OK(), "init D")
	st, msg = moveTo("d", "moved")
	h.assert(t, "#192: another repository is refused", st == "failed" && strings.Contains(msg, "another repository"), "%s %q", st, msg)
	unmoved("another repository")

	h.run("cp", "-a", repo("c"), repo("e"))
	resticE := h.resticOn(realRestic, repo("e"), pw)
	h.forgetByOriginal(resticE, snap1)
	er := resticE("snapshots", "--json", "--no-lock")
	h.assert(t, "#192 control: E lacks one recorded snapshot", er.OK() && !strings.Contains(er.Out, snap1), "E: %s", er)
	st, msg = moveTo("e", "moved")
	h.assert(t, "#192: a copy missing a recorded snapshot is refused", st == "failed" && strings.Contains(msg, "not in the new repository"), "%s %q", st, msg)
	unmoved("a snapshot missing")

	// A failure after restic copied: C itself lacks a recorded snapshot
	// now, so the copy to F succeeds and the check after it fails.
	h.forgetByOriginal(h.resticOn(realRestic, repo("c"), pw), snap2)
	st, msg = moveTo("f", "copy")
	h.assert(t, "#192: a check that fails after the copy moves nothing", st == "failed" && strings.Contains(msg, "not in the new repository"), "%s %q", st, msg)
	fr := h.resticOn(realRestic, repo("f"), pw)("snapshots", "--json", "--no-lock")
	h.assert(t, "#192 control: the copy to F did run before the check", fr.OK() && strings.Contains(fr.Out, snap1), "F: %s", fr)
	unmoved("the check after the copy")
}

// siteBackup is one record of a site's backup list.
type siteBackup struct {
	ID           int64  `json:"id"`
	SnapshotID   string `json:"snapshot_id"`
	Repository   string `json:"repository"`
	Kind         string `json:"kind"`
	VerifyStatus string `json:"verify_status"`
	VerifiedAt   string `json:"verified_at"`
	CreatedAt    string `json:"created_at"`
}

// siteBackups is the site's backup records, newest first.
func (h *harness) siteBackups(site int64) []siteBackup {
	_, b := h.api.get(fmt.Sprintf("/api/sites/%d/backups", site))
	var list []siteBackup
	_ = json.Unmarshal(b, &list)
	return list
}

// waitVerifiedAfter waits for record rec of site to pass a restore test
// that ran after since.
func (h *harness) waitVerifiedAfter(site, rec int64, since time.Time, timeout time.Duration) bool {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(pollEvery) {
		for _, b := range h.siteBackups(site) {
			if b.ID != rec || b.VerifyStatus != "passed" {
				continue
			}
			if at, err := time.Parse(time.RFC3339Nano, b.VerifiedAt); err == nil && at.After(since) {
				return true
			}
		}
	}
	return false
}

// forgetByOriginal forgets, with restic itself, every snapshot of a
// repository whose ID or original starts with id: a recorded backup taken
// out behind the panel's back.
func (h *harness) forgetByOriginal(restic func(args ...string) result, id string) {
	var snaps []struct {
		ID       string `json:"id"`
		Original string `json:"original"`
	}
	_ = json.Unmarshal([]byte(restic("snapshots", "--json", "--no-lock").Out), &snaps)
	for _, s := range snaps {
		if strings.HasPrefix(s.ID, id) || strings.HasPrefix(s.Original, id) {
			restic("forget", s.ID)
		}
	}
}

// archiveEntries is every regular file of a .tar.gz, by name.
func archiveEntries(gz []byte) map[string]string {
	out := map[string]string{}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return out
	}
	tr := tar.NewReader(zr)
	for {
		hd, err := tr.Next()
		if err != nil {
			return out
		}
		if hd.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			out[hd.Name] = string(b)
		}
	}
}
