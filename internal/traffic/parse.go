// Package traffic builds a site's visitor statistics from its nginx access
// log (#33, SPEC.md): pure, it parses lines and keeps aggregates; it never
// stores an address.
package traffic

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Hit is one parsed line of the cloudground log format (SPEC §1).
type Hit struct {
	Addr   string
	Time   time.Time
	Method string // "" when the request was not METHOD /target PROTO
	Path   string // without the query string; "" with Method
	Status int
	Bytes  int64
	Cache  string // $upstream_cache_status, "-" for none
	UA     string
}

const (
	timeLayout = "02/Jan/2006:15:04:05 -0700"
	maxPath    = 200
)

// ParseLine reads one line (SPEC §1); ok is false for anything that is not
// one.
func ParseLine(line string) (h Hit, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	addr, rest, found := strings.Cut(line, " - ")
	if !found || addr == "" || strings.ContainsAny(addr, " \t") {
		return Hit{}, false
	}
	h.Addr = addr
	// The host, then [time].
	open := strings.Index(rest, " [")
	if open < 0 {
		return Hit{}, false
	}
	rest = rest[open+2:]
	ts, rest, found := strings.Cut(rest, "] \"")
	if !found {
		return Hit{}, false
	}
	t, err := time.Parse(timeLayout, ts)
	if err != nil {
		return Hit{}, false
	}
	h.Time = t
	req, rest, found := strings.Cut(rest, "\" ")
	if !found {
		return Hit{}, false
	}
	h.Method, h.Path = splitRequest(req)
	// status bytes request_time cache:X "UA"
	f := strings.SplitN(rest, " ", 4)
	if len(f) < 4 {
		return Hit{}, false
	}
	st, err := strconv.Atoi(f[0])
	if err != nil || st < 100 || st > 599 {
		return Hit{}, false
	}
	h.Status = st
	n, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || n < 0 {
		return Hit{}, false
	}
	h.Bytes = n
	cache, ua, found := strings.Cut(f[3], " \"")
	if !found || !strings.HasPrefix(cache, "cache:") || !strings.HasSuffix(ua, "\"") {
		return Hit{}, false
	}
	h.Cache = strings.TrimPrefix(cache, "cache:")
	h.UA = strings.TrimSuffix(ua, "\"")
	return h, true
}

var methodRe = regexp.MustCompile(`^[A-Z]{3,10}$`)

// splitRequest is the method and the path of "METHOD /target PROTO".
func splitRequest(req string) (method, path string) {
	f := strings.Fields(req)
	if len(f) < 2 || !methodRe.MatchString(f[0]) || !strings.HasPrefix(f[1], "/") {
		return "", ""
	}
	p := f[1]
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if len(p) > maxPath {
		p = p[:maxPath]
	}
	return f[0], p
}

// Kinds of request (SPEC §2).
const (
	kindInternal = iota
	kindBot
	kindOther
	kindPage
)

var botRe = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|curl|wget|python|go-http-client|java/|libwww|httpclient|okhttp|axios|node-fetch|headless|scrapy|facebookexternalhit|preview|monitor|uptime|lighthouse|feedfetcher`)

var assetExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields("css js mjs map png jpg jpeg gif webp avif svg ico bmp woff woff2 ttf otf eot mp4 webm mp3 ogg wav pdf zip gz xml txt json webmanifest") {
		assetExt[e] = true
	}
}

var (
	notPagePrefixes = []string{"/wp-admin/", "/wp-content/", "/wp-includes/", "/wp-json/", "/.well-known/", "/.cg-maintenance/"}
	notPagePaths    = map[string]bool{"/wp-login.php": true, "/wp-cron.php": true, "/xmlrpc.php": true, "/favicon.ico": true, "/robots.txt": true}
)

// kind classifies a hit (SPEC §2).
func kind(h Hit) int {
	if strings.HasPrefix(h.UA, "CloudGround") {
		return kindInternal
	}
	if h.UA == "" || h.UA == "-" || botRe.MatchString(h.UA) {
		return kindBot
	}
	if h.Method != "GET" || !(h.Status/100 == 2 || h.Status == 304) || !isPagePath(h.Path) {
		return kindOther
	}
	return kindPage
}

func isPagePath(p string) bool {
	if p == "" || notPagePaths[p] {
		return false
	}
	for _, pre := range notPagePrefixes {
		if strings.HasPrefix(p, pre) {
			return false
		}
	}
	last := p[strings.LastIndexByte(p, '/')+1:]
	if i := strings.LastIndexByte(last, '.'); i >= 0 && assetExt[strings.ToLower(last[i+1:])] {
		return false
	}
	return true
}
