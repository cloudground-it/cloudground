//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// siteSettings (R-4.3): one PATCH /api/sites/{id} changes settings that
// used to need three routes (PHP limits, cache, aliases, public root), one
// job applies them, and nginx and PHP-FPM serve the result: the alias
// answers, from the new public root, with the new limits. A refused patch
// names each wrong field and changes nothing. The routes PATCH replaced
// are gone (R-8.2): PUT /php answers 404 and changes nothing.
func (h *harness) siteSettings(t *testing.T) {
	dom, alias := h.dom("r43"), h.dom("r43alias")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "R-4.3: the settings site provisions", ok, "R-4.3: %s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	u := h.api.site(id).SystemUser
	path := fmt.Sprintf("/api/sites/%d", id)
	probe := `<?php echo 'cg-r43 ', ini_get('memory_limit'), ' ', ini_get('max_execution_time');`
	// The new public root, made by the site's own user before it is used.
	h.as(u, "mkdir", "-p", "/srv/sites/"+dom+"/current/web")
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/web/cg-r43.php", probe, u)

	// Another tenant, with a file of its own the first site must never get
	// nginx to serve (nginx is in every site's group).
	other := h.dom("r43other")
	oid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, other, h.PHPV))
	oActive := h.api.waitActive(oid, 4*time.Minute) == "active"
	h.assert(t, "R-4.3 control: the other tenant provisions", oActive, "R-4.3: %s did not become active", other)
	ou := h.api.site(oid).SystemUser
	secret := fmt.Sprintf("cg-r43-secret-%d", h.pid)
	otherDir := "/srv/sites/" + other + "/current/pub"
	h.as(ou, "mkdir", "-p", otherDir)
	h.mustWriteAs(t, otherDir+"/cg-r43-secret.txt", secret, ou)
	defer func() {
		h.api.deleteSite(oid)
		h.api.teardownGone(oid)
	}()

	// R-4.3 review: a public root through a link the site planted is
	// refused at the PATCH (the agent Lstats each component).
	h.as(u, "ln", "-s", otherDir, "/srv/sites/"+dom+"/current/lnk")
	lc, lb := h.api.do("PATCH", path, map[string]any{"public_root": "lnk"})
	h.check(t, "R-4.3: a public root through a symlink is refused at the PATCH",
		fmt.Sprintf("%d %t", lc, strings.Contains(string(lb), "symbolic link")), "400 true")

	// A certificate the operator brought: the panel cannot reissue it for
	// new aliases, and the patch says so. (The suite issues no Let's
	// Encrypt certificates, so the reissue path is the unit tests'.)
	cert, err := selfSigned("cg-e2e-r43", dom)
	if err != nil {
		t.Logf("fixture: certificate: %v", err)
	}
	h.check(t, "R-4.3 control: a custom certificate is installed", code(h.api.put(path+"/certificate/custom", cert)), "200")

	code, b := h.api.do("PATCH", path, map[string]any{
		"aliases": []string{alias}, "memory_limit_mb": 384, "max_execution_seconds": 200,
		"cache_ttl_sec": 600, "public_root": "web",
	})
	var ans struct {
		Task struct {
			ID   int64  `json:"id"`
			Kind string `json:"kind"`
		} `json:"task"`
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(b, &ans)
	h.check(t, "R-4.3: one PATCH is accepted with one site.update task", fmt.Sprintf("%d %s", code, ans.Task.Kind), "202 site.update")
	h.assert(t, "R-4.3: new aliases on a custom certificate answer a warning",
		len(ans.Warnings) == 1 && strings.Contains(ans.Warnings[0], "custom certificate"),
		"R-4.3: warnings = %q", ans.Warnings)
	h.check(t, "R-4.3: the patch's task succeeds", h.api.waitTask(ans.Task.ID, 2*time.Minute), "succeeded")

	conf := h.run("nginx", "-T").Out
	h.assert(t, "R-4.3: nginx answers for the patched alias", strings.Contains(conf, "server_name "+dom+" "+alias),
		"R-4.3: no 'server_name %s %s' in nginx -T", dom, alias)
	// Each read has a query of its own: the page cache keeps answers.
	fetch := func(n int) string {
		r := h.get(alias, fmt.Sprintf("/cg-r43.php?r43=%d-%d", h.pid, n))
		return fmt.Sprintf("%s %s", r.CodeStr(), strings.TrimSpace(string(r.Body)))
	}
	h.check(t, "R-4.3: the alias serves the new public root with the patched PHP limits",
		fetch(1), "200 cg-r43 384M 200")
	pool := readTrim("/etc/cloudground-php/" + u + ".conf")
	h.assert(t, "R-4.3: the site's pool has the patched memory limit", strings.Contains(pool, "php_admin_value[memory_limit] = 384M"),
		"R-4.3: the pool file of %s does not say memory_limit 384M", u)

	// R-4.3 review: the site user swaps its valid public root for a link
	// to the other tenant after the PATCH. nginx checks every component
	// below "current" (disable_symlinks from=<root>/current): the link and
	// its target have different owners, so the other site's file is never
	// served.
	own := h.get(other, "/pub/cg-r43-secret.txt")
	h.check(t, "R-4.3 control: nginx serves the other tenant's file on its own site",
		fmt.Sprintf("%s %t", own.CodeStr(), strings.Contains(string(own.Body), secret)), "200 true")
	web := "/srv/sites/" + dom + "/current/web"
	h.as(u, "mv", web, web+".real")
	h.as(u, "ln", "-s", otherDir, web)
	sw := h.get(alias, "/cg-r43-secret.txt")
	h.assert(t, "R-4.3: a public root swapped for a link to another tenant does not serve its files",
		sw.Code != 200 && !strings.Contains(string(sw.Body), secret),
		"R-4.3: %s served the other tenant's file through the swapped public root (HTTP %s)", alias, sw.CodeStr())
	h.as(u, "rm", web)
	h.as(u, "mv", web+".real", web)

	// Refused field by field; nothing changes.
	code, b = h.api.do("PATCH", path, map[string]any{"memory_limit_mb": 1, "upload_max_mb": 0, "profile": "turbo"})
	var bad struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}
	_ = json.Unmarshal(b, &bad)
	h.check(t, "R-4.3: a wrong patch is refused naming each field",
		fmt.Sprintf("%d %d %t", code, len(bad.Fields), bad.Fields["memory_limit_mb"] != "" && bad.Fields["upload_max_mb"] != "" && bad.Fields["profile"] != ""),
		"400 3 true")
	h.check(t, "R-4.3: the refused patch changed nothing", fetch(2), "200 cg-r43 384M 200")

	// R-8.2: the routes PATCH replaced are gone. A script still calling
	// one is told so (it used to get the app's page), and nothing changes.
	code, b = h.api.put(path+"/php", `{"memory_limit_mb":320}`)
	h.check(t, "R-8.2: the removed PUT /php answers 404 from the API",
		fmt.Sprintf("%d %t", code, strings.Contains(string(b), "no such API route")), "404 true")
	code, b = h.api.post(path+"/wp/object-cache", `{"enable":true}`)
	h.check(t, "R-8.2: the removed POST /wp/object-cache answers 404 from the API",
		fmt.Sprintf("%d %t", code, strings.Contains(string(b), "no such API route")), "404 true")
	h.check(t, "R-8.2: PHP keeps the patched limit after the removed route", fetch(3), "200 cg-r43 384M 200")

	// R-8.2: the backup policy is stored as sent, renders nothing, and a
	// schedule the sweep does not know is refused.
	tasksOf := func() int {
		_, tb := h.api.get("/api/tasks")
		return strings.Count(string(tb), fmt.Sprintf(`"site_id":%d,`, id))
	}
	tasksBefore := tasksOf()
	code, b = h.api.put(path+"/backups/policy", `{"enabled":true,"schedule":"weekly","retention_days":14}`)
	h.check(t, "R-8.2: the backup policy is accepted", fmt.Sprint(code), "200")
	after := h.api.site(id)
	h.check(t, "R-8.2: the backup policy is stored",
		fmt.Sprintf("%t %s %d", after.Config.Backups.Enabled, after.Config.Backups.Schedule, after.Config.Backups.RetentionDays), "true weekly 14")
	// "Renders nothing": no job is queued for the site and nothing is left
	// pending. The store moves desired and applied together for a change
	// that needs no render (MutateSiteApplied), so the generations' values
	// themselves may advance.
	h.check(t, "R-8.2: the backup policy renders nothing",
		fmt.Sprintf("%t %d", after.DesiredGeneration == after.AppliedGeneration, tasksOf()-tasksBefore), "true 0")
	code, b = h.api.put(path+"/backups/policy", `{"schedule":"monthly"}`)
	h.check(t, "R-8.2: an unknown backup schedule is refused", fmt.Sprintf("%d %t", code, strings.Contains(string(b), "hourly, daily or weekly")), "400 true")

	h.check(t, "R-4.3: the settings site deletes", h.api.deleteSite(id), "202")
	h.check(t, "R-4.3: the settings site teardown completes", h.api.teardownGone(id), "yes")
}
