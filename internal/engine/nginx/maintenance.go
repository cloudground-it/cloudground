package nginx

import (
	"fmt"
	"net/netip"
	"strings"
	"unicode"
)

// A site's maintenance page (SPEC §4.9, #79): maps before the server, an
// `if` in it, and two locations.

// Defaults of the page and of its Retry-After.
const (
	defaultMaintenanceTitle   = "Down for maintenance"
	defaultMaintenanceMessage = "We are making some changes. Please come back in a little while."
	defaultMaintenanceRetry   = 3600
)

// maintenanceText encodes user text for the single-quoted string of a
// `return`: letters, digits, spaces and a few plain marks as they are, a
// line break as <br>, everything else as a numeric character reference, so
// nothing in it is HTML, nor a variable or a quote to nginx.
func maintenanceText(s string) string {
	var b strings.Builder
	for _, r := range strings.ReplaceAll(s, "\r\n", "\n") {
		switch {
		case r == '\n':
			b.WriteString("<br>")
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune(".,:!?()-_/@+%", r):
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "&#%d;", r)
		}
	}
	return b.String()
}

// maintenancePage is the HTML the visitor gets.
func maintenancePage(title, message string) string {
	if title == "" {
		title = defaultMaintenanceTitle
	}
	if message == "" {
		message = defaultMaintenanceMessage
	}
	t, m := maintenanceText(title), maintenanceText(message)
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">` +
		`<title>` + t + `</title><style>` +
		`body{margin:0;min-height:100vh;display:grid;place-items:center;font:18px/1.6 system-ui,sans-serif;color:#1f2937;background:#f8fafc}` +
		`main{max-width:34rem;padding:2rem}h1{font-size:1.75rem;margin:0 0 1rem}p{margin:0}` +
		`@media(prefers-color-scheme:dark){body{color:#e5e7eb;background:#0f172a}}` +
		`</style></head><body><main><h1>` + t + `</h1><p>` + m + `</p></main></body></html>`
}

// maintenanceMaps writes what decides who sees the site: the addresses
// (loopback, this machine's, the site's own list), then the map that adds
// the cookie and the key URL.
func (v *vhost) maintenanceMaps(c *conf) {
	m := v.in.Site.Config.Maintenance
	if !m.Enabled {
		return
	}
	id := v.in.Site.ID
	c.put(0, "")
	c.putf(0, "geo $cg_mnt_ip_%d {", id)
	c.put(1, "default  0;", "127.0.0.0/8  1;", "::1/128  1;")
	for _, a := range v.in.MaintenanceExempt {
		a = a.Unmap()
		c.putf(1, "%s  1;", netip.PrefixFrom(a, a.BitLen()))
	}
	for _, a := range m.Allow {
		c.putf(1, "%s  1;", a)
	}
	c.put(0, "}")
	c.putf(0, `map "$cg_mnt_ip_%d|$cookie_cg_maint|$uri" $cg_mnt_%d {`, id, id)
	c.put(1, "default  1;", `"~^1\|"  0;`, fmt.Sprintf(`"~^0\|%s\|"  0;`, m.Key),
		fmt.Sprintf(`"~^0\|[^|]*\|/\.cg-maintenance/%s$"  0;`, m.Key))
	c.put(0, "}")
}

// maintenanceServer writes the server's part: the `if` and the locations.
func (v *vhost) maintenanceServer(c *conf) {
	m := v.in.Site.Config.Maintenance
	if !m.Enabled {
		return
	}
	retry := m.RetryAfterSec
	if retry == 0 {
		retry = defaultMaintenanceRetry
	}
	c.put(1, "",
		"# The maintenance page (SPEC §4.9): a 503 for everyone but this machine, the",
		"# addresses listed and a browser holding the key's cookie. The server rewrite",
		"# phase runs before the page cache is read.")
	c.putf(1, "if ($cg_mnt_%d) {", v.in.Site.ID)
	c.put(2, "return 599;")
	c.put(1, "}")
	// 599 is no status a backend gives: only this `if` reaches the page, and
	// the page answers 503 whatever PHP does for the visitors let through.
	c.put(1, "error_page 599 =503 @cg_maintenance;")
	c.put(1, "location @cg_maintenance {")
	c.put(2, "default_type text/html;", "charset utf-8;")
	c.put(2, v.sec...)
	c.putf(2, "add_header Retry-After %d always;", retry)
	c.put(2, `add_header Cache-Control "no-store" always;`)
	c.putf(2, "return 503 '%s';", maintenancePage(m.Title, m.Message))
	c.put(1, "}")
	c.put(1, "# Opening this address in a browser sets the cookie that lets it through.")
	c.putf(1, "location = /.cg-maintenance/%s {", m.Key)
	c.put(2, v.sec...)
	c.putf(2, `add_header Set-Cookie "cg_maint=%s; Path=/; Max-Age=28800; Secure; HttpOnly; SameSite=Lax" always;`, m.Key)
	c.put(2, `add_header Cache-Control "no-store" always;`)
	c.put(2, "return 302 /;")
	c.put(1, "}")
}
