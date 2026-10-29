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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// restoreAcrossUpdates (R-6.6, #37): a backup taken on a real older release
// restores after an update to the current build. For each old release of
// the bundle (built from its tag by scripts/restore-upgrade-bundle.sh, signed
// with a throwaway key, taking updates from a feed this section serves on
// the loopback): the release is installed on an empty state and set up; a
// WordPress site gets a marker post and a marker file, and a backup in a
// local repository that passes its restore test there; the panel's own
// update takes it to the current build (labelled the highest tag plus one
// patch): its guard, snapshot and schema steps run on the old release's real
// database. On the current build the old backup is still recorded, passes
// its restore test, and restores in every mode (full, files, database, one
// path) with the markers back, downloads, and retention leaves it alone. The
// site is deleted and the box's own build and state are put back after each
// release.
//
// It runs only with CG_E2E_RESTORE_BUNDLE naming the unpacked bundle
// (docs/e2e.md), alone, on a panel without sites: each round replaces the
// panel's state for its duration.
func (h *harness) restoreAcrossUpdates(t *testing.T) {
	bundle := os.Getenv("CG_E2E_RESTORE_BUNDLE")
	if bundle == "" {
		t.Log("R-6.6: CG_E2E_RESTORE_BUNDLE is not set: the section needs `make docker-restore-upgrade-bundle` unpacked on the box (docs/e2e.md); skipped")
		return
	}
	olds := strings.Fields(readTrim(filepath.Join(bundle, "old")))
	target := readTrim(filepath.Join(bundle, "head"))
	origin := readTrim(filepath.Join(bundle, "origin"))
	if len(olds) == 0 || target == "" || origin == "" {
		h.fail(t, "R-6.6: read the bundle", "old %q, head %q, origin %q", olds, target, origin)
		return
	}
	sites, raw := h.api.sites()
	if len(sites) > 0 || raw == nil {
		h.fail(t, "R-6.6: the panel has no sites before the section",
			"R-6.6 installs each old release on an empty state, which would leave these sites out of the panel: %d found; delete them first", len(sites))
		return
	}
	feed := &upgradeFeed{root: bundle}
	stopFeed, err := feed.serve(strings.TrimPrefix(origin, "http://"))
	if err != nil {
		h.fail(t, "R-6.6: serve the local release feed", "%v", err)
		return
	}
	defer stopFeed()

	box, err := h.saveBox()
	if err != nil {
		h.fail(t, "R-6.6: save the box's build and state", "%v", err)
		return
	}
	defer h.undo("put the box's own build and state back (R-6.6)", func() { h.putBoxBack(box, true) })()
	for _, old := range olds {
		h.restoreRound(t, bundle, feed, old, target)
		h.putBoxBack(box, false)
	}
}

// savedBox is the box's own build and state, put back after each round.
type savedBox struct {
	dir         string
	floor       []byte
	floorErr    error
	snapsBefore map[string]bool
}

const (
	r66State    = "/var/lib/cloudground"
	r66AgentDir = "/var/lib/cloudground-agent"
)

// r66AgentRecords are the agent's records of the sites and their databases.
var r66AgentRecords = []string{"sites.json", "databases.json"}

// saveBox copies the installed binaries, and the state database and its key
// with the API stopped (a consistent copy), then starts the API again.
func (h *harness) saveBox() (*savedBox, error) {
	dir, err := os.MkdirTemp("", "cg-e2e-r66-")
	if err != nil {
		return nil, err
	}
	b := &savedBox{dir: dir, snapsBefore: map[string]bool{}}
	for _, name := range []string{"panel-api", "panel-agent", "cgctl", "cg-site-shell"} {
		if data, err := os.ReadFile("/usr/local/bin/" + name); err == nil {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
				return nil, err
			}
		}
	}
	b.floor, b.floorErr = os.ReadFile(r66AgentDir + "/highest-release")
	for _, d := range listDir(r66AgentDir + "/update-snapshots") {
		b.snapsBefore[d] = true
	}
	h.run("systemctl", "stop", "cloudground-api.socket", "cloudground-api.service")
	defer h.run("systemctl", "start", "cloudground-api.socket", "cloudground-api.service")
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		return nil, err
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "secret.key"} {
		data, err := os.ReadFile(filepath.Join(r66State, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "state", name), data, 0o600); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "state.db")); err != nil {
		return nil, fmt.Errorf("no state database to save: %w", err)
	}
	// The agent's records of the sites and their databases go with the
	// panel's state (an update snapshots both).
	for _, name := range r66AgentRecords {
		if data, err := os.ReadFile(filepath.Join(r66AgentDir, name)); err == nil {
			if err := os.WriteFile(filepath.Join(dir, "state", "agent-"+name), data, 0o600); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

// emptyState stops the API and takes its database and key away.
func (h *harness) emptyState() {
	h.run("systemctl", "stop", "cloudground-api.socket", "cloudground-api.service")
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "secret.key"} {
		_ = os.Remove(filepath.Join(r66State, name))
	}
}

// putBoxBack puts the box's own state back and reinstalls its build over
// whatever a round left (cgctl install --from, unchecked: the development
// path), with the floor and the snapshot directory as they were. last
// removes the saved copy.
func (h *harness) putBoxBack(b *savedBox, last bool) {
	h.emptyState()
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "secret.key"} {
		data, err := os.ReadFile(filepath.Join(b.dir, "state", name))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(r66State, name), data, 0o600); err != nil {
			h.root.Logf("R-6.6 cleanup: put %s back: %v", name, err)
		}
		h.run("chown", "cloudground:cloudground", filepath.Join(r66State, name))
	}
	for _, name := range r66AgentRecords {
		if data, err := os.ReadFile(filepath.Join(b.dir, "state", "agent-"+name)); err == nil {
			_ = os.WriteFile(filepath.Join(r66AgentDir, name), data, 0o600)
		}
	}
	if b.floorErr == nil {
		_ = os.WriteFile(r66AgentDir+"/highest-release", b.floor, 0o600)
	} else {
		_ = os.Remove(r66AgentDir + "/highest-release")
	}
	r := h.run(filepath.Join(b.dir, "cgctl"), "install", "--from", b.dir, "--skip-packages")
	if !r.OK() {
		h.root.Logf("R-6.6 cleanup: reinstalling the box's build failed: %s", r)
	}
	for _, d := range listDir(r66AgentDir + "/update-snapshots") {
		if !b.snapsBefore[d] {
			_ = os.RemoveAll(filepath.Join(r66AgentDir, "update-snapshots", d))
		}
	}
	if last {
		_ = os.RemoveAll(b.dir)
	}
	h.api.relogin()
}

// setupURLRe finds the one-time setup link the installer prints (or the
// API logs) on an empty state.
var setupURLRe = regexp.MustCompile(`/setup/([A-Za-z0-9_-]{16,})`)

// restoreRound is one old release: installed on an empty state, a backup
// taken there, the panel updated to head, the backup restored on head.
func (h *harness) restoreRound(t *testing.T, bundle string, feed *upgradeFeed, old, target string) {
	name := "R-6.6 " + old
	h.emptyState()
	_ = os.Remove(r66AgentDir + "/highest-release") // the box's floor would refuse an old release
	// A fresh install, as on a new server: an old release's installer reads
	// the box's development build as an installed release and refuses to
	// replace it (it is no update path, internal/install SPEC §5 step 3a).
	h.run("systemctl", "stop", "cloudground-agent.service")
	_ = os.Remove("/usr/local/bin/panel-api")
	feed.list(old, target)
	// An old release's installer reads the box's development build as a
	// newer release: on an empty state, going down is what the round is.
	r := h.run("env", "CLOUDGROUND_VERSION="+old, "CLOUDGROUND_ALLOW_DOWNGRADE=1", filepath.Join(bundle, old, "cgctl"), "install",
		"--release", filepath.Join(bundle, old), "--skip-packages")
	h.assert(t, name+": installs from its signed release on an empty state", r.OK(), "%s", r)
	if !r.OK() {
		return
	}
	h.check(t, name+": runs", h.runningVersion(), old)

	// The setup link: in the installer's output, else in the API's journal.
	m := setupURLRe.FindStringSubmatch(r.Out + r.Err)
	for i := 0; m == nil && i < 30; i++ {
		time.Sleep(time.Second)
		m = setupURLRe.FindStringSubmatch(h.run("journalctl", "-u", "cloudground-api", "--since", "5 min ago", "-o", "cat").Out)
	}
	if m == nil {
		h.fail(t, name+": prints its setup link", "no /setup/<token> in the installer's output or the API's journal")
		return
	}
	c, sb := h.api.post("/api/setup", map[string]string{"email": h.Email, "password": h.PW, "token": m[1]})
	h.assert(t, name+": the first admin is set up with the printed token", c == http.StatusCreated, "HTTP %d %s", c, head(sb, 200))
	h.api.relogin()

	repo := fmt.Sprintf("/var/backups/cloudground/e2e-r66-%d-%s", h.pid, old)
	defer os.RemoveAll(repo)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	pw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	set, _ := json.Marshal(map[string]string{"backup_repository": repo, "backup_password": pw})
	h.check(t, name+": a local backup repository is accepted", code(h.api.put("/api/settings", set)), "200")

	dom := h.dom("r66-" + strings.ReplaceAll(old, ".", ""))
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"wordpress","title":"R-6.6","admin_email":"r66@%s","admin_user":"r66admin","admin_password":"r66-admin-password-%d"}`, dom, dom, h.pid))
	if st := h.api.waitActive(id, 8*time.Minute); st != "active" {
		h.fail(t, name+": a WordPress site is created", "site %d: %s", id, st)
		return
	}
	// The site goes with the round, on the round's own state, however it
	// ends: the box's state, put back after, never had it.
	defer func() {
		h.api.relogin()
		if c, _ := h.api.get(fmt.Sprintf("/api/sites/%d", id)); c == http.StatusOK {
			h.api.deleteSite(id)
			h.api.teardownGone(id)
		}
	}()
	site := h.api.site(id)
	root := "/srv/sites/" + dom
	cur := root + "/current"
	marker := fmt.Sprintf("cg-r66-%s-%d", old, time.Now().UnixNano())
	post := h.wp(site.SystemUser, cur, "post", "create", "--post_title="+marker, "--post_status=publish", "--porcelain")
	h.assert(t, name+": control: a marker post is written", post.OK() && post.Trim() != "", "%s", post)
	h.mustWriteAs(t, cur+"/cg-r66.txt", marker+"\n", site.SystemUser)
	hasPost := func() bool {
		r := h.wp(site.SystemUser, cur, "post", "list", "--post_type=post", "--format=ids", "--title="+marker)
		return r.OK() && r.Trim() != ""
	}
	file := func() string { return readTrim(cur + "/cg-r66.txt") }
	spoil := func() {
		h.wp(site.SystemUser, cur, "post", "delete", post.Trim(), "--force")
		h.mustWriteAs(t, cur+"/cg-r66.txt", "spoiled\n", site.SystemUser)
	}

	_, bb := h.api.post(fmt.Sprintf("/api/sites/%d/backups", id), map[string]any{})
	h.check(t, name+": a backup on "+old+" passes its restore test", h.api.waitTask(taskIDOf(bb), 15*time.Minute), "succeeded")
	rec := h.newestBackup(id, "full")
	if rec == 0 {
		h.fail(t, name+": the backup is recorded", "no passed full backup of site %d", id)
		return
	}
	_, vb := h.api.post(fmt.Sprintf("/api/backups/%d/verify", rec), map[string]any{})
	h.check(t, name+": the backup passes a restore test again on "+old, h.api.waitTask(taskIDOf(vb), 15*time.Minute), "succeeded")
	snap := h.recordSnapshot(id, rec)

	// The panel's own update, through the local feed.
	// The scheduler's first backup of the new site may be running: an
	// update waits for it (it refuses with 409 meanwhile).
	since := time.Now()
	c, ub := h.api.post("/api/panel/update", map[string]string{"version": target})
	for end := time.Now().Add(10 * time.Minute); c == http.StatusConflict && strings.Contains(string(ub), "running") && time.Now().Before(end); {
		time.Sleep(10 * time.Second)
		since = time.Now()
		c, ub = h.api.post("/api/panel/update", map[string]string{"version": target})
	}
	h.assert(t, name+": the panel's update to "+target+" is accepted", c == http.StatusOK, "HTTP %d %s", c, head(ub, 300))
	res := h.waitUpdateResult(r66AgentDir, since)
	h.assert(t, name+": the update to "+target+" is healthy", res.OK && res.Version == target, "result %+v, answer %s", res, head(ub, 200))
	h.check(t, name+": "+target+" runs after the update", h.runningVersion(), target)
	// The self-update installs no installer file: the new agent brings in
	// the release's kernel settings as it starts (ops/panel SPEC §2.7).
	dr := h.run(cgctlBinary, "doctor")
	h.assert(t, name+": the update brings "+target+"'s kernel settings, file and running values",
		regexp.MustCompile(`(?m)^=\s+sysctl\s+up to date\s*$`).MatchString(dr.Out), "%s",
		strings.Join(regexp.MustCompile(`(?m)^.*\bsysctl\b.*$`).FindAllString(dr.Out, -1), "\n"))
	h.api.relogin()
	if h.runningVersion() != target {
		return
	}

	// On target: the old backup, in every mode.
	h.check(t, name+": the old backup is still recorded on "+target, h.recordSnapshot(id, rec), snap)
	_, vb = h.api.post(fmt.Sprintf("/api/backups/%d/verify", rec), map[string]any{})
	h.check(t, name+": the old backup passes its restore test on "+target, h.api.waitTask(taskIDOf(vb), 15*time.Minute), "succeeded")
	restore := func(body map[string]any) string {
		body["confirm"] = dom
		_, b := h.api.post(fmt.Sprintf("/api/backups/%d/restore", rec), body)
		st := h.api.waitTask(taskIDOf(b), 20*time.Minute)
		if st != "succeeded" {
			_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", taskIDOf(b)))
			t.Logf("%s restore %v: %s", name, body["mode"], head(tb, 500))
		}
		return st
	}
	spoil()
	h.check(t, name+": a full restore of the old backup succeeds on "+target, restore(map[string]any{"mode": "full"}), "succeeded")
	h.assert(t, name+": the full restore brings the marker post back", hasPost(), "no post %q", marker)
	h.check(t, name+": the full restore brings the marker file back", file(), marker)
	spoil()
	h.check(t, name+": a files restore of the old backup succeeds on "+target, restore(map[string]any{"mode": "files"}), "succeeded")
	h.check(t, name+": the files restore brings the marker file back", file(), marker)
	h.assert(t, name+": the files restore leaves the database as it is", !hasPost(), "the post came back from a files restore")
	h.check(t, name+": a database restore of the old backup succeeds on "+target, restore(map[string]any{"mode": "database"}), "succeeded")
	h.assert(t, name+": the database restore brings the marker post back", hasPost(), "no post %q", marker)
	h.mustWriteAs(t, cur+"/cg-r66.txt", "spoiled\n", site.SystemUser)
	link, _ := os.Readlink(cur)
	rel := strings.TrimPrefix(strings.TrimPrefix(link, root+"/"), "./")
	h.check(t, name+": a one-path restore of the old backup succeeds on "+target,
		restore(map[string]any{"mode": "files", "paths": []string{rel + "/cg-r66.txt"}}), "succeeded")
	h.check(t, name+": the one-path restore brings the marker file back", file(), marker)

	_, ab := h.api.post(fmt.Sprintf("/api/backups/%d/archive", rec), map[string]string{"part": "full"})
	h.check(t, name+": the old backup's download is prepared on "+target, h.api.waitTask(taskIDOf(ab), 15*time.Minute), "succeeded")
	_, gz := h.api.get(fmt.Sprintf("/api/backups/%d/archive?part=full", rec))
	got := r66Archive(gz)
	var dump string
	for n, body := range got {
		if strings.HasPrefix(n, "databases/") {
			dump += body
		}
	}
	h.assert(t, name+": the download holds the marker file and the marker post",
		strings.TrimSpace(got["files/"+rel+"/cg-r66.txt"]) == marker && strings.Contains(dump, marker), "%d entries", len(got))

	// Retention keeps a fresh backup: nothing of the old one is forgotten.
	before := len(h.siteRecords(id))
	_, rb := h.api.post(fmt.Sprintf("/api/sites/%d/backups/retention", id), map[string]any{})
	h.check(t, name+": retention runs on "+target, h.api.waitTask(taskIDOf(rb), 10*time.Minute), "succeeded")
	h.assert(t, name+": retention forgets none of the backups", len(h.siteRecords(id)) == before && h.recordSnapshot(id, rec) == snap,
		"records %d -> %d", before, len(h.siteRecords(id)))

	h.check(t, name+": the site is deleted", h.api.deleteSite(id), "202")
	h.check(t, name+": the site is gone", h.api.teardownGone(id), "yes")
}

// r66Archive is every regular file of a .tar.gz, by name.
func r66Archive(gz []byte) map[string]string {
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
			b, _ := io.ReadAll(io.LimitReader(tr, 64<<20))
			out[hd.Name] = string(b)
		}
	}
}

// r66Record is one backup record as the list shows it.
type r66Record struct {
	ID         int64  `json:"id"`
	SnapshotID string `json:"snapshot_id"`
}

// siteRecords is the site's backup records.
func (h *harness) siteRecords(site int64) []r66Record {
	_, b := h.api.get(fmt.Sprintf("/api/sites/%d/backups", site))
	var list []r66Record
	_ = json.Unmarshal(b, &list)
	return list
}

// recordSnapshot is the snapshot of record rec of site ("" when absent).
func (h *harness) recordSnapshot(site, rec int64) string {
	for _, r := range h.siteRecords(site) {
		if r.ID == rec {
			return r.SnapshotID
		}
	}
	return ""
}
