package velocity

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

func cached(t state.SiteType, p state.Profile) state.Site {
	return state.Site{Type: t, Profile: p, Config: state.SiteConfig{CacheEnabled: true}}
}

// SPEC §1.1: no page cache when it is off, or for static and proxy sites.
func TestPolicyOff(t *testing.T) {
	for _, s := range []state.Site{
		{Type: state.SiteWordPress, Profile: state.ProfileBalanced},
		cached(state.SiteStatic, state.ProfileMaximum),
		cached(state.SiteProxy, state.ProfileBalanced),
	} {
		if p := PolicyFor(s); p.Enabled || p.TTLSec != 0 || p.BypassCookies != nil {
			t.Errorf("%s/%s cache %v: %+v", s.Type, s.Profile, s.Config.CacheEnabled, p)
		}
	}
}

// SPEC §1.3: the profile sets the times; a site's TTL replaces only the TTL.
func TestPolicyTimes(t *testing.T) {
	for _, c := range []struct {
		p                 state.Profile
		ttl, stale, maxMB int
	}{
		{state.ProfileBalanced, 600, 7200, 1024},
		{"", 600, 7200, 1024},
		{state.ProfileCommerce, 300, 3600, 1024},
		{state.ProfileMaximum, 86400, 259200, 4096},
	} {
		p := PolicyFor(cached(state.SiteWordPress, c.p))
		if !p.Enabled || p.TTLSec != c.ttl || p.StaleSec != c.stale || p.MaxSizeMB != c.maxMB ||
			p.LockTimeoutSec != 5 || p.ZoneSizeMB != 32 || !p.IgnoreTrackingArgs {
			t.Errorf("%q: %+v", c.p, p)
		}
	}
	s := cached(state.SiteWordPress, state.ProfileCommerce)
	s.Config.CacheTTLSec = 42
	if p := PolicyFor(s); p.TTLSec != 42 || p.StaleSec != 3600 {
		t.Errorf("TTL override: %+v", p)
	}
}

// SPEC §1.4: sessions and admin paths skip the cache, and a store's cart
// and checkout too; each policy owns its lists.
func TestPolicyBypass(t *testing.T) {
	wp := PolicyFor(cached(state.SiteWordPress, state.ProfileBalanced))
	if !slices.Contains(wp.BypassCookies, "wordpress_logged_in_") || !slices.Contains(wp.BypassURIPrefixes, "/wp-admin") ||
		slices.Contains(wp.BypassURIPrefixes, "/checkout") {
		t.Errorf("WordPress: %+v", wp)
	}
	shop := PolicyFor(cached(state.SiteWooCommerce, state.ProfileBalanced))
	if !slices.Contains(shop.BypassURIPrefixes, "/checkout") || !slices.Contains(shop.BypassURIPrefixes, "/?wc-ajax=") {
		t.Errorf("WooCommerce: %+v", shop)
	}
	if p := PolicyFor(cached(state.SitePHP, state.ProfileCommerce)); !slices.Contains(p.BypassURIPrefixes, "/cart") {
		t.Errorf("Commerce profile: %+v", p)
	}
	wp.BypassCookies[0] = "changed"
	if PolicyFor(cached(state.SiteWordPress, state.ProfileBalanced)).BypassCookies[0] != "wordpress_logged_in_" {
		t.Error("a policy's list is shared with every other policy")
	}
}

func seed(t *testing.T, dir string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		p := CacheFilePath(dir, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(k), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func present(dir, key string) bool {
	_, err := os.Stat(CacheFilePath(dir, key))
	return err == nil
}

// SPEC §2: nginx's levels=1:2 layout of the key's MD5.
func TestEntryLayout(t *testing.T) {
	got := CacheFilePath("/c", CacheKey("https", "GET", "example.com", "/hello/"))
	name := filepath.Base(got)
	want := filepath.Join("/c", name[31:], name[29:31], name)
	if len(name) != 32 || got != want {
		t.Fatalf("%s, want %s", got, want)
	}
}

// SPEC §3: both methods, both schemes, the extra hosts; a cold URL is no
// error and a bad one is.
func TestPurgeURLs(t *testing.T) {
	dir := t.TempDir()
	keep := CacheKey("https", "GET", "example.com", "/other/")
	seed(t, dir, CacheKey("https", "GET", "example.com", "/p/"), CacheKey("http", "HEAD", "example.com", "/p/"),
		CacheKey("https", "GET", "www.example.com", "/p/"), keep)
	n, err := PurgeURLs(dir, []string{"https://example.com/p/"}, "www.example.com")
	if err != nil || n != 3 || !present(dir, keep) {
		t.Fatalf("removed %d, err %v, unrelated entry kept %v", n, err, present(dir, keep))
	}
	if n, err := PurgeURLs(dir, []string{"https://example.com/cold/"}); n != 0 || err != nil {
		t.Fatalf("cold URL: %d %v", n, err)
	}
	if _, err := PurgeURLs(dir, []string{"/no-host/"}); err == nil {
		t.Fatal("a URL without a host was accepted")
	}
}

// R-5.2: nginx keys on $host, lower case and without the port, so a URL
// that names the port or other case must purge those entries. The old code
// keyed on "example.com:443" and removed nothing here.
func TestPurgeURLsKeysOnHostWithoutPort(t *testing.T) {
	for _, raw := range []string{"https://example.com:443/p/", "http://EXAMPLE.com:80/p/", "https://Example.COM/p/"} {
		dir := t.TempDir()
		seed(t, dir, CacheKey("https", "GET", "example.com", "/p/"), CacheKey("https", "HEAD", "example.com", "/p/"))
		if n, err := PurgeURLs(dir, []string{raw}); n != 2 || err != nil {
			t.Errorf("%s: removed %d, err %v", raw, n, err)
		}
	}
	dir := t.TempDir()
	seed(t, dir, CacheKey("https", "GET", "www.example.com", "/p/"))
	if n, _ := PurgeURLs(dir, []string{"https://example.com/p/"}, "WWW.example.com:443", "example.com"); n != 1 {
		t.Errorf("extra hosts with port or case: removed %d", n)
	}
	if got := purgeHosts(&url.URL{Host: "[::1]:443"}, []string{"Example.com", "example.com"}); !slices.Equal(got, []string{"[::1]", "example.com"}) {
		t.Errorf("hosts %q", got)
	}
}

// SPEC §4.
func TestPurgeAll(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "a", "b", "c")
	if n, err := PurgeAll(dir); n != 3 || err != nil {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left %d entries", len(left))
	}
	if n, err := PurgeAll(filepath.Join(dir, "absent")); n != 0 || err != nil {
		t.Fatalf("absent dir: %d %v", n, err)
	}
}

func TestCacheDirName(t *testing.T) {
	if got := SanitizeCacheDirName("Shop.Example.COM/../x y"); got != "shop.example.com_.._x_y" {
		t.Fatal(got)
	}
}
