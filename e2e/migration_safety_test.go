//go:build e2e

package e2e

import (
	"archive/tar"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// migrationSafety (#41): a migration onto a site that already has content
// takes a restore-tested pre-migration snapshot first and names it in the
// task, and restoring that snapshot puts back the files and the database
// the import replaced. On the build before, an import took no snapshot and
// the site's previous state was gone.
func (h *harness) migrationSafety(t *testing.T) {
	d := &dbFx{h: h, tag: fmt.Sprintf("e2e%dm", h.pid%100000)}
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-mig-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	_, sb := h.api.get("/api/settings")
	prev := jsonField(sb, "backup_repository")
	restoreSettings := h.undo("restore the backup repository setting (#41)", func() {
		h.api.relogin()
		if prev == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prev)
		}
		_ = os.RemoveAll(brepo)
	})
	defer restoreSettings()
	body, _ := json.Marshal(map[string]string{"backup_repository": brepo, "backup_password": bpw})
	if code(h.api.put("/api/settings", body)) != "200" {
		h.fail(t, "#41 control: the backup repository is accepted", "PUT /api/settings refused %s", brepo)
		return
	}

	id := d.staticSite(t, "migsafe")
	if id == 0 {
		return
	}
	defer func() {
		h.check(t, "#41 control: the site's delete is accepted", h.api.deleteSite(id), "202")
		h.check(t, "#41 control: the site's teardown completes", h.api.teardownGone(id), "yes")
	}()
	site := h.api.site(id)
	dom, su, root := site.Domain, site.SystemUser, "/srv/sites/"+site.Domain
	db := d.tag + "_content"
	if !d.createDB(t, id, db) {
		return
	}
	// The content the site has before the import: a file and a row.
	d.root("CREATE TABLE `" + db + "`.t (v TEXT); INSERT INTO `" + db + "`.t VALUES ('before')")
	h.mustWriteAs(t, root+"/current/cg-before.txt", "before\n", su)
	h.check(t, "#41 control: the site has its content before the import", readTrim(root+"/current/cg-before.txt")+" "+d.root("SELECT v FROM `"+db+"`.t"), "before before")

	archive, dump := root+"/shared/cg-e2e-safety.tgz", root+"/shared/cg-e2e-safety.sql"
	cleanup := h.undo("remove the #41 migration inputs", func() { h.remove(archive, dump) })
	defer cleanup()
	err := writeTGZ(archive, func(tw *tar.Writer) error {
		for name, body := range map[string]string{"index.html": "<p>imported</p>\n", "cg-imported.txt": "imported\n"} {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := io.WriteString(tw, body); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = h.writeAs(dump, "DROP TABLE IF EXISTS t;\nCREATE TABLE t (v TEXT);\nINSERT INTO t VALUES ('imported');\n", su, 0o600)
	}
	if err != nil {
		t.Logf("fixture: %v", err)
	}

	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/migration", id),
		fmt.Sprintf(`{"archive":"shared/cg-e2e-safety.tgz","sql":"shared/cg-e2e-safety.sql","database":%q,"confirm":%q}`, db, dom))
	task := taskID(b)
	h.check(t, "#41: the migration onto a site with content succeeds", h.api.waitTask(task, 10*time.Minute), "succeeded")
	h.check(t, "#41 control: the import replaced the files and the database",
		yesNo(exists(root+"/current/cg-imported.txt") && !exists(root+"/current/cg-before.txt"))+" "+d.root("SELECT v FROM `"+db+"`.t"), "yes imported")

	var rows []backupRow
	_, lb := h.api.get(fmt.Sprintf("/api/sites/%d/backups", id))
	_ = json.Unmarshal(lb, &rows)
	safety, ok := newest(rows, func(r backupRow) bool { return r.Kind == "pre-migration" })
	h.assert(t, "#41: the migration took a pre-migration safety snapshot", ok, "backups of %s: %s", dom, head(lb, 600))
	if !ok {
		return
	}
	h.check(t, "#41: the pre-migration snapshot passed its restore test (verify_status)", safety.VerifyStatus, "passed")
	_, rec := h.api.get(fmt.Sprintf("/api/tasks/%d", task))
	h.assert(t, "#41: the migration's task names its safety snapshot", safety.SnapshotID != "" && strings.Contains(string(rec), "safety snapshot "+safety.SnapshotID),
		"snapshot %q, task record: %s", safety.SnapshotID, head(rec, 800))

	_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", safety.ID), fmt.Sprintf(`{"confirm":%q,"mode":"full"}`, dom))
	h.check(t, "#41: restoring the pre-migration snapshot succeeds", h.api.waitTask(taskID(b), 10*time.Minute), "succeeded")
	h.check(t, "#41: the restore brought the site's files back", readTrim(root+"/current/cg-before.txt"), "before")
	h.assert(t, "#41: the restore took the imported files away", !exists(root+"/current/cg-imported.txt"), "current/cg-imported.txt is still there")
	h.check(t, "#41: the restore brought the site's database back", d.root("SELECT v FROM `"+db+"`.t"), "before")
}
