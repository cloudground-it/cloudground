package velocity

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The golden files pin the cache policy of every site kind, the cache key
// and its file layout, and what each purge removes (Phase 5 gate). Captured
// from the code before the rewrite; regenerate with CG_GOLDEN_UPDATE=1 only
// for an intended change.

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("CG_GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (CG_GOLDEN_UPDATE=1 writes it)", err)
	}
	if string(want) != got {
		wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
		for i := 0; i < len(wl) || i < len(gl); i++ {
			var w, g string
			if i < len(wl) {
				w = wl[i]
			}
			if i < len(gl) {
				g = gl[i]
			}
			if w != g {
				t.Fatalf("%s differs from the golden at line %d:\nwant %q\ngot  %q", name, i+1, w, g)
			}
		}
	}
}

func TestGoldenPolicy(t *testing.T) {
	var b strings.Builder
	for _, typ := range []state.SiteType{state.SiteWordPress, state.SiteWooCommerce, state.SiteLaravel,
		state.SitePHP, state.SiteStatic, state.SiteProxy, ""} {
		for _, prof := range []state.Profile{state.ProfileBalanced, state.ProfileCommerce, state.ProfileMaximum, ""} {
			for _, on := range []bool{false, true} {
				for _, ttl := range []int{-5, 0, 120, 86400 * 7} {
					s := state.Site{Type: typ, Profile: prof}
					s.Config.CacheEnabled = on
					s.Config.CacheTTLSec = ttl
					fmt.Fprintf(&b, "type=%q profile=%q cache=%v ttl=%d %#v\n", typ, prof, on, ttl, PolicyFor(s))
				}
			}
		}
	}
	checkGolden(t, "policy", b.String())
}

func TestGoldenKeys(t *testing.T) {
	var b strings.Builder
	for _, k := range [][4]string{
		{"https", "GET", "example.com", "/"},
		{"http", "HEAD", "example.com", "/a/b/?x=1"},
		{"https", "GET", "example.com:443", "/hello/"},
		{"", "", "", ""},
	} {
		key := CacheKey(k[0], k[1], k[2], k[3])
		fmt.Fprintf(&b, "%q -> %q -> %s\n", k, key, CacheFilePath("/var/cache/cloudground/x", key))
	}
	for _, d := range []string{"example.com", "Shop.Example.COM", "xn--bcher-kva.de", "a_b c/../d", "école.fr", ""} {
		fmt.Fprintf(&b, "SanitizeCacheDirName(%q)=%q\n", d, SanitizeCacheDirName(d))
	}
	checkGolden(t, "keys", b.String())
}

// purgeCase seeds a cache with an entry for every key in seed, purges, and
// reports what was removed and what is left.
func purgeCase(t *testing.T, seed []string, run func(dir string) (int, error)) string {
	dir := t.TempDir()
	for _, k := range seed {
		p := CacheFilePath(dir, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(k), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n, err := run(dir)
	var left []string
	for _, k := range seed {
		if _, err := os.Stat(CacheFilePath(dir, k)); err == nil {
			left = append(left, k)
		}
	}
	sort.Strings(left)
	return fmt.Sprintf("removed=%d err=%v left=%q", n, err, left)
}

func TestGoldenPurge(t *testing.T) {
	var seed []string
	for _, host := range []string{"example.com", "www.example.com", "example.com:443", "other.test"} {
		for _, scheme := range []string{"https", "http"} {
			for _, method := range []string{"GET", "HEAD", "POST"} {
				for _, uri := range []string{"/", "/post/", "/post/?utm_source=x", "/shop/?page=2"} {
					seed = append(seed, CacheKey(scheme, method, host, uri))
				}
			}
		}
	}
	cases := []struct {
		urls  []string
		extra []string
	}{
		{[]string{"https://example.com/post/"}, nil},
		{[]string{"https://example.com/post/"}, []string{"example.com", "www.example.com"}},
		{[]string{"http://www.example.com/"}, []string{"example.com"}},
		{[]string{"https://example.com:443/post/"}, nil},
		{[]string{"https://example.com:443/post/"}, []string{"example.com"}},
		{[]string{"https://EXAMPLE.com/post/"}, nil},
		{[]string{"https://example.com/shop/?page=2", "https://example.com"}, nil},
		{[]string{"https://example.com/post/?utm_source=x"}, nil},
		{[]string{"https://other.test/post/"}, []string{"example.com"}},
		{[]string{"/relative/"}, nil},
		{[]string{"https://example.com/post/", "::bad"}, nil},
		{nil, []string{"example.com"}},
	}
	var b strings.Builder
	for _, c := range cases {
		got := purgeCase(t, seed, func(dir string) (int, error) { return PurgeURLs(dir, c.urls, c.extra...) })
		fmt.Fprintf(&b, "PurgeURLs(%q, extra=%q): %s\n", c.urls, c.extra, got)
	}
	fmt.Fprintf(&b, "PurgeAll: %s\n", purgeCase(t, seed, PurgeAll))
	missing, err := PurgeAll(filepath.Join(t.TempDir(), "absent"))
	fmt.Fprintf(&b, "PurgeAll(absent): removed=%d err=%v\n", missing, err)
	checkGolden(t, "purge", b.String())
}
