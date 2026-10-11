package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// The web interface (SPEC.md §1.10): a single-page app embedded in the
// binary, and the Content-Security-Policy it runs under.

// handleUI serves the app: the file at the path when there is one, the app
// shell (index.html) for every other path, which the app routes itself.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	// A path under /api/ that no row matches is a client asking for a route
	// that does not exist (one removed, say): it gets an API answer, not
	// the app's page with 200.
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		respondErr(w, http.StatusNotFound, "no such API route: "+r.Method+" "+r.URL.Path)
		return
	}
	if s.UI == nil {
		http.Error(w, "ui not embedded", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if !isUIFile(s.UI, name) {
		// The shell, also for the per-route directories nuxt generate
		// leaves behind.
		name = "index.html"
	}
	cache := "no-cache"
	if strings.HasPrefix(name, "_nuxt/") || strings.HasPrefix(name, "_fonts/") {
		// Content-hashed: a new build writes new names.
		cache = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Cache-Control", cache)
	http.ServeFileFS(w, r, s.UI, name)
}

// isUIFile reports whether name is a regular file of the build.
func isUIFile(ui fs.FS, name string) bool {
	if name == "" {
		return false
	}
	info, err := fs.Stat(ui, name)
	return err == nil && info.Mode().IsRegular()
}

// contentSecurityPolicy lets scripts run from the panel's own origin and,
// pinned by hash, the inline scripts of the app shell (the import map, the
// colour-mode bootstrap, the runtime config). Nothing else inline, nothing
// from anywhere else.
func (s *Server) contentSecurityPolicy() string {
	s.cspOnce.Do(func() {
		scripts := []string{"script-src", "'self'"}
		if s.UI != nil {
			if shell, err := fs.ReadFile(s.UI, "index.html"); err == nil {
				for _, h := range inlineScriptHashes(shell) {
					scripts = append(scripts, "'"+h+"'")
				}
			}
		}
		s.csp = strings.Join([]string{
			"default-src 'self'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'self'",
			"img-src 'self' data:", "font-src 'self'", "style-src 'self' 'unsafe-inline'",
			strings.Join(scripts, " "), "connect-src 'self'", "object-src 'none'",
		}, "; ")
	})
	return s.csp
}

var scriptTagRe = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)

// inlineScriptHashes is the CSP source of every inline script in html a
// browser executes: one with a body and no src; a JSON data block never
// runs, an import map does.
func inlineScriptHashes(html []byte) []string {
	var hashes []string
	for _, m := range scriptTagRe.FindAllSubmatch(html, -1) {
		attrs, body := strings.ToLower(string(m[1])), m[2]
		if len(body) == 0 || strings.Contains(attrs, "src=") || strings.Contains(attrs, "application/json") {
			continue
		}
		sum := sha256.Sum256(body)
		hashes = append(hashes, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	}
	return hashes
}
