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
