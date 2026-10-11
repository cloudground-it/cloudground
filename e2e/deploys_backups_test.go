//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// deploysAndBackups is the R-4.3 section: deployments are a state machine
// whose new releases are ULIDs, and one backup snapshot can be deleted.
// It configures a throwaway backup repository of its own (and puts the
// setting back), so it runs alone.
func (h *harness) deploysAndBackups(t *testing.T) {
	h.clearStandIns(t, "restic", "mariadb-dump")
	if f, stg, ok := h.featureStaging(t); ok {
		f.deployStateMachine(t, stg)
		f.snapshotDelete(t)
	}
}

var ulidRe = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

type deployRow struct {
	ID        int64  `json:"id"`
	ReleaseID string `json:"release_id"`
	Status    string `json:"status"`
}

func (f *feat) deployments() map[int64]deployRow {
	var rows []deployRow
	_, b := f.h.api.get(f.api("/deployments"))
	_ = json.Unmarshal(b, &rows)
	out := map[int64]deployRow{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// deployStateMachine: a deploy is created (built, not live) under a ULID;
// a promote makes it the only live release and supersedes the one before;
// a rollback records which is live again; a failed build is failed and
// cannot be promoted (409, nothing queued). Rollbacks, by hand and a safe
// push's automatic one, go to the release that was live before, past a
// release built and never promoted.
func (f *feat) deployStateMachine(t *testing.T, stg siteInfo) {
	h := f.h
	root := "/srv/sites/" + f.fs
	before := readlink(root + "/current")
	// The releases are copies of the live one: whichever is live at the
	// end serves what the site served before. Put the old one back anyway.
	defer func() {
		if cur := readlink(root + "/current"); cur != before {
			for _, d := range f.deployments() {
				if "releases/"+d.ReleaseID == before {
					_, b := h.api.post(fmt.Sprintf("/api/deployments/%d/promote", d.ID), `{}`)
					h.api.waitTask(taskID(b), 120*time.Second)
				}
			}
		}
	}()

	deploy := func(src string) (deployRow, string) {
		_, b := h.api.post(f.api("/deployments"), map[string]string{"source_dir": src})
		id, _ := json.Number(jsonField(b, "deployment_id")).Int64()
		st := h.api.waitTask(taskID(b), 300*time.Second)
		return f.deployments()[id], st
	}
	promote := func(d deployRow) (int, string) {
		c, b := h.api.post(fmt.Sprintf("/api/deployments/%d/promote", d.ID), `{}`)
		if c != 202 {
			return c, ""
		}
		return c, h.api.waitTask(taskID(b), 120*time.Second)
	}

	a, st := deploy(root + "/current")
	h.check(t, "R-4.3: a deploy succeeds", st, "succeeded")
	h.assert(t, "R-4.3: a new release is named by a ULID", ulidRe.MatchString(a.ReleaseID), "release id %q", a.ReleaseID)
	h.check(t, "R-4.3: a built release is created, not live", a.Status+" "+readlink(root+"/current"), "created "+before)
	h.assert(t, "R-4.3: the release directory carries the ULID", exists(root+"/releases/"+a.ReleaseID), "releases/%s is missing", a.ReleaseID)

	c, st := promote(a)
	h.check(t, "R-4.3: the release is promoted", fmt.Sprint(c, " ", st, " ", f.deployments()[a.ID].Status), "202 succeeded promoted")
	// Built and never promoted, between a and b: the agent's own choice
	// ("the newest release older than the live one") would be this one.
	x, _ := deploy(root + "/current")
	b, _ := deploy(root + "/current")
	h.assert(t, "R-4.3: release ids increase with time", a.ReleaseID < x.ReleaseID && x.ReleaseID < b.ReleaseID, "%s, %s, %s", a.ReleaseID, x.ReleaseID, b.ReleaseID)
	promote(b)
	deps := f.deployments()
	h.check(t, "R-4.3: promoting a release supersedes the live one", deps[a.ID].Status+" "+deps[b.ID].Status, "superseded promoted")
	live := 0
	for _, d := range deps {
		if d.Status == "promoted" {
			live++
		}
	}
	h.check(t, "R-4.3: exactly one release of the site is recorded live", fmt.Sprint(live), "1")

	_, rb := h.api.post(f.api("/rollback"), `{}`)
	h.check(t, "R-4.3: the rollback succeeds", h.api.waitTask(taskID(rb), 120*time.Second), "succeeded")
	deps = f.deployments()
	h.check(t, "R-4.3: the rollback goes to the release that was live before, not the newer unpromoted one", readlink(root+"/current"), "releases/"+a.ReleaseID)
	h.check(t, "R-4.3: a rollback records which release is live again", deps[a.ID].Status+" "+deps[b.ID].Status+" "+deps[x.ID].Status, "promoted rolled_back created")
	h.check(t, "R-4.3: a rollback to a release that was never live is refused (409)",
		code(h.api.post(f.api("/rollback"), map[string]int64{"deployment_id": x.ID})), "409")

	f.safePushRegressionRollback(t, stg, a, deploy)

	// A build that fails (its source does not exist) is failed, and a
	// promote of it is refused before any job.
	bad, st := deploy(fmt.Sprintf("%s/cg-e2e-missing-%d", root, h.pid))
	h.check(t, "R-4.3: a deploy from a missing directory fails", st+" "+bad.Status, "failed failed")
	c, _ = promote(bad)
	h.check(t, "R-4.3: a release that was never built cannot be promoted (409)", fmt.Sprint(c), "409")
}

// safePushRegressionRollback: a safe push whose release fails production
// after its promotion is rolled back to the release that was live before
// the push (a), past a release built and never promoted (c). Staging gets a
// must-use plugin that answers 500 on any host but staging's own, so the
// comparison (made against staging) passes and production regresses.
func (f *feat) safePushRegressionRollback(t *testing.T, stg siteInfo, a deployRow, deploy func(string) (deployRow, string)) {
	h := f.h
	root := "/srv/sites/" + f.fs
	c, _ := deploy(root + "/current")
	h.check(t, "R-4.3 control: a release built and not promoted sits between the live one and the push",
		c.Status+" "+readlink(root+"/current"), "created releases/"+a.ReleaseID)
	stgu := h.api.site(stg.ID).SystemUser
	plugin := "/srv/sites/" + stg.Domain + "/current/wp-content/mu-plugins/cg-e2e-prod-500.php"
	h.mustWriteAs(t, plugin, fmt.Sprintf("<?php if (($_SERVER['HTTP_HOST'] ?? '') !== %q && PHP_SAPI !== 'cli') { http_response_code(500); exit; }\n", stg.Domain), stgu)
	clean := h.undo("remove the R-4.3 production-500 plugin from staging", func() { _ = os.Remove(plugin) })
	defer clean()
	_, b := h.api.post(f.api("/safe-push"), `{"paths":["/sample-page/"]}`)
	pushID, _ := json.Number(jsonField(b, "deployment_id")).Int64()
	h.check(t, "R-4.3 control: the safe push regresses after its promotion and fails", h.api.waitTask(taskID(b), 900*time.Second), "failed")
	deps := f.deployments()
	h.check(t, "R-4.3: the safe push's automatic rollback lands on the release live before it, not the unpromoted one",
		readlink(root+"/current"), "releases/"+a.ReleaseID)
	h.check(t, "R-4.3: the regressed push is rolled back and the release before it live again",
		deps[pushID].Status+" "+deps[a.ID].Status+" "+deps[c.ID].Status, "rolled_back promoted created")
	h.check(t, "R-4.3: a release rolled back for a regression is not a rollback target (409)",
		code(h.api.post(f.api("/rollback"), map[string]int64{"deployment_id": pushID})), "409")
}

// snapshotDelete: one snapshot is deleted from the repository with its
// database half; the site's newest verified one is refused; the repository
// is left without a lock and its remaining snapshot still restores.
func (f *feat) snapshotDelete(t *testing.T) {
	h := f.h
	realRestic, err := exec.LookPath("restic")
	if err != nil {
		h.fail(t, "R-4.3 fixture: restic is on PATH", "R-4.3 fixture: %v", err)
		return
	}
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-del-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	_, sb := h.api.get("/api/settings")
	prevRepo := jsonField(sb, "backup_repository")
	restoreSettings := h.undo("restore the backup repository setting (R-4.3)", func() {
		h.api.relogin()
		if prevRepo == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prevRepo)
		}
		_ = os.RemoveAll(brepo)
	})
	defer restoreSettings()
	setBody, _ := json.Marshal(map[string]string{"backup_repository": brepo, "backup_password": bpw})
	h.check(t, "R-4.3 control: the backup repository is accepted", code(h.api.put("/api/settings", setBody)), "200")

	restic := h.resticOn(realRestic, brepo, bpw)
	type snap struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	// snapshots lists the repository; ok is false when restic could not,
	// which proves neither that a snapshot is there nor that it is gone.
	snapshots := func() (out []snap, ok bool) {
		r := restic("snapshots", "--json", "--no-lock")
		if err := json.Unmarshal([]byte(r.Out), &out); err != nil || !r.OK() {
			t.Logf("restic snapshots: %s", r)
			return nil, false
		}
		return out, true
	}
	// lists reports whether the repository could be listed and whether it
	// holds a snapshot whose ID starts with prefix.
	lists := func(prefix string) (readable, found bool) {
		all, ok := snapshots()
		for _, s := range all {
			if prefix != "" && strings.HasPrefix(s.ID, prefix) {
				found = true
			}
		}
		return ok, found
	}
	has := func(prefix string) bool { ok, found := lists(prefix); return ok && found }
	gone := func(prefix string) bool { ok, found := lists(prefix); return ok && !found }

	for i := range 2 {
		_, b := h.api.post(f.api("/backups"), `{}`)
		h.check(t, fmt.Sprintf("R-4.3 control: backup %d runs and passes its restore test", i+1), h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
	}
	rows := f.backupsList()
	if len(rows) < 2 {
		h.fail(t, "R-4.3 control: two backups are listed", "%d backups listed", len(rows))
		return
	}
	newestRow, _ := newest(rows, func(r backupRow) bool { return r.VerifyStatus == "passed" })
	older, _ := newest(rows, func(r backupRow) bool { return r.VerifyStatus == "passed" && r.ID != newestRow.ID })

	h.check(t, "R-4.3: the site's newest verified snapshot cannot be deleted (409)",
		code(h.api.del(fmt.Sprintf("/api/backups/%d", newestRow.ID))), "409")

	// The database snapshots the older backup pairs with (one per database
	// of the site, F9b) go with it.
	var dbHalves []string
	all, _ := snapshots()
	for _, s := range all {
		if older.SnapshotID != "" && strings.HasPrefix(s.ID, older.SnapshotID) {
			for _, tag := range s.Tags {
				if v, ok := strings.CutPrefix(tag, "db:"); ok {
					dbHalves = append(dbHalves, v)
				}
			}
		}
	}
	allDB := func(want func(string) bool) bool {
		for _, d := range dbHalves {
			if !want(d) {
				return false
			}
		}
		return len(dbHalves) > 0
	}
	h.assert(t, "R-4.3 control: the older backup has a database snapshot", allDB(has), "db: tags %v on %s (each must be in the repository)", dbHalves, older.SnapshotID)

	c, b := h.api.del(fmt.Sprintf("/api/backups/%d", older.ID))
	h.check(t, "R-4.3: deleting a snapshot is queued (202)", fmt.Sprint(c), "202")
	h.check(t, "R-4.3: the deletion succeeds", h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
	unlisted := true
	for _, r := range f.backupsList() {
		unlisted = unlisted && r.ID != older.ID
	}
	h.assert(t, "R-4.3: the deleted snapshot's record is gone", unlisted, "backup %d is still listed", older.ID)
	h.assert(t, "R-4.3: the snapshot is forgotten in the repository", older.SnapshotID != "" && gone(older.SnapshotID), "restic still lists %s, or cannot list the repository", older.SnapshotID)
	h.assert(t, "R-4.3: its database snapshot is forgotten with it", allDB(gone), "restic still lists a database snapshot of %v, or cannot list the repository", dbHalves)
	h.assert(t, "R-4.3: the newest snapshot is kept", has(newestRow.SnapshotID), "restic lost %s, or cannot list the repository", newestRow.SnapshotID)
	lk := restic("list", "locks", "--no-lock")
	nlocks := fmt.Sprint(CountNonEmptyLines(lk.Out))
	if !lk.OK() {
		nlocks = "unreadable: " + lk.String()
	}
	h.check(t, "R-4.3: the deletion leaves no lock in the repository", nlocks, "0")

	// The prune left the remaining snapshot whole.
	_, vb := h.api.post(fmt.Sprintf("/api/backups/%d/verify", newestRow.ID), `{}`)
	h.check(t, "R-4.3: the snapshot left after a deletion still passes its restore test", h.api.waitTask(taskID(vb), 600*time.Second), "succeeded")
}
