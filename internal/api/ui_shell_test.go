package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The UI shell's inline scripts run under a CSP that allows each by its
// hash and nothing more (a JSON data block is not a script); every route,
// the per-route copies nuxt generate leaves included, serves the shell;
// hashed assets are cached for good.
func TestUIShellCSPAndRoutes(t *testing.T) {
	const importMap, boot = `{"imports":{}}`, `window.__NUXT__={}`
	shell := `<html><head><script type="importmap">` + importMap + `</script><script>` + boot + `</script>` +
		`<script type="application/json" data-nuxt-data>[1]</script><script type="module" src="/_nuxt/a.js"></script></head></html>`
	s := &Server{UI: fstest.MapFS{
		"index.html":       {Data: []byte(shell)},
		"audit/index.html": {Data: []byte("stale per-route copy")},
		"_nuxt/a.js":       {Data: []byte("console.log(1)")},
	}}
	handler := s.withCommonHeaders(http.HandlerFunc(s.handleUI))
	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	hashOf := func(script string) string {
		sum := sha256.Sum256([]byte(script))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}

	csp := serve("/").Header().Get("Content-Security-Policy")
	for _, script := range []string{importMap, boot} {
		if !strings.Contains(csp, hashOf(script)) {
			t.Fatalf("CSP %q does not allow %q", csp, script)
		}
	}
	for _, loose := range []string{hashOf("[1]"), "unsafe-eval", "script-src 'self' 'unsafe-inline'"} {
		if strings.Contains(csp, loose) {
			t.Fatalf("CSP %q contains %s", csp, loose)
		}
	}
	for _, path := range []string{"/audit", "/audit/", "/sites/3/staging"} {
		if rec := serve(path); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "importmap") {
			t.Fatalf("%s = %d %q, want the shell", path, rec.Code, rec.Body.String())
		}
	}
	if cc := serve("/_nuxt/a.js").Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("a hashed asset's Cache-Control is %q", cc)
	}
}
