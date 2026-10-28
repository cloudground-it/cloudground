//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// restoreFromAnotherServer (#193, internal/api SPEC §5.10, ops/backup SPEC
// §4, §5, §14): a WordPress site with a marker post, a marker file and a
// second database holding a marker row is backed up into a repository of the
// section's own. The box then plays another server: its hostname changes
// (put back at the end) and the site is deleted, its backups kept. The
// repository's other sites list it, with its server and its description;
// an API token and an operator are refused, and so is a wrong domain; the
// restore creates the site again, serving the post and the file, with the
// second database's row back, wp-config.php naming the new site's database,
// and a backup of its own taken here.
func (h *harness) restoreFromAnotherServer(t *testing.T) {
	base := fmt.Sprintf("/var/backups/cloudground/e2e-foreign-%d", h.pid)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	pw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(raw))
	_, sb := h.api.get("/api/settings")
	prevRepo := jsonField(sb, "backup_repository")
	defer h.undo("put the backup repository setting back (#193)", func() {
		h.api.relogin()
		if prevRepo == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prevRepo)
		}
		_ = os.RemoveAll(base)
	})()
	setBody, _ := json.Marshal(map[string]string{"backup_repository": base, "backup_password": pw})
	if c := code(h.api.put("/api/settings", setBody)); c != "200" {
		h.fail(t, "#193 control: the section's repository is accepted", "HTTP %s", c)
		return
	}

	dom := h.dom("bforeign")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"wordpress","title":"Foreign","admin_email":"f@%s","admin_user":"fadmin","admin_password":"foreign-admin-pw-%d"}`, dom, dom, h.pid))
	if h.api.waitActive(id, 8*time.Minute) != "active" {
		h.fail(t, "#193 control: the section's WordPress site provisions", "site %d", id)
		return
	}
	deleted := false
	defer func() {
		if !deleted {
			h.api.relogin()
			h.api.deleteSite(id)
			h.api.teardownGone(id)
		}
	}()
	site := h.api.site(id)
	cur := "/srv/sites/" + dom + "/current"
	marker := fmt.Sprintf("cg-foreign-%d", time.Now().UnixNano())
	post := h.wp(site.SystemUser, cur, "post", "create", "--post_title="+marker, "--post_status=publish", "--porcelain")
	h.assert(t, "#193 control: a marker post is written", post.OK() && post.Trim() != "", "%s", post)
	h.mustWriteAs(t, cur+"/cg-foreign.txt", marker+"\n", site.SystemUser)
	extra := fmt.Sprintf("cgf%d_extra", h.pid%100000)
	_, db := h.api.post(fmt.Sprintf("/api/sites/%d/databases", id), map[string]string{"name": extra})
	h.check(t, "#193 control: a second database is made", h.api.waitTask(taskIDOf(db), 5*time.Minute), "succeeded")
	r := h.run("mariadb", extra, "-e", "CREATE TABLE cg_marker (v VARCHAR(64)); INSERT INTO cg_marker VALUES ('"+marker+"');")
	h.assert(t, "#193 control: the second database holds a marker row", r.OK(), "%s", r)
	_, bb := h.api.post(fmt.Sprintf("/api/sites/%d/backups", id), map[string]any{})
	h.check(t, "#193 control: the site's backup passes its restore test", h.api.waitTask(taskIDOf(bb), 15*time.Minute), "succeeded")

	foreign := func() (list []struct {
		Domain   string `json:"domain"`
		Hostname string `json:"hostname"`
		Snapshot string `json:"snapshot"`
		Manifest bool   `json:"manifest"`
	}, status int) {
		c, b := h.api.get("/api/backups/foreign")
		var out struct {
			Sites []struct {
				Domain   string `json:"domain"`
				Hostname string `json:"hostname"`
				Snapshot string `json:"snapshot"`
				Manifest bool   `json:"manifest"`
			} `json:"sites"`
		}
		_ = json.Unmarshal(b, &out)
		return out.Sites, c
	}
	listed := func() (string, string, bool) {
		list, _ := foreign()
		for _, s := range list {
			if s.Domain == dom {
				return s.Snapshot, s.Hostname, s.Manifest
			}
		}
		return "", "", false
	}
	_, status := foreign()
	snapHere, _, _ := listed()
	h.assert(t, "#193: a site this panel has is not among the other servers' sites", status == 200 && snapHere == "", "HTTP %d, listed %q", status, snapHere)

	// An operator, assigned to the site while it is here.
	ope, opw := fmt.Sprintf("e2e-foreign-op-%d@example.com", h.pid), fmt.Sprintf("e2e-foreign-operator-%d", h.pid)
	_, ub := h.api.post("/api/users", fmt.Sprintf(`{"email":%q,"password":%q,"role":"operator","site_ids":[%d]}`, ope, opw, id))
	opid := jsonField(ub, "id")
	h.assert(t, "#193 control: an operator account is made", opid != "", "%s", head(ub, 200))
	if opid != "" {
		defer h.undo("delete the #193 operator", func() { h.api.relogin(); h.api.del("/api/users/" + opid) })()
	}

	// Another server: the box's name changes, and the site is gone. The
	// agent sees its hostname as it was when it started (its unit has
	// ProtectHostname=yes): it is restarted, as a new server's would start.
	origHost, _ := os.Hostname()
	other := fmt.Sprintf("cg-e2e-other-%d", h.pid)
	r = h.run("hostnamectl", "set-hostname", other)
	defer h.undo("put the hostname back (#193)", func() {
		h.run("hostnamectl", "set-hostname", origHost)
		h.run("systemctl", "restart", "cloudground-agent")
		h.runningVersion()
		h.api.relogin()
	})()
	h.run("systemctl", "restart", "cloudground-agent")
	h.runningVersion()
	h.api.relogin()
	if now, _ := os.Hostname(); !h.okHost(t, r, now, other) {
		return
	}
	h.check(t, "#193 control: the site is deleted, its backups kept", h.api.deleteSite(id), "202")
	h.check(t, "#193 control: the site is gone", h.api.teardownGone(id), "yes")
	deleted = true

	snap, host, manifest := listed()
	h.assert(t, "#193: the deleted site is listed with the server that backed it up and its description",
		snap != "" && host == origHost && manifest, "snapshot %q, host %q (want %q), manifest %v", snap, host, origHost, manifest)
	if snap == "" {
		return
	}

	// Only an administrator's session; the domain typed.
	c, tb := h.api.post("/api/tokens", map[string]any{"name": fmt.Sprintf("e2e-foreign-%d", h.pid), "expires_in_days": 1})
	var tok struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(tb, &tok)
	if c == 201 && tok.Token != "" {
		bearer := "Authorization: Bearer " + tok.Token
		c, _ = h.api.do("GET", "/api/backups/foreign", nil, bearer)
		h.check(t, "#193: an API token cannot list the other servers' sites", fmt.Sprint(c), "403")
		c, _ = h.api.do("POST", "/api/backups/foreign/restore", map[string]string{"snapshot": snap, "confirm": dom}, bearer)
		h.check(t, "#193: an API token cannot restore from another server", fmt.Sprint(c), "403")
		h.api.del(fmt.Sprintf("/api/tokens/%d", tok.APIToken.ID))
	} else {
		h.fail(t, "#193 control: an API token is made", "HTTP %d %s", c, head(tb, 200))
	}
	if opid != "" {
		op := newAPIClient(h, ope, opw)
		op.login()
		h.check(t, "#193: an operator cannot list the other servers' sites", code(op.get("/api/backups/foreign")), "403")
		h.check(t, "#193: an operator cannot restore from another server",
			code(op.post("/api/backups/foreign/restore", map[string]string{"snapshot": snap, "confirm": dom})), "403")
	}
	c, wb := h.api.post("/api/backups/foreign/restore", map[string]string{"snapshot": snap, "confirm": "not-" + dom})
	h.assert(t, "#193: a wrong domain is refused", c == 400 && strings.Contains(string(wb), "domain"), "HTTP %d %s", c, head(wb, 200))

	// The restore.
	c, rb := h.api.post("/api/backups/foreign/restore", map[string]string{"snapshot": snap, "confirm": dom})
	h.check(t, "#193: the restore from another server is accepted", fmt.Sprint(c), "202")
	var made struct {
		Site struct {
			ID int64 `json:"id"`
		} `json:"site"`
	}
	_ = json.Unmarshal(rb, &made)
	if made.Site.ID != 0 {
		defer func() { h.api.relogin(); h.api.deleteSite(made.Site.ID); h.api.teardownGone(made.Site.ID) }()
	}
	st := h.api.waitTask(taskIDOf(rb), 25*time.Minute)
	if st != "succeeded" {
		_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", taskIDOf(rb)))
		t.Logf("backup.import: %s", head(tb, 800))
	}
	h.check(t, "#193: the site is created and restored", st, "succeeded")
	if st != "succeeded" {
		return
	}
	ns := h.api.site(made.Site.ID)
	h.check(t, "#193: the restored site is active", ns.Status, "active")
	// WordPress answers ?p=<id> with a redirect to the post's own address.
	pr := h.get(dom, "/?p="+post.Trim())
	if loc := pr.Header.Get("Location"); pr.Code/100 == 3 && loc != "" {
		if u, err := url.Parse(loc); err == nil {
			pr = h.get(dom, u.RequestURI())
		}
	}
	body := string(pr.Body)
	h.assert(t, "#193: the restored site serves the marker post", pr.Code == 200 && strings.Contains(body, marker), "HTTP %d, body %q", pr.Code, clip(body))
	h.check(t, "#193: the restored site serves the marker file", strings.TrimSpace(string(h.get(dom, "/cg-foreign.txt").Body)), marker)
	h.check(t, "#193: the second database's row is back", strings.TrimSpace(h.run("mariadb", "-N", extra, "-e", "SELECT v FROM cg_marker").Out), marker)
	ncur := "/srv/sites/" + dom + "/current"
	h.check(t, "#193: wp-config.php names the new site's database", h.wp(ns.SystemUser, ncur, "config", "get", "DB_NAME").Trim(), ns.Config.Database.Name)
	h.assert(t, "#193: the restored site has a backup of its own here", h.newestBackup(made.Site.ID, "full") != 0, "no passed backup of site %d", made.Site.ID)
	// The exception was needed: the box is another server now, so the
	// backup the site came from is refused anywhere but that restore.
	rec := h.newestBackup(made.Site.ID, "full")
	if rec != 0 {
		_, list := h.api.get(fmt.Sprintf("/api/sites/%d/backups", made.Site.ID))
		h.assert(t, "#193 control: the site's own backup here is a new snapshot", !strings.Contains(string(list), snap), "the record names the snapshot the site came from")
	}
	_, audit := h.api.get("/api/audit")
	h.assert(t, "#193: the restore is audited", contains(audit, "backup.import") && contains(audit, "backup.imported"), "no backup.import(ed) in the audit log")
}

// okHost checks the hostname changed for the section.
func (h *harness) okHost(t *testing.T, r result, now, want string) bool {
	h.assert(t, "#193 control: the box plays another server (hostname "+want+")", r.OK() && now == want, "%s; hostname now %q", r, now)
	return r.OK() && now == want
}
