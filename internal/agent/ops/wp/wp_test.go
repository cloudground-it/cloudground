package wp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// fakeWP answers wp-cli invocations from respond and records them.
type fakeWP struct {
	mu      sync.Mutex
	calls   []host.Invocation
	respond func(args []string) (string, error)
}

func (f *fakeWP) Run(_ context.Context, inv host.Invocation) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inv)
	f.mu.Unlock()
	if f.respond == nil {
		return nil, nil
	}
	out, err := f.respond(inv.Args)
	return []byte(out), err
}

func (f *fakeWP) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// fakePHP records reloads and restarts.
type fakePHP struct {
	mu     sync.Mutex
	events []string
}

func (p *fakePHP) Reload(_ context.Context, s state.Site) error  { return p.add("reload " + s.Domain) }
func (p *fakePHP) Restart(_ context.Context, s state.Site) error { return p.add("restart " + s.Domain) }
func (p *fakePHP) add(e string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}
func (p *fakePHP) got() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.events, ",")
}

const domain = "shop.example.com"

func wpSite(root string) state.Site {
	return state.Site{ID: 7, Domain: domain, Type: state.SiteWooCommerce, SystemUser: "cg-site-7",
		RootPath: root, PHPVersion: "8.3"}
}

func newOps(t *testing.T, w *fakeWP, php *fakePHP) *Ops {
	t.Helper()
	return &Ops{
		WP:  w,
		PHP: php,
		OpenSite: func(s state.Site) (*sitefs.Root, error) {
			return sitefs.Open(s.RootPath, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
		},
	}
}

// siteTree builds root/{releases/r1/wp-content, shared} with current ->
// releases/r1, the layout the agent creates.
func siteTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"releases/r1/wp-content", "shared"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/r1", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	r, err := sitefs.Open(root, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
	if errors.Is(err, sitefs.ErrUnsupported) {
		t.Skip("no openat2 here")
	}
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	return root
}

// ---------------------------------------------------------------- warmer --

// origin is a stand-in for the site's nginx: it counts requests per path and
// answers like the page cache (MISS the first time, HIT after).
type origin struct {
	mu       sync.Mutex
	hits     map[string]int
	enc      map[string][]string
	hosts    map[string]bool
	agents   map[string]bool
	keys     map[string]bool
	inflight int
	peak     int
	delay    time.Duration
	pages    map[string]string // path -> body served with 200 (sitemaps)
}

func newOrigin() *origin {
	return &origin{hits: map[string]int{}, enc: map[string][]string{}, hosts: map[string]bool{},
		agents: map[string]bool{}, keys: map[string]bool{}, pages: map[string]string{}}
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	p := r.URL.RequestURI()
	o.hits[p]++
	n := o.hits[p]
	o.enc[p] = append(o.enc[p], r.Header.Get("Accept-Encoding"))
	o.hosts[r.Host+" sni="+r.TLS.ServerName] = true
	o.agents[r.UserAgent()] = true
	o.keys[r.Header.Get("X-CloudGround-Internal")] = true
	o.inflight++
	if o.inflight > o.peak {
		o.peak = o.inflight
	}
	body, isPage := o.pages[p]
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.inflight--; o.mu.Unlock() }()
	time.Sleep(o.delay)
	if isPage {
		w.Write([]byte(body))
		return
	}
	switch {
	case strings.HasPrefix(p, "/cookie"):
		w.Header().Set("Set-Cookie", "s=1")
		w.Header().Set("X-CloudGround-Cache", "MISS")
	case strings.HasPrefix(p, "/missing"):
		w.WriteHeader(http.StatusNotFound)
		return
	case n == 1:
		w.Header().Set("X-CloudGround-Cache", "MISS")
	default:
		w.Header().Set("X-CloudGround-Cache", "HIT")
	}
	w.Write([]byte("<html>" + strings.Repeat("x", 4096) + "</html>"))
}

func (o *origin) count(p string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[p]
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// The warmer makes exactly one request per page: the cache keeps one gzip
// entry and gunzips it for clients without gzip, so a second request (the old
// "plain" pass, then a confirming gzip pass) only costs PHP renders.
func TestWarmRequestsEachURLOnce(t *testing.T) {
	org := newOrigin()
	w := &fakeWP{respond: func(args []string) (string, error) {
		if args[0] == "post" {
			return strings.Join([]string{
				"https://" + domain + "/",              // the home page, already first
				"https://" + domain + "/hello-world/",  // a post
				"https://" + domain + "/sample-page/",  // a page
				"https://" + domain + "/product/mug/",  // a WooCommerce product
				"https://" + domain + "/hello-world/",  // a duplicate
				"https://SHOP.example.com/case/",       // the domain in another case
				"https://" + domain + ":443/port443/",  // the explicit default port
				"https://" + domain + "/q/?a=1&b=2",    // a query string kept
				"https://cookie." + domain + "/x/",     // a subdomain: not this site
				"https://evil.test/hello-world/",       // another host
				"http://" + domain + "/plain-http/",    // not https
				"https://" + domain + ":8443/other/",   // another port
				"https://" + domain + "@evil.test/at/", // user info trick
			}, "\n"), nil
		}
		return "", nil
	}}
	ops := newOps(t, w, &fakePHP{})
	ops.LoopbackAddr = serve(t, org)
	site := wpSite(t.TempDir())

	res, err := ops.WarmCache(context.Background(), rpc.WarmParams{Site: site})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/hello-world/", "/sample-page/", "/product/mug/", "/case/", "/port443/", "/q/?a=1&b=2"}
	for _, p := range want {
		if n := org.count(p); n != 1 {
			t.Errorf("%s requested %d times, want exactly 1", p, n)
		}
	}
	org.mu.Lock()
	total := 0
	for p, n := range org.hits {
		total += n
		for _, e := range org.enc[p] {
			if e != "gzip" {
				t.Errorf("%s requested with Accept-Encoding %q, want gzip", p, e)
			}
		}
	}
	if total != len(want) {
		t.Errorf("%d requests in all, want %d: %v", total, len(want), org.hits)
	}
	if !org.hosts[domain+" sni="+domain] || len(org.hosts) != 1 {
		t.Errorf("requests did not all carry the site's Host and SNI: %v", org.hosts)
	}
	if !org.agents[userAgent] || len(org.agents) != 1 {
		t.Errorf("user agents: %v", org.agents)
	}
	org.mu.Unlock()
	if res.Warmed != len(want) || res.Cached != len(want) {
		t.Errorf("result %+v, want %d warmed and cached", res, len(want))
	}
	var list string
	for _, l := range w.lines() {
		if strings.HasPrefix(l, "post list") {
			list = l
		}
	}
	if !strings.Contains(list, "--post_type=post,page,product") || !strings.Contains(list, "--post_status=publish") ||
		!strings.Contains(list, "--posts_per_page=200") {
		t.Errorf("wp-cli URL list: %q", list)
	}
}

// A site's sitemap decides nothing about where the warmer goes: foreign
// entries, including foreign child sitemaps, are skipped rather than turned
// into "/" (which warmed the home page once per foreign entry).
func TestWarmSitemapStaysOnTheSite(t *testing.T) {
	org := newOrigin()
	org.pages["/wp-sitemap.xml"] = `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://` + domain + `/wp-sitemap-posts-post-1.xml</loc></sitemap>
  <sitemap><loc>https://evil.test/wp-sitemap-posts-page-1.xml</loc></sitemap>
  <sitemap><loc>https://` + domain + `:6379/wp-sitemap-x.xml</loc></sitemap>
</sitemapindex>`
	org.pages["/wp-sitemap-posts-post-1.xml"] = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://` + domain + `/a/</loc></url>
  <url><loc> https://` + domain + `/b/?x=1&amp;y=2 </loc></url>
  <url><loc>https://evil.test/c/</loc></url>
  <url><loc>https://evil.test/d/</loc></url>
  <url><loc>//evil.test/e/</loc></url>
  <url><loc>/f/</loc></url>
</urlset>`
	ops := newOps(t, &fakeWP{}, &fakePHP{})
	ops.LoopbackAddr = serve(t, org)
	site := wpSite(t.TempDir())
	site.Type = state.SitePHP

	res, err := ops.WarmCache(context.Background(), rpc.WarmParams{Site: site})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/a/", "/b/?x=1&y=2", "/f/", "/wp-sitemap.xml", "/wp-sitemap-posts-post-1.xml"} {
		if n := org.count(p); n != 1 {
			t.Errorf("%s requested %d times, want 1", p, n)
		}
	}
	org.mu.Lock()
	for p := range org.hits {
		if strings.Contains(p, "page-1") || strings.Contains(p, "sitemap-x") || p == "/c/" || p == "/e/" {
			t.Errorf("foreign entry %s was requested", p)
		}
	}
	org.mu.Unlock()
	if res.Warmed != 4 {
		t.Errorf("warmed %d, want 4", res.Warmed)
	}
}

func TestWarmIsBoundedAndCarriesTheStagingKey(t *testing.T) {
	org := newOrigin()
	org.delay = 30 * time.Millisecond
	var urls []string
	for i := range 30 {
		urls = append(urls, "https://"+domain+"/p"+strings.Repeat("x", i)+"/")
	}
	w := &fakeWP{respond: func(args []string) (string, error) { return strings.Join(urls, "\n"), nil }}
	ops := newOps(t, w, &fakePHP{})
	ops.LoopbackAddr = serve(t, org)
	ops.WarmWorkers = 3
	site := wpSite(t.TempDir())
	site.Config.StagingAuthHash = "$2a$10$abcdefghijklmnopqrstuv"

	res, err := ops.WarmCache(context.Background(), rpc.WarmParams{Site: site, MaxURLs: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Warmed != 11 {
		t.Errorf("warmed %d, want 11 (the home page and max_urls 10)", res.Warmed)
	}
	org.mu.Lock()
	defer org.mu.Unlock()
	if org.peak > 3 || org.peak < 2 {
		t.Errorf("peak concurrency %d, want 2..3 with 3 workers", org.peak)
	}
	if !org.keys[site.InternalKey()] || len(org.keys) != 1 {
		t.Errorf("internal key not on every request: %v", org.keys)
	}
}

func TestWarmStopsWhenCancelled(t *testing.T) {
	org := newOrigin()
	org.delay = 50 * time.Millisecond
	var urls []string
	for i := range 100 {
		urls = append(urls, "/p"+strings.Repeat("x", i)+"/")
	}
	ops := newOps(t, &fakeWP{respond: func([]string) (string, error) { return strings.Join(urls, "\n"), nil }}, &fakePHP{})
	ops.LoopbackAddr = serve(t, org)
	ops.WarmWorkers = 2
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := ops.WarmCache(ctx, rpc.WarmParams{Site: wpSite(t.TempDir())})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v, want the deadline", err)
	}
	if res.Warmed >= 50 {
		t.Errorf("warmed %d pages after the deadline", res.Warmed)
	}
}

func TestWarmCountsWhatTheCacheTook(t *testing.T) {
	org := newOrigin()
	w := &fakeWP{respond: func([]string) (string, error) {
		return "/cookie/\n/missing/\n/fine/", nil
	}}
	ops := newOps(t, w, &fakePHP{})
	ops.LoopbackAddr = serve(t, org)
	res, err := ops.WarmCache(context.Background(), rpc.WarmParams{Site: wpSite(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	// "/" and /fine/ are MISSes the cache stores; /cookie/ sets a cookie and
	// /missing/ carries no cache header at all.
	if res.Warmed != 4 || res.Cached != 2 {
		t.Errorf("result %+v, want 4 warmed, 2 cached", res)
	}
}

func TestOwnPath(t *testing.T) {
	cases := map[string]string{
		"https://example.com/a/b/":     "/a/b/",
		"https://example.com":          "/",
		"https://EXAMPLE.com/x":        "/x",
		"https://example.com:443/x":    "/x",
		"/already/a/path":              "/already/a/path",
		"https://example.com/x?y=1#f":  "/x?y=1",
		"  https://example.com/trim/ ": "/trim/",
	}
	for in, want := range cases {
		if got, ok := ownPath(in, "example.com"); !ok || got != want {
			t.Errorf("ownPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"https://example.com@evil.test:8443/x",
		"https://user@example.com/x",
		"https://example.com:6379/",
		"https://example.com:80/",
		"http://example.com/x",
		"ftp://example.com/x",
		"https://evil.test/example.com/x",
		"//evil.test/x",
		"https://sub.example.com/x",
		"https://example.com.evil.test/x",
		"example.com/x",
		"",
		"https://example.com/\x00",
	} {
		if got, ok := ownPath(in, "example.com"); ok {
			t.Errorf("ownPath(%q) = %q, accepted", in, got)
		}
	}
}

// -------------------------------------------------------------- toolkit --

func TestMagicLogin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	earlier := "cloudground_login_" + strings.Repeat("ab", 32)
	w := &fakeWP{respond: func(args []string) (string, error) {
		switch strings.Join(args[:2], " ") {
		case "user list":
			return "3\n", nil
		case "option list":
			// An unused earlier link, and an option that only looks like one.
			return earlier + "\ncloudground_login_settings\n", nil
		}
		return "", nil
	}}
	ops := newOps(t, w, &fakePHP{})
	ops.Now = func() time.Time { return now }
	site := wpSite("/srv/sites/" + domain)
	res, err := ops.MagicLogin(context.Background(), rpc.WPParams{Site: site})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "https://" + domain + "/?cloudground_login=1#"
	token, ok := strings.CutPrefix(res.URL, prefix)
	if !ok || !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(token) {
		t.Fatalf("login URL %q", res.URL)
	}
	sum := sha256.Sum256([]byte(token))
	want := []string{
		"user list --role=administrator --field=ID --orderby=ID --order=ASC --number=1",
		"option list --search=cloudground_login_* --field=option_name",
		"option delete " + earlier,
		// Keyed by the hash: the option name is the table's unique index,
		// so the connector finds the row with one index lookup.
		"option add cloudground_login_" + hex.EncodeToString(sum[:]) + " 3:1800000300 --autoload=no",
	}
	if got := w.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wp-cli calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(w.lines(), " "), token) {
		t.Error("the token itself reached wp-cli's argv")
	}
	for _, c := range w.calls {
		if c.User != site.SystemUser || c.Path != site.RootPath+"/current" {
			t.Errorf("wp-cli ran as %q in %q", c.User, c.Path)
		}
	}

	// No earlier link: nothing to delete.
	w2 := &fakeWP{respond: func(args []string) (string, error) {
		if args[0] == "user" {
			return "3\n", nil
		}
		return "", nil
	}}
	ops.WP = w2
	if _, err := ops.MagicLogin(context.Background(), rpc.WPParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	for _, l := range w2.lines() {
		if strings.HasPrefix(l, "option delete") {
			t.Errorf("deleted with nothing to delete: %q", l)
		}
	}

	ops.WP = &fakeWP{respond: func([]string) (string, error) { return "", nil }}
	if _, err := ops.MagicLogin(context.Background(), rpc.WPParams{Site: site}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Errorf("no administrator: %v", err)
	}
	ops.WP = &fakeWP{respond: func(args []string) (string, error) {
		if args[0] == "user" {
			return "3\n", nil
		}
		if args[0] == "option" && args[1] == "add" {
			return "", errors.New("could not add option")
		}
		return "", nil
	}}
	if _, err := ops.MagicLogin(context.Background(), rpc.WPParams{Site: site}); err == nil {
		t.Error("a link was issued without its row")
	}
}

func TestPluginsFailLoudly(t *testing.T) {
	w := &fakeWP{respond: func(args []string) (string, error) {
		return "Notice: something\n[{\"name\":\"akismet\",\"status\":\"inactive\",\"version\":\"5.3\",\"update\":\"available\"}]", nil
	}}
	ops := newOps(t, w, &fakePHP{})
	res, err := ops.Plugins(context.Background(), rpc.WPParams{Site: wpSite("/srv/sites/x")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plugins) != 1 || res.Plugins[0] != (rpc.WPPlugin{Name: "akismet", Status: "inactive", Version: "5.3", Update: "available"}) || len(res.Themes) != 1 {
		t.Errorf("result %+v", res)
	}
	w.respond = func(args []string) (string, error) {
		if args[0] == "theme" {
			return "", errors.New("exit 255")
		}
		return "[]", nil
	}
	if _, err := ops.Plugins(context.Background(), rpc.WPParams{Site: wpSite("/srv/sites/x")}); err == nil {
		t.Error("a failing theme list was reported as an empty one")
	}
	w.respond = func([]string) (string, error) { return "PHP Fatal error", nil }
	if _, err := ops.Plugins(context.Background(), rpc.WPParams{Site: wpSite("/srv/sites/x")}); err == nil {
		t.Error("output that is not JSON was reported as an empty list")
	}
}

// TestParseListRealWPCLI: the shapes wp-cli 2.11/2.12 print for
// `plugin list` and `theme list --format=json --fields=name,status,version,update`.
// A must-use plugin and a drop-in carry "update": false (the bool), which
// failed the whole list with a 502 on every WordPress site, since every
// site has the connector mu-plugin.
func TestParseListRealWPCLI(t *testing.T) {
	plugins := `[{"name":"akismet","status":"inactive","version":"5.4","update":"available"},` +
		`{"name":"hello","status":"inactive","version":"1.7.2","update":"none"},` +
		`{"name":"woocommerce","status":"active","version":"10.2.1","update":"unavailable"},` +
		`{"name":"old-thing","status":"inactive","version":"9.9","update":"version higher than expected"},` +
		`{"name":"cloudground","status":"must-use","version":"1.0.0","update":false},` +
		`{"name":"object-cache.php","status":"dropin","version":"","update":false}]`
	got, err := parseList([]byte(plugins))
	if err != nil {
		t.Fatal(err)
	}
	want := []rpc.WPPlugin{
		{Name: "akismet", Status: "inactive", Version: "5.4", Update: "available"},
		{Name: "hello", Status: "inactive", Version: "1.7.2", Update: "none"},
		{Name: "woocommerce", Status: "active", Version: "10.2.1", Update: "unavailable"},
		{Name: "old-thing", Status: "inactive", Version: "9.9", Update: "version higher than expected"},
		{Name: "cloudground", Status: "must-use", Version: "1.0.0", Update: "none"},
		{Name: "object-cache.php", Status: "dropin", Version: "", Update: "none"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("plugins\n got %+v\nwant %+v", got, want)
	}
	themes := `[{"name":"twentytwentyfive","status":"active","version":"1.3","update":"none"},` +
		`{"name":"child","status":"inactive","version":2,"update":true},{"name":"bare","status":"inactive"}]`
	got, err = parseList([]byte(themes))
	if err != nil {
		t.Fatal(err)
	}
	want = []rpc.WPPlugin{
		{Name: "twentytwentyfive", Status: "active", Version: "1.3", Update: "none"},
		{Name: "child", Status: "inactive", Version: "2", Update: "available"},
		{Name: "bare", Status: "inactive", Version: "", Update: "none"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("themes\n got %+v\nwant %+v", got, want)
	}
	if _, err := parseList([]byte(`[{"name":"x","update":{"a":1}}]`)); err == nil {
		t.Error("an update field that is neither a string nor a bool was accepted")
	}
}

func TestUpdate(t *testing.T) {
	w := &fakeWP{respond: func(args []string) (string, error) {
		if strings.Join(args, " ") == "core update" {
			return "", errors.New("exit 1")
		}
		return "", nil
	}}
	php := &fakePHP{}
	ops := newOps(t, w, php)
	site := wpSite("/srv/sites/x")
	if _, err := ops.Update(context.Background(), rpc.WPParams{Site: site, What: "all"}); err == nil {
		t.Error("a failed core update was reported as success")
	}
	got := strings.Join(w.lines(), "|")
	for _, want := range []string{"core update-db", "plugin update --all", "theme update --all"} {
		if !strings.Contains(got, want) {
			t.Errorf("all: %q did not run after a failed core update (%s)", want, got)
		}
	}
	// The object cache is flushed (through wp-cli) and PHP reloaded, also
	// after a partial update.
	if !strings.HasSuffix(got, "|cache flush --skip-plugins --skip-themes") || php.got() != "reload "+domain {
		t.Errorf("caches after a partial update: %s; PHP %q", got, php.got())
	}

	w.calls = nil
	if _, err := ops.Update(context.Background(), rpc.WPParams{Site: site, What: "core"}); err == nil {
		t.Error("core: failure not reported")
	}
	if got := w.lines(); len(got) != 2 || got[0] != "core update" || got[1] != "cache flush --skip-plugins --skip-themes" {
		t.Errorf("core: update-db ran after a failed core update: %v", got)
	}

	if _, err := ops.Update(context.Background(), rpc.WPParams{Site: site, What: "everything"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("unknown target: %v", err)
	}
}

func TestSearchReplace(t *testing.T) {
	w := &fakeWP{}
	ops := newOps(t, w, &fakePHP{})
	site := wpSite("/srv/sites/x")
	if err := ops.SearchReplace(context.Background(), site, "https://old.test", "https://"+domain); err != nil {
		t.Fatal(err)
	}
	if got := w.lines(); len(got) != 1 || got[0] != "search-replace https://old.test https://"+domain+" --all-tables-with-prefix --precise --skip-columns=guid" {
		t.Errorf("wp-cli %v", got)
	}
	for _, bad := range [][2]string{{"", "x"}, {"x", ""}, {"x", "x"}, {"--url=x", "y"}, {"x", "--y"}} {
		if err := ops.SearchReplace(context.Background(), site, bad[0], bad[1]); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("SearchReplace(%q, %q): %v", bad[0], bad[1], err)
		}
	}
}

// The table prefix is read from shared/wp-config.php as text, never by
// running it, and rewritten in one rename keeping the file's mode; nothing
// but a prefix WordPress accepts is ever written.
func TestTablePrefix(t *testing.T) {
	ops := newOps(t, &fakeWP{}, &fakePHP{})
	root := siteTree(t)
	site := wpSite(root)
	cfg := filepath.Join(root, "shared/wp-config.php")
	const config = "<?php\ndefine( 'DB_NAME', 'site_7' );\n/**\n * Database table prefix.\n */\n$table_prefix = 'wp_';\n\nrequire_once ABSPATH . 'wp-settings.php';\n"
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := ops.TablePrefix(context.Background(), site); err != nil || p != "wp_" {
		t.Fatalf("TablePrefix = %q, %v", p, err)
	}
	if err := ops.SetTablePrefix(context.Background(), site, "xyz_"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	if string(b) != strings.Replace(config, "'wp_'", "'xyz_'", 1) {
		t.Fatalf("wp-config.php after the change:\n%s", b)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	for _, bad := range []string{"", "x'; system('id'); $a='", "wp-", "--raw", "a b", strings.Repeat("a", 65)} {
		if err := ops.SetTablePrefix(context.Background(), site, bad); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("SetTablePrefix(%q): %v", bad, err)
		}
	}
	if b2, _ := os.ReadFile(cfg); string(b2) != string(b) {
		t.Fatal("an invalid prefix changed wp-config.php")
	}
}

func TestParseTablePrefix(t *testing.T) {
	for conf, want := range map[string]string{
		"<?php\n$table_prefix = 'wp_';\n":                            "wp_",
		"<?php\n  $table_prefix=\"Ab9_\" ;\n":                        "Ab9_",
		"<?php\n// $table_prefix = 'old_';\n$table_prefix = 'x_';\n": "x_",
	} {
		if got, err := ParseTablePrefix([]byte(conf)); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", conf, got, err, want)
		}
	}
	for _, conf := range []string{
		"<?php\n",
		"<?php\n$table_prefix = 'a_';\n$table_prefix = 'b_';\n",
		"<?php\n$table_prefix = getenv('P');\n",
		"<?php\n$table_prefix = 'a_' . 'b_';\n",
		"<?php\n$table_prefix = 'a_\";\n",
		"<?php\n$table_prefix = '';\n",
		"<?php\n$table_prefix = 'wp-';\n",
	} {
		if got, err := ParseTablePrefix([]byte(conf)); err == nil {
			t.Errorf("%q read as %q", conf, got)
		}
	}
}

// ---------------------------------------------------------- object cache --

func readEpoch(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, sharedEpoch))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestObjectCacheOnOff(t *testing.T) {
	root := siteTree(t)
	php := &fakePHP{}
	ops := newOps(t, &fakeWP{}, php)
	site := wpSite(root)
	link := filepath.Join(root, "releases/r1", DropinLink)

	res, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: site, Enable: true})
	if err != nil {
		t.Fatal(err)
	}
	if res["object_cache"] != "enabled" || res["backend"] != "apcu" {
		t.Errorf("result %v", res)
	}
	b, err := os.ReadFile(filepath.Join(root, sharedDropin))
	if err != nil || string(b) != connector.ObjectCacheAPCu {
		t.Fatalf("shared drop-in: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != DropinTarget {
		t.Fatalf("drop-in link: %q %v", target, err)
	}
	first := readEpoch(t, root)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(first) {
		t.Fatalf("epoch %q", first)
	}

	// On again: the warm cache is kept.
	if _, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: site, Enable: true}); err != nil {
		t.Fatal(err)
	}
	if readEpoch(t, root) != first {
		t.Error("enabling an enabled cache dropped its namespace")
	}

	if res, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: site}); err != nil || res["object_cache"] != "disabled" {
		t.Fatalf("disable: %v %v", res, err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("the drop-in is still linked after disable")
	}

	// Back on after time off: APCu may hold entries from before the database
	// changed, so the namespace must be new.
	if _, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: site, Enable: true}); err != nil {
		t.Fatal(err)
	}
	if readEpoch(t, root) == first {
		t.Error("re-enabling kept the old namespace: APCu would serve entries from before the cache was off")
	}
	if want := strings.TrimSuffix(strings.Repeat("reload "+domain+",", 4), ","); php.got() != want {
		t.Errorf("PHP: %q, want a reload per change", php.got())
	}

	if _, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: site, Enable: true, What: "redis"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("redis: %v", err)
	}
}

// A drop-in name the site planted as a symlink is replaced, not written
// through.
func TestObjectCacheDoesNotFollowPlantedLinks(t *testing.T) {
	root := siteTree(t)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, sharedDropin)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, sharedEpoch)); err != nil {
		t.Fatal(err)
	}
	ops := newOps(t, &fakeWP{}, &fakePHP{})
	if _, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: wpSite(root), Enable: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatal("a file outside the site was written through a planted link")
	}

	// wp-content replaced by a symlink: refused.
	rel := filepath.Join(root, "releases/r1")
	os.RemoveAll(filepath.Join(rel, "wp-content"))
	if err := os.Symlink(t.TempDir(), filepath.Join(rel, "wp-content")); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: wpSite(root), Enable: true}); err == nil {
		t.Error("a symlinked wp-content was followed")
	}
}

// ---------------------------------------------- refresh and cache stats --

// connectorStub plays the site's connector for the statistics.
func connectorStub(t *testing.T, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != domain || r.UserAgent() != agentUserAgent {
			t.Errorf("connector request host %q agent %q", r.Host, r.UserAgent())
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cloudground_stats") != "1" {
			t.Errorf("unexpected connector request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-CloudGround-Token") != token {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
			return
		}
		w.Write([]byte(`{"hits":90,"misses":10,"entries":42,"mem_used":1000,"mem_total":4000}`))
	})
}

func tokenWP(token string) *fakeWP {
	return &fakeWP{respond: func(args []string) (string, error) {
		if strings.Join(args, " ") == "config get CLOUDGROUND_SITE_TOKEN" {
			return token + "\n", nil
		}
		return "", nil
	}}
}

// Refresh flushes through wp-cli: the drop-in moves its generation, which
// PHP-FPM reads on its next request (connector/SPEC.md §3.4). Nothing is
// requested from the site any more.
func TestRefresh(t *testing.T) {
	flushOK := func([]string) (string, error) { return "Success: The cache was flushed.", nil }
	flushFails := func([]string) (string, error) {
		return "", errors.New("Error: The object cache could not be flushed.")
	}
	const flush = "cache flush --skip-plugins --skip-themes"
	for _, tc := range []struct {
		name    string
		site    func(state.Site) state.Site
		respond func([]string) (string, error)
		want    string
		calls   string
	}{
		{"flushed, then reloaded", func(s state.Site) state.Site { return s }, flushOK, "reload " + domain, flush},
		{"flush failed: restart", func(s state.Site) state.Site { return s }, flushFails, "restart " + domain, flush},
		{"not WordPress: restart", func(s state.Site) state.Site { s.Type = state.SitePHP; return s }, flushOK, "restart " + domain, ""},
		{"no PHP: nothing", func(s state.Site) state.Site { s.PHPVersion = ""; return s }, flushOK, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			php := &fakePHP{}
			w := &fakeWP{respond: tc.respond}
			ops := newOps(t, w, php)
			ops.LoopbackAddr = "127.0.0.1:1" // nothing may be requested
			if err := ops.Refresh(context.Background(), tc.site(wpSite("/srv/sites/x"))); err != nil {
				t.Fatal(err)
			}
			if php.got() != tc.want {
				t.Errorf("PHP %q, want %q", php.got(), tc.want)
			}
			if got := strings.Join(w.lines(), ";"); got != tc.calls {
				t.Errorf("wp-cli %q, want %q", got, tc.calls)
			}
		})
	}
}

func TestCacheStatsWriteNothingIntoTheSite(t *testing.T) {
	root := siteTree(t)
	ops := newOps(t, tokenWP("tok"), &fakePHP{})
	ops.LoopbackAddr = serve(t, connectorStub(t, "tok"))
	site := wpSite(root)

	res, err := ops.CacheStats(context.Background(), rpc.WPParams{Site: site})
	if err != nil || res.Backend != "none" {
		t.Fatalf("no drop-in: %+v %v", res, err)
	}
	if _, err := ops.ObjectCache(context.Background(), rpc.WPParams{Site: site, Enable: true}); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, root)
	res, err = ops.CacheStats(context.Background(), rpc.WPParams{Site: site})
	if err != nil {
		t.Fatal(err)
	}
	want := rpc.CacheStatsResult{Backend: "apcu", Hits: 90, Misses: 10, Entries: 42, MemUsed: 1000, MemTotal: 4000, HitRatePct: 90}
	if res != want {
		t.Errorf("stats %+v, want %+v", res, want)
	}
	if after := listTree(t, root); after != before {
		t.Errorf("CacheStats changed the site tree:\n%s\n---\n%s", before, after)
	}

	ops.WP = tokenWP("wrong")
	if _, err := ops.CacheStats(context.Background(), rpc.WPParams{Site: site}); err == nil {
		t.Error("a refused stats request was reported as numbers")
	}
}

func listTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			b.WriteString(p + "\n")
		}
		return nil
	})
	return b.String()
}
