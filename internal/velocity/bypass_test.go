package velocity

import (
	"slices"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Each site type bypasses on its own cookies and paths (internal/engine
// SPEC §5.1, velocity SPEC §1.4, R-5.2).
func TestBypassSetPerType(t *testing.T) {
	policy := func(typ state.SiteType, prof state.Profile) CachePolicy {
		return PolicyFor(state.Site{Type: typ, Profile: prof, Config: state.SiteConfig{CacheEnabled: true}})
	}

	lv := policy(state.SiteLaravel, state.ProfileBalanced)
	for _, c := range []string{"_session=", "XSRF-TOKEN=", "remember_web_"} {
		if !slices.Contains(lv.BypassCookies, c) {
			t.Errorf("laravel: no bypass on %q", c)
		}
	}
	for _, u := range []string{"/login", "/logout", "/admin", "/horizon"} {
		if !slices.Contains(lv.BypassURIPrefixes, u) {
			t.Errorf("laravel: no bypass under %q", u)
		}
	}
	// WordPress's and WooCommerce's rules mean nothing to Laravel.
	for _, c := range lv.BypassCookies {
		if slices.Contains(sessionCookies, c) {
			t.Errorf("laravel has WordPress's cookie %q", c)
		}
	}
	if lc := policy(state.SiteLaravel, state.ProfileCommerce); slices.Contains(lc.BypassURIPrefixes, "/cart") {
		t.Error("laravel on the commerce profile has WooCommerce's /cart")
	}

	wp := policy(state.SiteWordPress, state.ProfileBalanced)
	if !slices.Equal(wp.BypassCookies, sessionCookies) || !slices.Equal(wp.BypassURIPrefixes, dynamicPaths) {
		t.Errorf("wordpress: %v %v", wp.BypassCookies, wp.BypassURIPrefixes)
	}
	if woo := policy(state.SiteWooCommerce, state.ProfileBalanced); !slices.Contains(woo.BypassURIPrefixes, "/checkout") {
		t.Error("woocommerce without its checkout bypass")
	}
	php := policy(state.SitePHP, state.ProfileBalanced)
	if !slices.Contains(php.BypassCookies, "PHPSESSID") || !slices.Contains(php.BypassCookies, "wordpress_logged_in_") {
		t.Errorf("php: %v", php.BypassCookies)
	}
	if pc := policy(state.SitePHP, state.ProfileCommerce); !slices.Contains(pc.BypassURIPrefixes, "/cart") {
		t.Error("php on the commerce profile lost the commerce paths")
	}
}
