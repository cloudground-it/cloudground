//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupSelective (#23): an SFTP backup destination with the panel's own
// key, on a second site of this box (its SFTP account, keys only, chrooted
// to its root); a backup of a first site there, restore-tested; one file of
// it restored while another changed file stays as it is, behind a
// restore-tested safety snapshot; the backup downloaded as an archive that
// holds the file as it was. The server's repository setting is put back.
func (h *harness) backupSelective(t *testing.T) {
	_, sb := h.api.get("/api/settings")
	orig := jsonField(sb, "backup_repository")
	if orig == "" || strings.Contains(orig, "••") {
		t.Logf("(the section needs a configured repository without credentials in its URL, to put it back after; found %q)", orig)
		return
	}
	src := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("bsel-src")))
	dst := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("bsel-dst")))
	if h.api.waitActive(src, 4*time.Minute) != "active" || h.api.waitActive(dst, 4*time.Minute) != "active" {
		h.fail(t, "control: the section's two sites provision", "sites %d %d", src, dst)
		return
	}
	srcSite, dstSite := h.api.site(src), h.api.site(dst)
	_, rawSrc := h.api.get(fmt.Sprintf("/api/sites/%d", src))
	_, rawDst := h.api.get(fmt.Sprintf("/api/sites/%d", dst))
	srcRoot, dstRoot := jsonField(rawSrc, "root_path"), jsonField(rawDst, "root_path")

	// The panel's key, accepted by the destination site's SFTP account.
	st, kb := h.api.post("/api/settings/backup-ssh-key", map[string]string{})
	pub := jsonField(kb, "public_key")
	h.assert(t, "#23: the panel makes its SFTP key", st == 200 && strings.HasPrefix(pub, "ssh-ed25519 "), "HTTP %d %s", st, head(kb, 200))
	if pub == "" {
		return
	}
	h.check(t, "control: the destination site's SFTP takes the panel's key",
		code(h.api.post(fmt.Sprintf("/api/sites/%d/sftp", dst), map[string]any{"enable": true, "keys_only": true, "public_key": pub})), "200")
	dir := "shared"
	if fi, err := os.Stat(filepath.Join(dstRoot, "shared")); err != nil || !fi.IsDir() {
		dir = "logs"
	}
	repo := fmt.Sprintf("sftp://%s@127.0.0.1:%s//%s/cg-e2e-repo", dstSite.SystemUser, h.sshPort(), dir)
	st, rb := h.api.put("/api/settings", map[string]string{"backup_repository": repo})
	h.assert(t, "#23: an SFTP repository on the destination site is accepted (its pairing test passes)", st == 200, "HTTP %d %s", st, head(rb, 400))
	defer h.undo("put the backup repository back", func() {
		if st, b := h.api.put("/api/settings", map[string]string{"backup_repository": orig}); st != 200 {
			h.root.Logf("could not put the backup repository back to %s: HTTP %d %s", orig, st, b)
		}
	})()
	if st != 200 {
		return
	}
	_, err := os.Stat(filepath.Join(dstRoot, dir, "cg-e2e-repo", "config"))
	h.assert(t, "#23: the repository is on the SFTP destination", err == nil, "no restic config under %s/%s/cg-e2e-repo: %v", dstRoot, dir, err)

	// A backup of the source site with two files in it.
	write := func(name, content string) string {
		return code(h.api.post(fmt.Sprintf("/api/sites/%d/files/write", src), map[string]string{"path": "current/" + name, "content": content}))
	}
	h.check(t, "control: a file is written", write("cg-e2e-restore.txt", "v1\n"), "200")
	h.check(t, "control: another file is written", write("cg-e2e-other.txt", "keep-v1\n"), "200")
	link, _ := os.Readlink(filepath.Join(srcRoot, "current"))
	release := strings.TrimPrefix(strings.TrimPrefix(link, srcRoot+"/"), "./")
	st, bb := h.api.post(fmt.Sprintf("/api/sites/%d/backups", src), nil)
	h.check(t, "#23: a backup to the SFTP destination succeeds and passes its restore test", h.api.waitTask(taskIDOf(bb), 10*time.Minute), "succeeded")
	backupID := h.newestBackup(src, "full")
	if st != 202 || backupID == 0 {
		h.fail(t, "control: the backup is recorded", "HTTP %d %s", st, head(bb, 200))
		return
	}

	// One file back, the other left as it is now.
	h.check(t, "control: the files change", write("cg-e2e-restore.txt", "v2\n")+write("cg-e2e-other.txt", "keep-v2\n"), "200200")
	path := filepath.ToSlash(filepath.Join(release, "cg-e2e-restore.txt"))
	st, rsb := h.api.post(fmt.Sprintf("/api/backups/%d/restore", backupID), map[string]any{"confirm": srcSite.Domain, "mode": "files", "paths": []string{path}})
	h.check(t, "#23: a one-file restore is accepted", fmt.Sprint(st), "202")
	h.check(t, "#23: the one-file restore succeeds", h.api.waitTask(taskIDOf(rsb), 10*time.Minute), "succeeded")
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(srcRoot, "current", name))
		return string(b)
	}
	h.check(t, "#23: the chosen file is as it was in the backup", read("cg-e2e-restore.txt"), "v1\n")
	h.check(t, "#23: the rest of the site is left as it is", read("cg-e2e-other.txt"), "keep-v2\n")
	h.assert(t, "#23: a restore-tested safety snapshot was taken first", h.newestBackup(src, "pre-restore") != 0, "no passed pre-restore backup")

	// The backup, downloaded.
	st, ab := h.api.post(fmt.Sprintf("/api/backups/%d/archive", backupID), map[string]string{"part": "files"})
	h.check(t, "#23: preparing a download succeeds", h.api.waitTask(taskIDOf(ab), 10*time.Minute), "succeeded")
	st, gz := h.api.get(fmt.Sprintf("/api/backups/%d/archive?part=files", backupID))
	want := "files/" + path
	got, found := "", false
	if zr, err := gzip.NewReader(bytes.NewReader(gz)); err == nil {
		tr := tar.NewReader(zr)
		for {
			hd, err := tr.Next()
			if err != nil {
				break
			}
			if hd.Name == want {
				b, _ := io.ReadAll(tr)
				got, found = string(b), true
			}
		}
	}
	h.assert(t, "#23: the download is a .tar.gz holding the file as it was", st == 200 && found && got == "v1\n", "HTTP %d, %d bytes, %s found=%v %q", st, len(gz), want, found, got)
	_, audit := h.api.get("/api/audit")
	h.assert(t, "#23: the download is audited", contains(audit, "backup.download"), "no backup.download in the audit log")
	h.check(t, "#23: a downloaded archive is gone from the server", code(h.api.get(fmt.Sprintf("/api/backups/%d/archive?part=files", backupID))), "404")
}

// taskIDOf reads {"task": {"id": …}}.
func taskIDOf(b []byte) int64 {
	var v struct {
		Task struct {
			ID int64 `json:"id"`
		} `json:"task"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Task.ID
}

// newestBackup is the id of the site's newest passed backup of kind, 0 for
// none.
func (h *harness) newestBackup(site int64, kind string) int64 {
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
