//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// siteLifecycle (R-3.7): every process of a site runs in its slice
// (cg-site-<id>.slice); creations and deletions running at the same time
// leave nginx with exactly the live sites; a deleted site leaves no user,
// no process, no unit and no tree, even with a process of its user running
// outside the slice.
func (h *harness) siteLifecycle(t *testing.T) {
	type made struct {
		id        int64
		dom, user string
	}
	create := func(label string) made {
		dom := h.dom(label)
		id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
		return made{id: id, dom: dom}
	}
	settle := func(m *made) bool {
		ok := h.api.waitActive(m.id, 4*time.Minute) == "active"
		h.assert(t, "R-3.7: "+m.dom+" provisions", ok, "R-3.7: %s (id %d) did not become active", m.dom, m.id)
		m.user = h.api.site(m.id).SystemUser
		return ok && m.user != ""
	}

	first := []made{create("r37a"), create("r37b"), create("r37c")}
	for i := range first {
		if !settle(&first[i]) {
			return
		}
	}
	a := first[0]

	// The PHP master is in the site's slice. Before R-3.7 it ran in
	// system.slice, and nothing tied a site's processes together.
	h.check(t, "R-3.7: a site's PHP master runs in the site's slice",
		h.run("systemctl", "show", "-p", "Slice", "--value", "cloudground-php-"+a.user+".service").Trim(), a.user+".slice")

	// A cron job run now is in it too.
	_, site := h.api.get(fmt.Sprintf("/api/sites/%d", a.id))
	body := fmt.Sprintf(`{"site":%s,"job":{"id":%d,"schedule":"* * * * *","command":"cat /proc/self/cgroup","enabled":true},"max_seconds":30}`,
		site, 997000+h.pid%1000)
	code, b, err := h.agentCall("RunCron", body)
	h.assert(t, "R-3.7: a cron job run now is in the site's slice",
		err == nil && code == 200 && strings.Contains(jsonField(b, "output"), "/cg-site.slice/"+a.user+".slice/"),
		"R-3.7: RunCron = %d %s (%v)", code, head(b, 300), err)

	// A process of the user outside the slice, as a job crond started or
	// an SFTP session would be: deletion must still end it and remove the
	// user.
	marker := fmt.Sprintf("297.%d", h.pid)
	stray := exec.Command("runuser", "-u", a.user, "--", "sleep", marker)
	if err := stray.Start(); err != nil {
		h.fail(t, "R-3.7: start a process of the site user", "R-3.7: %v", err)
	} else {
		go stray.Wait()
		h.undo("kill the stray process", func() { h.run("pkill", "-f", "sleep "+marker) })
	}
	uids := map[string]string{}
	for _, m := range first[:2] {
		uids[m.user] = h.run("id", "-u", m.user).Trim()
	}

	// The storm: two deletions and two creations at once.
	gone := first[:2]
	var fresh []made
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range gone {
		wg.Add(1)
		go func(m made) { defer wg.Done(); h.api.deleteSite(m.id) }(m)
	}
	for _, label := range []string{"r37d", "r37e"} {
		wg.Add(1)
		go func(label string) {
			defer wg.Done()
			m := create(label)
			mu.Lock()
			fresh = append(fresh, m)
			mu.Unlock()
		}(label)
	}
	wg.Wait()
	for _, m := range gone {
		h.check(t, "R-3.7: "+m.dom+" is deleted during the storm", h.api.teardownGone(m.id), "yes")
	}
	live := []made{first[2]}
	for i := range fresh {
		if settle(&fresh[i]) {
			live = append(live, fresh[i])
		}
	}

	// nginx: exactly the live sites of this section, and each one serves.
	conf := h.run("nginx", "-T").Out
	for _, m := range gone {
		h.check(t, "R-3.7: nginx -T has nothing of deleted "+m.dom, strconv.Itoa(strings.Count(conf, m.dom)), "0")
	}
	var want, got []string
	for _, m := range live {
		want = append(want, m.dom+".conf")
		h.assert(t, "R-3.7: nginx -T has live "+m.dom, strings.Contains(conf, "server_name "+m.dom), "R-3.7: %s is missing from nginx -T", m.dom)
		c, fatal := h.serves(m.dom, "/")
		h.check(t, "R-3.7: live "+m.dom+" serves 200 after the storm", c+" fatal="+fatal, "200 fatal=no")
	}
	ents, _ := os.ReadDir("/etc/nginx/sites-enabled")
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "r37") {
			got = append(got, e.Name())
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	h.check(t, "R-3.7: the vhosts of this section are exactly the live sites", strings.Join(got, " "), strings.Join(want, " "))

	// Nothing of a deleted site is left.
	for _, m := range gone {
		h.assert(t, "R-3.7: "+m.user+" is gone from the user database", !h.run("getent", "passwd", m.user).OK(),
			"R-3.7: getent passwd %s still answers", m.user)
		h.assert(t, "R-3.14: the group "+m.user+" is gone with its site", !h.run("getent", "group", m.user).OK(),
			"R-3.14: getent group %s still answers", m.user)
		h.check(t, "R-3.7: no process of deleted "+m.user+" is left", strconv.Itoa(procsOfUID(uids[m.user])), "0")
		h.check(t, "R-3.7: the slice of deleted "+m.user+" is stopped",
			h.run("systemctl", "show", "-p", "ActiveState", "--value", m.user+".slice").Trim(), "inactive")
		for _, p := range []string{"/etc/systemd/system/cloudground-php-" + m.user + ".service",
			"/etc/systemd/system/cloudground-php-" + m.user + ".service.d", "/etc/cloudground-php/" + m.user + ".conf",
			"/srv/sites/" + m.dom, "/var/log/cloudground/" + m.dom} {
			h.assert(t, "R-3.7: "+p+" is removed with its site", !exists(p), "R-3.7: %s outlived its site", p)
		}
	}
	h.check(t, "R-3.7: the user's process outside the slice was ended", strconv.Itoa(procsMatching("sleep "+marker)), "0")

	for _, m := range live {
		h.api.deleteSite(m.id)
		h.check(t, "R-3.7: "+m.dom+" teardown completes", h.api.teardownGone(m.id), "yes")
	}
}

// procsOfUID counts the processes with uid as their real uid ("" counts
// none).
func procsOfUID(uid string) int {
	if uid == "" {
		return 0
	}
	ents, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range ents {
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(strings.TrimPrefix(line, "Uid:")); strings.HasPrefix(line, "Uid:") && len(f) > 0 && f[0] == uid {
				n++
			}
		}
	}
	return n
}
