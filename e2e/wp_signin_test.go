//go:build e2e

package e2e

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// wpTwoFactor (#74): with two-factor sign-in required on the WooCommerce
// site, its administrator's password alone opens no session (wp-login.php
// sends it to the second step, without an auth cookie, and wp-admin still
// asks to sign in), XML-RPC refuses the password, the right app code then
// signs in, and the panel's magic login still does. The mode reaches PHP
// through the pool, and turned off, the password alone works again.
func (h *harness) wpTwoFactor(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	path := fmt.Sprintf("/api/sites/%d/wp/two-factor", w.id)
	pool := "/etc/cloudground-php/" + w.user + ".conf"
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	defer h.undo("turn two-factor sign-in off on the WooCommerce site", func() {
		h.api.put(path, map[string]string{"mode": "off"})
		for _, k := range []string{"secret", "last", "recovery", "pending", "fails"} {
			h.wp(w.user, w.path, "user", "meta", "delete", "1", "cloudground_2fa_"+k)
		}
	})()

	c, b := h.api.put(path, map[string]string{"mode": "required"})
	h.check(t, "#74: two-factor sign-in is required", fmt.Sprintf("%d %s", c, strings.TrimSpace(string(b))), `200 {"mode":"required"}`)
	h.assert(t, "#74: the mode is in the PHP master's pool", strings.Contains(readTrim(pool), "env[CLOUDGROUND_2FA] = required"), "#74: %s has no env[CLOUDGROUND_2FA] = required", pool)

	signIn := func(jar *Jar) resp {
		form := url.Values{"log": {"admin"}, "pwd": {wpAdminPW}, "wp-submit": {"Log In"}, "redirect_to": {"https://" + w.dom + "/wp-admin/"}}
		return h.fetch(req{URL: "https://" + w.dom + "/wp-login.php", Form: form.Encode(), Jar: jar})
	}
	loggedIn := func(jar *Jar) bool {
		for _, c := range jar.Cookies(nil) {
			if strings.HasPrefix(c.Name, "wordpress_logged_in_") {
				return true
			}
		}
		return false
	}

	// Not set up yet: the password goes to the setup step, nothing more.
	jar := NewJar()
	r := signIn(jar)
	h.check(t, "#74: a required administrator's password alone is held", fmt.Sprintf("%s %t", r.CodeStr(), strings.Contains(r.Header.Get("Location"), "action=cloudground_2fa")), "303 true")
	h.assert(t, "#74: no session from the password alone", !loggedIn(jar), "#74: wp-login.php set a logged-in cookie for the password alone")
	adm := h.fetch(req{URL: "https://" + w.dom + "/wp-admin/", Jar: jar})
	h.check(t, "#74: wp-admin still asks to sign in", fmt.Sprintf("%s %t", adm.CodeStr(), strings.Contains(adm.Header.Get("Location"), "wp-login.php")), "302 true")
	step := h.fetch(req{URL: "https://" + w.dom + "/wp-login.php?action=cloudground_2fa", Jar: jar})
	h.assert(t, "#74: the second step asks to set the app up with an emailed code", step.Code == 200 && strings.Contains(string(step.Body), `name="cg2fa_mail"`),
		"#74: the setup step answered %s: %.300s", step.CodeStr(), step.Body)

	x := `<?xml version="1.0"?><methodCall><methodName>wp.getUsersBlogs</methodName><params><param><value>admin</value></param><param><value>` + wpAdminPW + `</value></param></params></methodCall>`
	xr := h.fetch(req{URL: "https://" + w.dom + "/xmlrpc.php", Method: "POST", Form: x, Header: map[string]string{"Content-Type": "text/xml"}})
	h.assert(t, "#74: XML-RPC refuses the password alone", strings.Contains(string(xr.Body), "faultCode") && !strings.Contains(string(xr.Body), "isAdmin"),
		"#74: XML-RPC answered %s: %.300s", xr.CodeStr(), xr.Body)

	// With an app: the password, then its code.
	sealed := h.wp(w.user, w.path, "eval", `update_user_meta(1, "cloudground_2fa_secret", \CloudGround\Connector\Seal::close("`+secret+`")); echo "ok";`)
	h.check(t, "#74: a key is set up for the administrator", sealed.Trim(), "ok")
	jar = NewJar()
	r = signIn(jar)
	h.check(t, "#74: an enrolled administrator's password alone is held", fmt.Sprintf("%s %t", r.CodeStr(), loggedIn(jar)), "303 false")
	step = h.fetch(req{URL: "https://" + w.dom + "/wp-login.php?action=cloudground_2fa", Jar: jar})
	m := regexp.MustCompile(`name="cg2fa_form" value="([0-9a-f]{64})"`).FindSubmatch(step.Body)
	form := ""
	if m != nil {
		form = string(m[1])
	}
	post := func(code string) resp {
		return h.fetch(req{URL: "https://" + w.dom + "/wp-login.php?action=cloudground_2fa", Jar: jar,
			Form: url.Values{"cg2fa_code": {code}, "cg2fa_form": {form}}.Encode()})
	}
	r = post("000000")
	h.check(t, "#74: a wrong code opens no session", fmt.Sprintf("%s %t", r.CodeStr(), loggedIn(jar)), "200 false")
	r = post(totpCode(secret, time.Now()))
	h.check(t, "#74: the right code signs in", fmt.Sprintf("%s %t %s", r.CodeStr(), loggedIn(jar), r.Header.Get("Location")), "303 true https://"+w.dom+"/wp-admin/")
	adm = h.fetch(req{URL: "https://" + w.dom + "/wp-admin/", Jar: jar})
	h.check(t, "#74: and wp-admin opens", adm.CodeStr(), "200")

	// The panel's magic login is the panel's own second factor.
	_, b = h.api.post(fmt.Sprintf("/api/sites/%d/wp/login", w.id), `{}`)
	ml := jsonField(b, "url")
	tok := ml[strings.LastIndex(ml, "#")+1:]
	jar = NewJar()
	r = h.fetch(req{URL: "https://" + w.dom + "/?cloudground_login=1", Form: "cloudground_token=" + tok, Jar: jar})
	h.check(t, "#74: the magic login still signs in", fmt.Sprintf("%s %t", r.CodeStr(), loggedIn(jar)), "302 true")

	// Off: the password alone signs in again, and the pool has no setting.
	c, _ = h.api.put(path, map[string]string{"mode": "off"})
	h.check(t, "#74: two-factor sign-in turned off", fmt.Sprint(c), "200")
	h.assert(t, "#74: the pool no longer carries the mode", !strings.Contains(readTrim(pool), "CLOUDGROUND_2FA"), "#74: %s still names CLOUDGROUND_2FA", pool)
	jar = NewJar()
	r = signIn(jar)
	h.check(t, "#74: off, the password signs in", fmt.Sprintf("%s %t", r.CodeStr(), loggedIn(jar)), "302 true")
}

// totpCode is RFC 6238's code of a base32 key at t (SHA-1, six digits, 30 s).
func totpCode(secret string, t time.Time) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(t.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	o := sum[19] & 0x0f
	n := binary.BigEndian.Uint32(sum[o:o+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000)
}

// wpHardening (#75): each hardening item on the WooCommerce site, on with
// the recommended set and then alone off again, checked from outside:
// readme and licence files answer 404 and come back; xmlrpc.php answers
// 403, then answers again from an address on the exception list; the
// version leaves the page head and the asset URLs; the file editor is
// refused to the administrator; the shop and the cart still answer. Then
// the login path: wp-login.php answers 404 (in any spelling), the new path
// shows the form and signs the administrator in, wp-admin answers 404 to a
// visitor; and with everything off the site is as before.
func (h *harness) wpHardening(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	path := fmt.Sprintf("/api/sites/%d/wp/hardening", w.id)
	pool := "/etc/cloudground-php/" + w.user + ".conf"
	defer h.undo("turn the hardening off on the WooCommerce site", func() { h.api.put(path, map[string]any{}) })()
	n := 0
	get := func(p string) resp {
		n++
		return h.fetch(req{URL: "https://" + w.dom + p})
	}
	codeOf := func(p string) string { return get(p).CodeStr() }
	xmlrpc := func() resp {
		x := `<?xml version="1.0"?><methodCall><methodName>system.listMethods</methodName><params></params></methodCall>`
		return h.fetch(req{URL: "https://" + w.dom + "/xmlrpc.php", Method: "POST", Form: x, Header: map[string]string{"Content-Type": "text/xml"}})
	}
	version := h.wp(w.user, w.path, "core", "version").Trim()

	before := map[string]string{}
	for _, p := range []string{"/readme.html", "/license.txt", "/wp-content/plugins/woocommerce/readme.txt"} {
		before[p] = codeOf(p)
	}
	h.check(t, "#75: before, readme.html is served", before["/readme.html"], "200")

	c, b := h.api.put(path, map[string]any{"xmlrpc_off": true, "file_edit_off": true, "hide_version": true, "block_readme": true})
	h.check(t, "#75: the recommended set is applied", fmt.Sprint(c), "200")
	h.assert(t, "#75: the connector's items are in the pool", strings.Contains(readTrim(pool), "env[CLOUDGROUND_HARDENING] = xmlrpc,file_edit,version"), "#75: %s lacks the hardening: %s", pool, b)

	for _, p := range []string{"/readme.html", "/license.txt", "/wp-content/plugins/woocommerce/readme.txt", "/README.html"} {
		h.check(t, "#75: "+p+" answers 404", codeOf(p), "404")
	}
	h.check(t, "#75: an ordinary plugin file is still served", codeOf("/wp-content/plugins/woocommerce/assets/css/woocommerce.css"), "200")
	h.check(t, "#75: XML-RPC answers 403", xmlrpc().CodeStr(), "403")
	home := get("/")
	h.check(t, "#75: the home page answers", home.CodeStr(), "200")
	h.assert(t, "#75: no generator tag", !strings.Contains(string(home.Body), `content="WordPress`), "#75: the home page still has a WordPress generator tag")
	h.assert(t, "#75: no core version in asset URLs", version != "" && !strings.Contains(string(home.Body), "ver="+version), "#75: the home page shows ver=%s", version)
	h.check(t, "#75: the shop answers", codeOf("/shop/"), "200")
	h.check(t, "#75: the cart answers", codeOf("/cart/"), "200")

	_, b = h.api.post(fmt.Sprintf("/api/sites/%d/wp/login", w.id), `{}`)
	ml := jsonField(b, "url")
	jar := NewJar()
	h.fetch(req{URL: "https://" + w.dom + "/?cloudground_login=1", Form: "cloudground_token=" + ml[strings.LastIndex(ml, "#")+1:], Jar: jar})
	ed := h.fetch(req{URL: "https://" + w.dom + "/wp-admin/theme-editor.php", Jar: jar})
	h.assert(t, "#75: the theme editor is refused to the administrator", ed.Code != 200 || strings.Contains(string(ed.Body), "not allowed"), "#75: theme-editor.php answered %s", ed.CodeStr())
	h.check(t, "#75: wp-admin still opens for the administrator", h.fetch(req{URL: "https://" + w.dom + "/wp-admin/", Jar: jar}).CodeStr(), "200")

	c, _ = h.api.put(path, map[string]any{"xmlrpc_off": true, "xmlrpc_allow": []string{"127.0.0.1"}, "file_edit_off": true, "hide_version": true, "block_readme": true})
	h.check(t, "#75: an exception is added", fmt.Sprint(c), "200")
	xr := xmlrpc()
	h.check(t, "#75: XML-RPC answers an address on the exception list", fmt.Sprintf("%s %t", xr.CodeStr(), strings.Contains(string(xr.Body), "system.listMethods")), "200 true")

	c, _ = h.api.put(path, map[string]any{"xmlrpc_off": true, "file_edit_off": true, "hide_version": true})
	h.check(t, "#75: one item off on its own", fmt.Sprint(c), "200")
	h.check(t, "#75: readme.html is served again", codeOf("/readme.html"), before["/readme.html"])
	h.check(t, "#75: XML-RPC is still off", xmlrpc().CodeStr(), "403")

	// The login path.
	c, _ = h.api.put(path, map[string]any{"login_path": "cg-door"})
	h.check(t, "#75: the login path is set", fmt.Sprint(c), "200")
	h.assert(t, "#75: the pool names the login path", strings.Contains(readTrim(pool), "env[CLOUDGROUND_LOGIN_PATH] = cg-door"), "#75: %s lacks the login path", pool)
	for _, p := range []string{"/wp-login.php", "//wp-login.php", "/wp-login.ph%70", "/x/../wp-login.php"} {
		h.check(t, "#75: "+p+" answers 404", codeOf(p), "404")
	}
	door := get("/cg-door")
	h.assert(t, "#75: the new path shows the form, posting to itself", door.Code == 200 && strings.Contains(string(door.Body), `action="https://`+w.dom+`/cg-door"`),
		"#75: /cg-door answered %s: %.300s", door.CodeStr(), door.Body)
	adm := get("/wp-admin/")
	h.check(t, "#75: wp-admin answers 404 to a visitor, with no redirect", fmt.Sprintf("%s %q", adm.CodeStr(), adm.Header.Get("Location")), `404 ""`)
	h.check(t, "#75: admin-ajax.php still answers", h.fetch(req{URL: "https://" + w.dom + "/wp-admin/admin-ajax.php", Form: "action=cg-none"}).CodeStr(), "400")
	jar = NewJar()
	r := h.fetch(req{URL: "https://" + w.dom + "/cg-door", Form: url.Values{"log": {"admin"}, "pwd": {wpAdminPW}, "wp-submit": {"Log In"}}.Encode(), Jar: jar})
	signedIn := false
	for _, ck := range jar.Cookies(nil) {
		signedIn = signedIn || strings.HasPrefix(ck.Name, "wordpress_logged_in_")
	}
	h.check(t, "#75: the administrator signs in at the new path", fmt.Sprintf("%s %t", r.CodeStr(), signedIn), "302 true")

	c, _ = h.api.put(path, map[string]any{})
	h.check(t, "#75: everything off", fmt.Sprint(c), "200")
	h.assert(t, "#75: the pool carries no hardening", !strings.Contains(readTrim(pool), "CLOUDGROUND_HARDENING") && !strings.Contains(readTrim(pool), "CLOUDGROUND_LOGIN_PATH"), "#75: %s still has hardening", pool)
	h.check(t, "#75: wp-login.php answers again", codeOf("/wp-login.php"), "200")
	h.check(t, "#75: XML-RPC answers again", xmlrpc().CodeStr(), "200")
}
