package velocity

import (
	"slices"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A plain PHP site also bypasses on the PHP frameworks' default session
// cookies, and every cached type on the site's own bypass cookies, each as
// "name="; an invalid name never reaches the policy (R-10.5).
func TestFrameworkAndSiteBypassCookies(t *testing.T) {
	php := PolicyFor(state.Site{Type: state.SitePHP, Config: state.SiteConfig{CacheEnabled: true}})
	for _, c := range []string{"PHPSESSID", "_session=", "CAKEPHP=", "XSRF-TOKEN=", "remember_web_"} {
		if !slices.Contains(php.BypassCookies, c) {
			t.Errorf("php: no bypass on %q", c)
		}
	}
	if wp := PolicyFor(state.Site{Type: state.SiteWordPress, Config: state.SiteConfig{CacheEnabled: true}}); slices.Contains(wp.BypassCookies, "CAKEPHP=") {
		t.Error("wordpress has the PHP frameworks' cookies")
	}
	extra := []string{"myapp_sid", "bad name", "x\"y", "${a}", "sess.v2"}
	for _, typ := range []state.SiteType{state.SitePHP, state.SiteWordPress, state.SiteLaravel} {
		base := PolicyFor(state.Site{Type: typ, Config: state.SiteConfig{CacheEnabled: true}}).BypassCookies
		p := PolicyFor(state.Site{Type: typ, Config: state.SiteConfig{CacheEnabled: true, BypassCookies: extra}})
		if want := append(slices.Clone(base), "myapp_sid=", "sess.v2="); !slices.Equal(p.BypassCookies, want) {
			t.Errorf("%s: %q, want %q", typ, p.BypassCookies, want)
		}
	}
	if p := PolicyFor(state.Site{Type: state.SiteStatic, Config: state.SiteConfig{CacheEnabled: true, BypassCookies: extra}}); p.Enabled {
		t.Error("a static site got a cache")
	}
	for name, ok := range map[string]bool{"a": true, "My-App.sid_2": true, "": false, "a b": false, "a;b": false, "a=b": false, "a\"": false, "{x}": false, "a$": false} {
		if ValidBypassCookie(name) != ok {
			t.Errorf("ValidBypassCookie(%q) != %v", name, ok)
		}
	}
}
