//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// phpExtensions (#29): the PHP page lists what a version's FPM binary
// loads, and an administrator installs a common extension the version
// lacks: the task succeeds, the list shows it, and the PHP site's own
// master, restarted by the install, loads it (asked through a page the site
// serves, not through the API's list). Anything off the list is refused.
// The backup policy keeps the hour it is given. The package is removed at
// the end, so the check runs again on the next build.
func (h *harness) phpExtensions(t *testing.T) {
	dom := h.dom("php-ext")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#29: the PHP site provisions", ok, "#29: %s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	u := h.api.site(id).SystemUser
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/cg-ext.php", `<?php echo 'cg-ext ', extension_loaded('gmp') ? 'gmp' : 'none';`, u)
	served := func(n int) string {
		r := h.get(dom, fmt.Sprintf("/cg-ext.php?n=%d-%d", h.pid, n))
		return fmt.Sprintf("%s %s", r.CodeStr(), strings.TrimSpace(string(r.Body)))
	}

	type extList struct {
		Version    string   `json:"version"`
		Modules    []string `json:"modules"`
		Extensions []struct {
			Name      string `json:"name"`
			Installed bool   `json:"installed"`
		} `json:"extensions"`
	}
	list := func() (int, extList, map[string]bool) {
		c, b := h.api.get("/api/php/" + h.PHPV + "/extensions")
		var l extList
		_ = json.Unmarshal(b, &l)
		installed := map[string]bool{}
		for _, e := range l.Extensions {
			installed[e.Name] = e.Installed
		}
		return c, l, installed
	}
	c, l, installed := list()
	h.check(t, "#29: the extensions of the site's PHP version are listed",
		fmt.Sprint(c, l.Version == h.PHPV, slices.Contains(l.Modules, "curl"), installed["curl"], installed["mysql"]), "200 true true true true")
	c, _ = h.api.post("/api/php/"+h.PHPV+"/extensions", `{"extension":"xdebug"}`)
	h.check(t, "#29: an extension off the list is refused", fmt.Sprint(c), "400")

	if installed["gmp"] {
		t.Logf("(gmp is already installed for PHP %s: the install is not checked)", h.PHPV)
	} else {
		pkg := "php" + h.PHPV + "-gmp"
		cleanup := h.undo("remove "+pkg, func() { h.run("apt-get", "purge", "-y", "-qq", pkg) })
		defer cleanup()
		h.check(t, "#29 control: the site's PHP lacks gmp before the install", served(1), "200 cg-ext none")
		c, b := h.api.post("/api/php/"+h.PHPV+"/extensions", `{"extension":"gmp"}`)
		var ans struct {
			Task struct {
				ID   int64  `json:"id"`
				Kind string `json:"kind"`
			} `json:"task"`
		}
		_ = json.Unmarshal(b, &ans)
		h.check(t, "#29: the install is a php.extension task", fmt.Sprint(c, " ", ans.Task.Kind), "202 php.extension")
		h.check(t, "#29: the install succeeds", h.api.waitTask(ans.Task.ID, 10*time.Minute), "succeeded")
		_, _, installed = list()
		h.assert(t, "#29: the list shows gmp installed", installed["gmp"], "#29: gmp is not installed after the task")
		h.check(t, "#29: the site's PHP master loads gmp after the install", served(2), "200 cg-ext gmp")
	}

	path := fmt.Sprintf("/api/sites/%d/backups/policy", id)
	c, b := h.api.put(path, `{"enabled":true,"schedule":"weekly","hour":3}`)
	var p struct {
		Schedule string `json:"schedule"`
		Hour     *int   `json:"hour"`
	}
	_ = json.Unmarshal(b, &p)
	h.check(t, "#29: a weekly backup at 03:00 is stored", fmt.Sprintf("%d %s %t", c, p.Schedule, p.Hour != nil && *p.Hour == 3), "200 weekly true")
	c, _ = h.api.put(path, `{"hour":24}`)
	h.check(t, "#29: an hour outside 0-23 is refused", fmt.Sprint(c), "400")
}
