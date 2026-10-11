//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// protectedPaths (#26): a path of a PHP site is put behind a username and
// password. nginx asks for them for the folder and everything below it,
// for a static file and for a PHP script there (the script never runs
// without them, whatever the spelling of the path), takes the right ones,
// refuses the wrong ones, and leaves the rest of the site and a longer
// name open. The password file is root's and nginx's group's only, and
// goes with the protection.
func (h *harness) protectedPaths(t *testing.T) {
	dom := h.dom("protect")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, h.PHPV))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#26: the PHP site provisions", ok, "#26: %s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	u := h.api.site(id).SystemUser
	cur := "/srv/sites/" + dom + "/current"
	h.as(u, "mkdir", "-p", cur+"/private", cur+"/privateer")
	h.mustWriteAs(t, cur+"/private/note.txt", "cg-protect-note", u)
	h.mustWriteAs(t, cur+"/private/run.php", `<?php echo 'cg-protect-ran';`, u)
	h.mustWriteAs(t, cur+"/privateer/open.txt", "cg-protect-open", u)
	h.mustWriteAs(t, cur+"/public.txt", "cg-protect-public", u)

	path := fmt.Sprintf("/api/sites/%d/protected-paths", id)
	c, b := h.api.post(path, `{"path":"/private","username":"cg-e2e","password":"cg-e2e-secret"}`)
	h.check(t, "#26: /private is protected", fmt.Sprintf("%d %t", c, strings.Contains(string(b), `"username":"cg-e2e"`) && !strings.Contains(string(b), "$2")), "200 true")

	basic := func(user, pw string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
	}
	n := 0
	get := func(p string, auth string) string {
		n++
		q := fmt.Sprintf("%s?cg26=%d-%d", p, h.pid, n)
		var r resp
		if auth == "" {
			r = h.get(dom, q)
		} else {
			r = h.get(dom, q, "Authorization", auth)
		}
		return fmt.Sprintf("%s %s", r.CodeStr(), strings.TrimSpace(string(r.Body)))
	}
	code := func(s string) string { c, _, _ := strings.Cut(s, " "); return c }
	for _, p := range []string{"/private", "/private/", "/private/note.txt", "/private/run.php", "//private/run.php", "/x/../private/run.php"} {
		got := get(p, "")
		h.check(t, "#26: without the password "+p+" answers 401", code(got), "401")
		h.assert(t, "#26: without the password "+p+" runs nothing", !strings.Contains(got, "cg-protect-ran") && !strings.Contains(got, "cg-protect-note"), "#26: %s served %q", p, got)
	}
	h.check(t, "#26: a wrong password is refused", code(get("/private/note.txt", basic("cg-e2e", "wrong"))), "401")
	h.check(t, "#26: the right password reads the file", get("/private/note.txt", basic("cg-e2e", "cg-e2e-secret")), "200 cg-protect-note")
	h.check(t, "#26: the right password runs the script", get("/private/run.php", basic("cg-e2e", "cg-e2e-secret")), "200 cg-protect-ran")
	h.check(t, "#26: the rest of the site stays open", get("/public.txt", ""), "200 cg-protect-public")
	h.check(t, "#26: a longer name is not the protected path", get("/privateer/open.txt", ""), "200 cg-protect-open")

	files, _ := filepath.Glob("/etc/nginx/cloudground-auth/" + dom + ".path-*.htpasswd")
	h.check(t, "#26: one password file for the path", fmt.Sprint(len(files)), "1")
	if len(files) == 1 {
		owner, mode := statOwnerMode(files[0])
		h.check(t, "#26: the password file is root's, 0640", owner+" "+mode, "root 640")
		h.assert(t, "#26: the password file holds a bcrypt hash, not the password",
			strings.HasPrefix(readTrim(files[0]), "cg-e2e:$2") && !strings.Contains(readTrim(files[0]), "cg-e2e-secret"), "#26: %s holds %q", files[0], readTrim(files[0]))
	}

	c, _ = h.api.do("DELETE", path, map[string]string{"path": "/private"})
	h.check(t, "#26: the protection is taken away", fmt.Sprint(c), "200")
	h.check(t, "#26: /private is open again", get("/private/note.txt", ""), "200 cg-protect-note")
	left, _ := filepath.Glob("/etc/nginx/cloudground-auth/" + dom + ".path-*.htpasswd")
	h.check(t, "#26: the password file went with the protection", fmt.Sprint(len(left)), "0")
	c, _ = h.api.post(path, `{"path":"/","username":"cg-e2e","password":"cg-e2e-secret"}`)
	h.check(t, "#26: the whole site cannot be protected this way", fmt.Sprint(c), "400")
}
