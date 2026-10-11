//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"
)

type auditEvent struct {
	ID     int64  `json:"id"`
	Action string `json:"action"`
}

func isReconcile(a string) bool { return a == "sites.resized" || a == "upgrade.reconciled" }

func (h *harness) audit() []auditEvent {
	var ev []auditEvent
	_, b := h.api.get("/api/audit")
	_ = json.Unmarshal(b, &ev)
	return ev
}

// reconcileMark is the id of the newest reconcile event (0 if none).
func (h *harness) reconcileMark() int64 {
	var m int64
	for _, e := range h.audit() {
		if isReconcile(e.Action) && e.ID > m {
			m = e.ID
		}
	}
	return m
}

// firstReconcileAfter is the action of the oldest reconcile event newer
// than mark ("" if none).
func (h *harness) firstReconcileAfter(mark int64) string {
	var newer []auditEvent
	for _, e := range h.audit() {
		if e.ID > mark && isReconcile(e.Action) {
			newer = append(newer, e)
		}
	}
	if len(newer) == 0 {
		return ""
	}
	sort.Slice(newer, func(a, b int) bool { return newer[a].ID < newer[b].ID })
	return newer[0].Action
}

// maintenanceReconcile runs one scheduler tick now (POST
// /api/system/maintenance, which answers once the tick is done) and returns
// the action of the first reconcile event newer than mark: "none" when the
// tick ran no reconcile, or what the call answered when it failed. The
// scheduler's own tick may have got there first; its event counts the same.
// A build older than the route (when a new check is proved on the old
// build) answers with the UI's page instead of {"ran":true}: it gets the
// restart instead.
func (h *harness) maintenanceReconcile(mark int64) string {
	c, b := h.api.runMaintenance()
	if c == http.StatusOK && jsonBool(b, "ran") != "true" {
		h.root.Logf("POST /api/system/maintenance answered no run (a build without the route): restarting the API instead")
		return h.restartReconcile(mark)
	}
	if c != http.StatusOK {
		return fmt.Sprintf("maintenance answered %d %s", c, head(b, 200))
	}
	if a := h.firstReconcileAfter(mark); a != "" {
		return a
	}
	return "none"
}

// restartReconcile restarts the API (its scheduler ticks 30 s after start) and
// returns the action of the first reconcile event newer than mark, or
// "timeout" after 300 s.
func (h *harness) restartReconcile(mark int64) (action string) {
	found := false
	h.run("systemctl", "restart", "cloudground-api.service")
	started := time.Now().Unix()
	// The tick runs 30 s after the start and re-applies every PHP site; one
	// that is busy (a slow certificate issuance holds its lock) delays the
	// event, so allow 5 minutes and leave the API's own account on timeout.
	defer func() {
		if !found {
			h.root.Logf("API journal since the restart:\n%s", h.run("journalctl", "-u", "cloudground-api", "--since", "@"+strconv.FormatInt(started, 10), "-o", "cat", "--no-pager").Out)
		}
	}()
	for end := time.Now().Add(300 * time.Second); time.Now().Before(end); {
		time.Sleep(2 * time.Second)
		if c, _ := h.api.get("/api/sites"); c == http.StatusUnauthorized {
			h.api.login()
		}
		if a := h.firstReconcileAfter(mark); a != "" {
			found = true
			return a
		}
	}
	return "timeout"
}

// r07 is the snapshot r07_snapshot took: every active PHP site's pool hash
// and master PID, and the static probe's vhost inode.
type r07 struct {
	users []string
	pool  map[string]string
	pid   map[string]string
	vh    string
}

func (h *harness) mainPID(unit string) string {
	return h.run("systemctl", "show", "-p", "MainPID", "--value", unit).Trim()
}

func (h *harness) r07Snapshot(probe string) r07 {
	s := r07{pool: map[string]string{}, pid: map[string]string{}}
	all, _ := h.api.sites()
	for _, site := range all {
		if site.Status == "active" && site.Type != "static" && site.Type != "proxy" {
			s.users = append(s.users, site.SystemUser)
		}
	}
	for _, u := range s.users {
		s.pool[u] = sha256File("/etc/cloudground-php/" + u + ".conf")
		s.pid[u] = h.mainPID("cloudground-php-" + u + ".service")
	}
	s.vh = inode("/etc/nginx/sites-enabled/" + probe + ".conf")
	return s
}

var (
	nginxReloadRe = regexp.MustCompile(`^Reload`)
	unitEventRe   = regexp.MustCompile(`^(Reloading|Reloaded|Stopping|Stopped|Starting|Started) `)
)

// r07Assert checks the reconcile that followed <what>, since epoch since.
func (h *harness) r07Assert(t *testing.T, what, probe string, snap r07, since int64) {
	vhNow := inode("/etc/nginx/sites-enabled/" + probe + ".conf")
	h.assert(t, fmt.Sprintf("R-0.7 (%s): the static site was not re-rendered by the site-count reconcile", what), vhNow == snap.vh,
		"R-0.7 (%s): the site-count reconcile rewrote the static site's vhost (inode %s -> %s)", what, snap.vh, vhNow)
	at := "@" + strconv.FormatInt(since, 10)
	nrel := CountLines(h.run("journalctl", "-u", "nginx.service", "--since", at, "-o", "cat").Out, nginxReloadRe)
	h.check(t, fmt.Sprintf("R-0.7 (%s): nginx reloads during the site-count reconcile", what), strconv.Itoa(nrel), "0")
	for _, u := range snap.users {
		unit := "cloudground-php-" + u + ".service"
		if !h.run("systemctl", "cat", unit).OK() {
			continue // deleted with its site
		}
		pool := sha256File("/etc/cloudground-php/" + u + ".conf")
		ev := CountLines(h.run("journalctl", "-u", unit, "--since", at, "-o", "cat").Out, unitEventRe)
		pid := h.mainPID(unit)
		if pool == snap.pool[u] {
			// Its budget came out the same: nothing may have touched the master.
			h.check(t, fmt.Sprintf("R-0.7 (%s): %s's pool is unchanged; systemd reload/restart events on its master", what, u), strconv.Itoa(ev), "0")
			h.check(t, fmt.Sprintf("R-0.7 (%s): %s's pool is unchanged; its master's MainPID", what, u), pid, snap.pid[u])
		} else {
			// Its budget changed: the master must have been reloaded to apply it.
			h.assert(t, fmt.Sprintf("R-0.7 (%s): %s's pool budget changed and its master was reloaded to apply it", what, u), ev > 0,
				"R-0.7 (%s): %s's pool changed on disk but its master was never reloaded", what, u)
		}
	}
}

// reconcile (R-0.7): a change in the number of sites re-renders only what
// it sizes. A static probe site must not be touched at all, nginx must not
// reload, and a PHP master whose pool comes out byte-identical keeps running.
// The first reconcile is forced by restarting the API (its tick runs 30 s
// after it starts), which also checks that a restart still reconciles; the
// two under test run through the admin's run-now (POST
// /api/system/maintenance), which runs the same tick and answers when it is
// done, instead of a restart and a wait for the tick each.
func (h *harness) reconcile(t *testing.T) {
	r7s, r7x := h.dom("r07s"), h.dom("r07x")
	mark := h.reconcileMark()
	sid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, r7s))
	if sid != 0 {
		defer func() { h.api.deleteSite(sid); h.api.teardownGone(sid) }()
	}
	if sid == 0 || h.api.waitActive(sid, 120*time.Second) != "active" {
		h.fail(t, "R-0.7: the static probe site did not provision", "R-0.7: the static probe site did not provision")
		return
	}
	// Settle the count change the probe itself made, so the next reconcile
	// is caused by the PHP site alone.
	r := h.restartReconcile(mark)
	h.assert(t, "R-0.7: restarting the API runs a reconcile", r != "timeout", "R-0.7: no reconcile event within 300 s of restarting the API (the API journal is in the log)")
	t.Logf("first reconcile: %s", r)
	mark = h.reconcileMark()
	snap := h.r07Snapshot(r7s)
	xid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, r7x, h.PHPV))
	got := ""
	if xid != 0 {
		got = h.api.waitActive(xid, 120*time.Second)
	}
	h.check(t, "R-0.7: a PHP site is added", got, "active")
	// The window opens after the new site's own provisioning reloads.
	since := time.Now().Unix() + 1
	time.Sleep(2 * time.Second)
	h.check(t, "R-0.7 (create): adding a site runs the site-count reconcile, not a full one", h.maintenanceReconcile(mark), "sites.resized")
	h.r07Assert(t, "create", r7s, snap, since)

	mark = h.reconcileMark()
	snap = h.r07Snapshot(r7s)
	if xid != 0 {
		h.check(t, "R-0.7: the added PHP site is deleted", h.api.deleteSite(xid), "202")
		h.check(t, "R-0.7: its teardown completes", h.api.teardownGone(xid), "yes")
	}
	since = time.Now().Unix() + 1
	time.Sleep(2 * time.Second)
	h.check(t, "R-0.7 (delete): deleting a site runs the site-count reconcile, not a full one", h.maintenanceReconcile(mark), "sites.resized")
	h.r07Assert(t, "delete", r7s, snap, since)
}
