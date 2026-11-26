package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

var (
	testPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 40)...)
	testICO = append([]byte{0, 0, 1, 0, 1, 0}, bytes.Repeat([]byte{9}, 40)...)
	testSVG = []byte("<?xml version=\"1.0\"?>\n<!-- logo -->\n<svg xmlns=\"http://www.w3.org/2000/svg\"><script>alert(1)</script></svg>")
)

// fakeNginx is this server's nginx as the favicon lookup sees it: one TLS
// listener that serves every site by its Host.
type fakeNginx struct {
	mu    sync.Mutex
	pages map[string]func(w http.ResponseWriter, r *http.Request) // host+path
	seen  []string                                                // host+path of every request
	sni   []string
	keys  []string
}

func (f *fakeNginx) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Host+r.URL.RequestURI())
	f.sni = append(f.sni, r.TLS.ServerName)
	f.keys = append(f.keys, r.Header.Get("X-CloudGround-Internal"))
	h := f.pages[r.Host+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func (f *fakeNginx) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.seen
	f.seen = nil
	return out
}

func serve(ctype string, body []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write(body)
	}
}

func redirect(to string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
}

func withFakeNginx(t *testing.T, s *Server, pages map[string]func(http.ResponseWriter, *http.Request)) *fakeNginx {
	t.Helper()
	f := &fakeNginx{pages: pages}
	ts := httptest.NewTLSServer(f)
	t.Cleanup(ts.Close)
	s.favicons.loopback = ts.Listener.Addr().String()
	return f
}

func getFavicon(t *testing.T, c *client, id int64, etag string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/sites/%d/favicon", c.base, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestFaviconFromTheHomePage(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "fav.example.com", state.SiteWordPress, "8.4", 0)
	home := `<!doctype html><html><head>
<LINK rel="icon" href="https://cdn.elsewhere.test/logo.svg">
<link rel=icon href=/small.png sizes=16x16>
<link rel='shortcut icon' href='/wp-content/uploads/icon-192.png?v=1&amp;x=2' sizes='192x192'>
<link rel="apple-touch-icon" href="https://fav.example.com/apple.png">
<link rel="stylesheet" href="/style.css">
</head><body></body></html>`
	nginx := withFakeNginx(t, s, map[string]func(http.ResponseWriter, *http.Request){
		"fav.example.com/":                                serve("text/html", []byte(home)),
		"fav.example.com/apple.png":                       serve("text/html", []byte("<html>not an image</html>")),
		"fav.example.com/wp-content/uploads/icon-192.png": serve("text/plain", testPNG),
		"fav.example.com/small.png":                       serve("image/png", testPNG),
	})

	resp, body := getFavicon(t, admin, site.ID, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, testPNG) {
		t.Fatalf("favicon = %d %q", resp.StatusCode, body)
	}
	h := resp.Header
	if h.Get("Content-Type") != "image/png" || h.Get("Cache-Control") != faviconCacheOK || h.Get("Content-Security-Policy") != faviconCSP || h.Get("ETag") == "" {
		t.Fatalf("headers %v", h)
	}
	// The offsite SVG is never asked for; the apple-touch icon (180) comes
	// before the 192 one and is refused for its bytes; the 16 px one is not
	// needed.
	want := []string{"fav.example.com/", "fav.example.com/apple.png", "fav.example.com/wp-content/uploads/icon-192.png?v=1&x=2"}
	if got := nginx.requests(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("requests %v, want %v", got, want)
	}
	for _, sni := range nginx.sni {
		if sni != "fav.example.com" {
			t.Fatalf("SNI %q", sni)
		}
	}

	// Kept: the next request reads nothing, and a matching ETag gets a 304.
	resp, _ = getFavicon(t, admin, site.ID, h.Get("ETag"))
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation = %d", resp.StatusCode)
	}
	if got := nginx.requests(); len(got) != 0 {
		t.Fatalf("requests after the first lookup: %v", got)
	}
}

func TestFaviconFallsBackToFaviconIco(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "ico.example.com", state.SiteStatic, "", 0)
	nginx := withFakeNginx(t, s, map[string]func(http.ResponseWriter, *http.Request){
		"ico.example.com/":            serve("text/html", []byte("<html><head><title>x</title></head></html>")),
		"ico.example.com/favicon.ico": serve("image/vnd.microsoft.icon", testICO),
	})
	resp, body := getFavicon(t, admin, site.ID, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/x-icon" || !bytes.Equal(body, testICO) {
		t.Fatalf("favicon = %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if got := nginx.requests(); len(got) != 2 {
		t.Fatalf("requests %v", got)
	}
}

func TestFaviconNone(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "wp.example.com", state.SiteWordPress, "8.4", 0)
	// WordPress without a site icon: /favicon.ico redirects to its own logo.
	nginx := withFakeNginx(t, s, map[string]func(http.ResponseWriter, *http.Request){
		"wp.example.com/":            serve("text/html", []byte(`<link rel="icon" href="/evil.png">`)),
		"wp.example.com/evil.png":    serve("image/png", []byte("<html><script>alert(1)</script></html>")),
		"wp.example.com/favicon.ico": redirect("/wp-includes/images/w-logo-blue-white-bg.png"),
		"wp.example.com/wp-includes/images/w-logo-blue-white-bg.png": serve("image/png", testPNG),
	})
	resp, body := getFavicon(t, admin, site.ID, "")
	if resp.StatusCode != http.StatusNoContent || len(body) != 0 || resp.Header.Get("Cache-Control") != faviconCacheMiss {
		t.Fatalf("favicon = %d %v %q", resp.StatusCode, resp.Header, body)
	}
	want := "wp.example.com/ wp.example.com/evil.png wp.example.com/favicon.ico"
	if got := strings.Join(nginx.requests(), " "); got != want {
		t.Fatalf("requests %s, want %s", got, want)
	}
	if resp, _ = getFavicon(t, admin, site.ID, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second = %d", resp.StatusCode)
	}
	if got := nginx.requests(); len(got) != 0 {
		t.Fatalf("none is kept too, yet: %v", got)
	}
}

func TestFaviconRedirectsStayOnTheSite(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "red.example.com", state.SiteStatic, "", 0)
	site, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Aliases = []string{"www.red.example.com"}
		st.Config.StagingAuthHash = "$2y$10$abcdefghijklmnopqrstuv"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	nginx := withFakeNginx(t, s, map[string]func(http.ResponseWriter, *http.Request){
		// The canonical host is the alias.
		"red.example.com/":          redirect("https://www.red.example.com/"),
		"www.red.example.com/":      serve("text/html", []byte(`<link rel="icon" href="/a.svg"><link rel="icon" href="/b.png" sizes="64x64">`)),
		"www.red.example.com/a.svg": redirect("https://evil.test/a.svg"),
		"www.red.example.com/b.png": redirect("http://red.example.com/c.png"),
		"red.example.com/c.png":     serve("image/png", testPNG),
	})
	resp, body := getFavicon(t, admin, site.ID, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, testPNG) {
		t.Fatalf("favicon = %d %q", resp.StatusCode, body)
	}
	want := "red.example.com/ www.red.example.com/ www.red.example.com/a.svg www.red.example.com/b.png red.example.com/c.png"
	if got := strings.Join(nginx.requests(), " "); got != want {
		t.Fatalf("requests %s, want %s", got, want)
	}
	key := site.InternalKey()
	if key == "" {
		t.Fatal("no internal key")
	}
	for _, k := range nginx.keys {
		if k != key {
			t.Fatalf("internal key %q, want %q", k, key)
		}
	}
}

func TestFaviconOnlyForActiveSites(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "busy.example.com", state.SiteStatic, "", 0)
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Status = state.SiteProvisioning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	nginx := withFakeNginx(t, s, map[string]func(http.ResponseWriter, *http.Request){
		"busy.example.com/favicon.ico": serve("image/x-icon", testICO),
	})
	if resp, _ := getFavicon(t, admin, site.ID, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("favicon = %d", resp.StatusCode)
	}
	if got := nginx.requests(); len(got) != 0 {
		t.Fatalf("requests %v", got)
	}
}

func TestFaviconSVGAndLimits(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "svg.example.com", state.SiteStatic, "", 0)
	large := seedActiveSite(t, s, "big.example.com", state.SiteStatic, "", 0)
	big := append(append([]byte{}, testPNG...), bytes.Repeat([]byte{1}, faviconMaxIcon)...)
	withFakeNginx(t, s, map[string]func(http.ResponseWriter, *http.Request){
		"svg.example.com/":        serve("text/html", []byte(`<link rel="icon" href="/png" sizes="any"><link rel="icon" type="image/svg+xml" href="/logo">`)),
		"svg.example.com/logo":    serve("text/html", testSVG),
		"svg.example.com/png":     serve("image/png", testPNG),
		"big.example.com/":        serve("text/html", []byte(`<link rel="icon" href="/big.png">`)),
		"big.example.com/big.png": serve("image/png", big),
	})
	if resp, _ := getFavicon(t, admin, large.ID, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("an icon over %d bytes = %d", faviconMaxIcon, resp.StatusCode)
	}
	resp, body := getFavicon(t, admin, site.ID, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" || !bytes.Equal(body, testSVG) {
		t.Fatalf("favicon = %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP %q", csp)
	}
}

func TestIconType(t *testing.T) {
	for in, want := range map[string]string{
		string(testPNG):                         "image/png",
		string(testICO):                         "image/x-icon",
		"GIF89a......":                          "image/gif",
		"\xff\xd8\xff\xe0....":                  "image/jpeg",
		"RIFF\x10\x00\x00\x00WEBPVP8 ":          "image/webp",
		"\xef\xbb\xbf <svg viewBox='0 0 1 1'/>": "image/svg+xml",
		"<!DOCTYPE svg><svg>":                   "image/svg+xml",
		"<svgx>":                                "",
		"<html><svg></svg></html>":              "",
		"<!-- never closed <svg>":               "",
		"\x00\x00\x01\x00":                      "",
		"":                                      "",
	} {
		if got := iconType([]byte(in)); got != want {
			t.Errorf("iconType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIconCandidatesOrder(t *testing.T) {
	page := `<link rel="icon" href="/16.png" sizes="16x16"><link rel="icon" href="/none.png">
<link rel="icon" href="/32.png" sizes="32x32"><link rel="apple-touch-icon" href="/apple.png">
<link rel="icon" href="/96.png" sizes="96x96 48x48"><link rel="icon" href="/v.svg?x=1"><link href="/no-rel.png">`
	var got []string
	for _, c := range iconCandidates([]byte(page)) {
		got = append(got, c.href)
	}
	want := "/v.svg?x=1 /96.png /apple.png /none.png /32.png /16.png"
	if strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
}

func FuzzLinkTags(f *testing.F) {
	for _, seed := range []string{
		`<link rel="icon" href="/a.png">`, `<link rel='icon`, `<link`, `<LINK REL=ICON HREF=x>`,
		"<link rel=\"icon\" href=\"\xc4\xb0\xc4\xb0\">", `<link =x =>`, `<link/rel=icon/href=/a>`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, page []byte) {
		_ = iconCandidates(page)
		_ = iconType(page)
	})
}
