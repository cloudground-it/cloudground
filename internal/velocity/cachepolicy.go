// Package velocity decides how each site's pages are cached and removes
// cached pages from disk (SPEC.md).
package velocity

import (
	"regexp"

	"github.com/cloudground-it/cloudground/internal/state"
)

// CachePolicy is what the nginx renderer needs to know about a site's page
// cache (SPEC §1).
type CachePolicy struct {
	Enabled bool
	// TTLSec: how long a stored page is fresh.
	TTLSec int
	// StaleSec: how long an expired page may still be served, while it is
	// regenerated or while the site errors.
	StaleSec int
	// LockTimeoutSec: how long concurrent misses wait for one regeneration.
	LockTimeoutSec int
	// BypassCookies: a cookie whose name starts with one of these skips the
	// cache.
	BypassCookies []string
	// BypassURIPrefixes are path prefixes whose requests are never cached.
	BypassURIPrefixes []string
	// IgnoreTrackingArgs: a query of tracking parameters only is cached
	// under the bare path; otherwise any query string skips the cache.
	IgnoreTrackingArgs bool
	// ZoneSizeMB sizes the key zone in shared memory.
	ZoneSizeMB int
	// MaxSizeMB caps the site's cache on disk.
	MaxSizeMB int
}

// profileTimes are the per-profile numbers of SPEC §1.3.
type profileTimes struct{ ttl, stale, maxMB int }

var byProfile = map[state.Profile]profileTimes{
	state.ProfileBalanced: {ttl: 600, stale: 7200, maxMB: 1024},
	state.ProfileCommerce: {ttl: 300, stale: 3600, maxMB: 1024},
	state.ProfileMaximum:  {ttl: 86400, stale: 3 * 86400, maxMB: 4096},
}

// Bypass lists of SPEC §1.4.
var (
	sessionCookies = []string{"wordpress_logged_in_", "wp-postpass_", "comment_author_",
		"woocommerce_cart_hash", "woocommerce_items_in_cart", "wp_woocommerce_session_"}
	dynamicPaths = []string{"/wp-admin", "/wp-login.php", "/wp-cron.php", "/xmlrpc.php", "/wp-json/", "/feed"}
	storePaths   = []string{"/cart", "/checkout", "/my-account", "/addons", "/wc-api", "/?wc-ajax="}
)

// Laravel's bypass lists (SPEC §1.4, R-5.2). The session cookie is
// "<APP_NAME slug>_session" (laravel_session by default), so any cookie
// ending in "_session"; then the CSRF cookie and the remember-me cookie
// "remember_web_<hash>". The paths are the routes of Laravel's own auth
// scaffolding (Breeze, Fortify, laravel/ui) and of its first-party admin and
// realtime packages.
var (
	laravelCookies = []string{"_session=", "XSRF-TOKEN=", "remember_web_"}
	laravelPaths   = []string{"/login", "/logout", "/register", "/password/", "/forgot-password",
		"/reset-password", "/email/verify", "/admin", "/nova", "/horizon", "/telescope",
		"/sanctum/", "/livewire/", "/broadcasting/"}
)

// phpSessionCookie is PHP's own session cookie, bypassed on a plain PHP site
// on top of WordPress's set (it may be a hand-installed WordPress).
const phpSessionCookie = "PHPSESSID"

// frameworkCookies are the default session cookies of the PHP frameworks a
// plain PHP site may run (R-10.5), bypassed on top of PHPSESSID: any cookie
// ending in "_session" (CodeIgniter's ci_session, Laravel's
// laravel_session and <app>_session), CakePHP's CAKEPHP, the XSRF-TOKEN
// Laravel and Angular set with a session, and Laravel's remember-me cookie.
// Symfony, Yii, Laminas and Slim keep PHP's own PHPSESSID by default. A
// generic "session" is not on the list: analytics and consent cookies carry
// it too, and every anonymous visitor would skip the cache. Anything else is
// the site's BypassCookies.
var frameworkCookies = []string{"_session=", "CAKEPHP=", "XSRF-TOKEN=", "remember_web_"}

// MaxBypassCookies bounds a site's own bypass cookies.
const MaxBypassCookies = 20

// bypassCookieRe is a cookie name a site may add: letters, digits and
// . _ - (a subset of RFC 6265's token that nothing in nginx's
// configuration reads as syntax), at most 64.
var bypassCookieRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidBypassCookie reports whether name may be one of a site's
// BypassCookies.
func ValidBypassCookie(name string) bool { return bypassCookieRe.MatchString(name) }

// siteCookies are the site's own bypass cookies, each as "name=" (the name
// followed by its value), the invalid ones dropped.
func siteCookies(site state.Site) []string {
	var out []string
	for _, c := range site.Config.BypassCookies {
		if ValidBypassCookie(c) && len(out) < MaxBypassCookies {
			out = append(out, c+"=")
		}
	}
	return out
}

// PolicyFor is site's page-cache policy (SPEC §1).
func PolicyFor(site state.Site) CachePolicy {
	if !site.Config.CacheEnabled || site.Type == state.SiteStatic || site.Type == state.SiteProxy {
		return CachePolicy{}
	}
	times, ok := byProfile[site.Profile]
	if !ok {
		times = byProfile[state.ProfileBalanced]
	}
	p := CachePolicy{
		Enabled:            true,
		TTLSec:             times.ttl,
		StaleSec:           times.stale,
		LockTimeoutSec:     5,
		IgnoreTrackingArgs: true,
		ZoneSizeMB:         32,
		MaxSizeMB:          times.maxMB,
	}
	if site.Config.CacheTTLSec > 0 {
		p.TTLSec = site.Config.CacheTTLSec
	}
	var paths []string
	switch site.Type {
	case state.SiteLaravel:
		// Laravel's own set only: WordPress's and WooCommerce's rules mean
		// nothing to it, whatever the profile.
		p.BypassCookies = append(append([]string(nil), laravelCookies...), siteCookies(site)...)
		p.BypassURIPrefixes = append([]string(nil), laravelPaths...)
		return p
	case state.SitePHP:
		p.BypassCookies = append(append(append([]string(nil), sessionCookies...), phpSessionCookie), frameworkCookies...)
		paths = append(paths, dynamicPaths...)
	case state.SiteWordPress, state.SiteWooCommerce:
		p.BypassCookies = append([]string(nil), sessionCookies...)
		paths = append(paths, dynamicPaths...)
	}
	if site.Type == state.SiteWooCommerce || site.Profile == state.ProfileCommerce {
		paths = append(paths, storePaths...)
	}
	p.BypassCookies = append(p.BypassCookies, siteCookies(site)...)
	p.BypassURIPrefixes = paths
	return p
}
