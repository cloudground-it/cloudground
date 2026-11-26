package api

// The site's icon (SPEC.md §3.23): GET /api/sites/{id}/favicon reads the
// icon a browser shows for the site through this server's own nginx, checks
// it by its first bytes, and keeps it in memory.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

const (
	faviconLoopback   = "127.0.0.1:443"
	faviconTimeout    = 5 * time.Second
	faviconMaxPage    = 256 << 10
	faviconMaxIcon    = 100 << 10
	faviconTries      = 3
	faviconRedirects  = 3
	faviconParallel   = 4
	faviconTTL        = 6 * time.Hour
	faviconMissTTL    = 30 * time.Minute
	faviconCSP        = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
	faviconCacheOK    = "private, max-age=3600"
	faviconCacheMiss  = "private, max-age=1800"
	faviconGoodSize   = 64
	faviconAppleTouch = 180
)

// favicon is one lookup's answer: an icon, or none (body nil).
type favicon struct {
	body   []byte
	ctype  string
	etag   string
	domain string
	at     time.Time
}

func (f *favicon) fresh(now time.Time) bool {
	ttl := faviconTTL
	if f.body == nil {
		ttl = faviconMissTTL
	}
	return now.Sub(f.at) < ttl
}

// favicons is the panel's memory of the sites' icons and the lookups under
// way: one per site, at most faviconParallel in all.
type favicons struct {
	mu       sync.Mutex
	sites    map[int64]*favicon
	inflight map[int64]chan struct{}
	slots    chan struct{}
	// loopback replaces 127.0.0.1:443 and now the clock (tests).
	loopback string
	now      func() time.Time
}

func (f *favicons) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// get answers the site's icon from memory, or looks it up: a request for a
// site already being looked up waits for that lookup.
func (f *favicons) get(ctx context.Context, site state.Site) *favicon {
	for {
		f.mu.Lock()
		if f.sites == nil {
			f.sites, f.inflight = map[int64]*favicon{}, map[int64]chan struct{}{}
			f.slots = make(chan struct{}, faviconParallel)
		}
		if got := f.sites[site.ID]; got != nil && got.domain == site.Domain && got.fresh(f.clock()) {
			f.mu.Unlock()
			return got
		}
		if wait, busy := f.inflight[site.ID]; busy {
			f.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return &favicon{}
			}
		}
		done := make(chan struct{})
		f.inflight[site.ID] = done
		slots := f.slots
		f.mu.Unlock()

		got := f.run(ctx, slots, site)

		f.mu.Lock()
		if got != nil {
			f.sites[site.ID] = got
		}
		delete(f.inflight, site.ID)
		close(done)
		f.mu.Unlock()
		if got == nil {
			return &favicon{}
		}
		return got
	}
}

// run waits for a free slot as long as the request does, then looks the
// icon up. The lookup outlives the request, so that its answer is kept for
// the next one. It answers nil when the request gave up first.
func (f *favicons) run(ctx context.Context, slots chan struct{}, site state.Site) *favicon {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-slots }()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faviconTimeout)
	defer cancel()
	got := f.lookup(lctx, site)
	got.domain, got.at = site.Domain, f.clock()
	return got
}

// faviconFetch is one site's lookup: its names, and a client that only ever
// dials this server's nginx.
type faviconFetch struct {
	site   state.Site
	names  map[string]bool
	client *http.Client
}

func (f *favicons) lookup(ctx context.Context, site state.Site) *favicon {
	addr := f.loopback
	if addr == "" {
		addr = faviconLoopback
	}
	d := &net.Dialer{Timeout: 2 * time.Second}
	fx := &faviconFetch{
		site:  site,
		names: map[string]bool{strings.ToLower(site.Domain): true},
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "tcp", addr)
				},
				// Our own nginx over loopback, which may still serve the
				// fallback certificate: what matters is that the request
				// never leaves the machine, not the certificate's name.
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
				TLSHandshakeTimeout: 2 * time.Second,
				DisableKeepAlives:   true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	for _, a := range site.Aliases {
		fx.names[strings.ToLower(a)] = true
	}
	home := &url.URL{Scheme: "https", Host: site.Domain, Path: "/"}
	if page, at, ok := fx.get(ctx, home, faviconMaxPage, true); ok {
		page = page[:min(len(page), faviconMaxPage)]
		tried := 0
		for _, c := range iconCandidates(page) {
			if tried == faviconTries {
				break
			}
			ref, err := url.Parse(c.href)
			if err != nil {
				continue
			}
			u := at.ResolveReference(ref)
			if !fx.ours(u) {
				continue
			}
			tried++
			if icon := fx.icon(ctx, u, true); icon != nil {
				return icon
			}
		}
	}
	if icon := fx.icon(ctx, &url.URL{Scheme: "https", Host: site.Domain, Path: "/favicon.ico"}, false); icon != nil {
		return icon
	}
	return &favicon{}
}

// ours reports whether u is an http(s) address on one of the site's names.
func (fx *faviconFetch) ours(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" || u.User != nil {
		return false
	}
	return fx.names[strings.ToLower(u.Hostname())]
}

// get requests u over HTTPS through the loopback and reads at most limit+1
// bytes of a 200's body, so that the caller can tell a longer one. Redirects
// are followed while they stay on the site's names, when follow is set. It
// answers the body and the address it came from.
func (fx *faviconFetch) get(ctx context.Context, u *url.URL, limit int64, follow bool) ([]byte, *url.URL, bool) {
	for hop := 0; ; hop++ {
		target := *u
		target.Scheme, target.Host, target.Fragment = "https", strings.ToLower(u.Hostname()), ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, nil, false
		}
		req.Header.Set("User-Agent", "CloudGround-Favicon/"+version.Version)
		if key := fx.site.InternalKey(); key != "" {
			req.Header.Set("X-CloudGround-Internal", key)
		}
		resp, err := fx.client.Do(req)
		if err != nil {
			return nil, nil, false
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			if err != nil {
				return nil, nil, false
			}
			return body, &target, true
		case resp.StatusCode >= 300 && resp.StatusCode < 400 && follow && hop < faviconRedirects:
			next, err := target.Parse(resp.Header.Get("Location"))
			if err != nil || !fx.ours(next) {
				return nil, nil, false
			}
			u = next
		default:
			return nil, nil, false
		}
	}
}

// icon fetches u and keeps it when its first bytes name an image type.
func (fx *faviconFetch) icon(ctx context.Context, u *url.URL, follow bool) *favicon {
	body, _, ok := fx.get(ctx, u, faviconMaxIcon, follow)
	if !ok || len(body) > faviconMaxIcon {
		return nil
	}
	ctype := iconType(body)
	if ctype == "" {
		return nil
	}
	sum := sha256.Sum256(body)
	return &favicon{body: body, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:12]) + `"`}
}

// iconType names an image by its first bytes, or answers "".
func iconType(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0, 0, 1, 0}) && len(b) > 6:
		return "image/x-icon"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	case isSVG(b):
		return "image/svg+xml"
	}
	return ""
}

// isSVG reports whether b starts with an <svg> element, after a byte-order
// mark, an XML prolog, comments, a doctype and white space.
func isSVG(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	for {
		b = bytes.TrimLeft(b, " \t\r\n")
		var end []byte
		switch {
		case bytes.HasPrefix(b, []byte("<?")):
			end = []byte("?>")
		case bytes.HasPrefix(b, []byte("<!--")):
			end = []byte("-->")
		case bytes.HasPrefix(b, []byte("<!")):
			end = []byte(">")
		default:
			if !bytes.HasPrefix(b, []byte("<svg")) || len(b) < 5 {
				return false
			}
			c := b[4]
			return c == ' ' || c == '>' || c == '\t' || c == '\r' || c == '\n' || c == '/'
		}
		i := bytes.Index(b, end)
		if i < 0 {
			return false
		}
		b = b[i+len(end):]
	}
}

// iconLink is one <link> of the home page that names an icon.
type iconLink struct {
	href string
	svg  bool
	size int // the largest declared side, 0 when none is declared
}

// rank orders the candidates (SPEC.md §3.23, step 2): SVG, then sizes from
// faviconGoodSize up, then no size, then smaller sizes largest first.
func (l iconLink) rank() (int, int) {
	switch {
	case l.svg:
		return 0, 0
	case l.size >= faviconGoodSize:
		return 1, l.size
	case l.size == 0:
		return 2, 0
	}
	return 3, -l.size
}

// iconCandidates lists the icons a page's <link> elements name, best first.
func iconCandidates(page []byte) []iconLink {
	var out []iconLink
	for _, attrs := range linkTags(page) {
		rels := strings.Fields(strings.ToLower(attrs["rel"]))
		icon, apple := slices.Contains(rels, "icon"), slices.Contains(rels, "apple-touch-icon")
		href := strings.TrimSpace(attrs["href"])
		if !icon && !apple || href == "" {
			continue
		}
		l := iconLink{href: href, size: largestSize(attrs["sizes"])}
		path := href
		if i := strings.IndexAny(path, "?#"); i >= 0 {
			path = path[:i]
		}
		l.svg = strings.Contains(strings.ToLower(attrs["type"]), "svg") || strings.HasSuffix(strings.ToLower(path), ".svg")
		if apple && l.size == 0 {
			l.size = faviconAppleTouch
		}
		out = append(out, l)
	}
	slices.SortStableFunc(out, func(a, b iconLink) int {
		ag, av := a.rank()
		bg, bv := b.rank()
		if ag != bg {
			return ag - bg
		}
		return av - bv
	})
	return out
}

// largestSize reads a sizes attribute ("32x32 48x48", "any").
func largestSize(sizes string) int {
	best := 0
	for _, s := range strings.Fields(strings.ToLower(sizes)) {
		w, _, ok := strings.Cut(s, "x")
		if n, err := strconv.Atoi(w); ok && err == nil && n > best {
			best = n
		}
	}
	return best
}

// linkTags reads the attributes of every <link> element of an HTML page.
// It is a scanner, not a parser: enough for the <head> of a real page,
// and it never fails, it only finds fewer links.
func linkTags(page []byte) []map[string]string {
	var out []map[string]string
	lower := asciiLower(page)
	for i := 0; i < len(page); {
		j := bytes.Index(lower[i:], []byte("<link"))
		if j < 0 {
			break
		}
		i += j + len("<link")
		if i >= len(page) || !isSpace(page[i]) && page[i] != '/' && page[i] != '>' {
			continue
		}
		attrs := map[string]string{}
		for i < len(page) && page[i] != '>' {
			for i < len(page) && (isSpace(page[i]) || page[i] == '/') {
				i++
			}
			start := i
			for i < len(page) && !isSpace(page[i]) && page[i] != '=' && page[i] != '>' && page[i] != '/' {
				i++
			}
			name := string(lower[start:i])
			for i < len(page) && isSpace(page[i]) {
				i++
			}
			value := ""
			if i < len(page) && page[i] == '=' {
				i++
				for i < len(page) && isSpace(page[i]) {
					i++
				}
				if i < len(page) && (page[i] == '"' || page[i] == '\'') {
					q := page[i]
					i++
					start := i
					for i < len(page) && page[i] != q {
						i++
					}
					value = string(page[start:i])
					if i < len(page) {
						i++
					}
				} else {
					start := i
					for i < len(page) && !isSpace(page[i]) && page[i] != '>' {
						i++
					}
					value = string(page[start:i])
				}
			}
			if name != "" {
				if _, seen := attrs[name]; !seen {
					attrs[name] = html.UnescapeString(value)
				}
			} else if i < len(page) && page[i] != '>' {
				i++
			}
		}
		out = append(out, attrs)
	}
	return out
}

// asciiLower lowercases A to Z only, so that offsets into it are offsets
// into b (bytes.ToLower may change the length of non-ASCII text).
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// handleSiteFavicon is GET /api/sites/{id}/favicon (SPEC.md §3.23).
func (s *Server) handleSiteFavicon(w http.ResponseWriter, r *http.Request, c *call) {
	h := w.Header()
	if c.Site.Status != state.SiteActive {
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	icon := s.favicons.get(r.Context(), c.Site)
	if icon.body == nil {
		h.Set("Cache-Control", faviconCacheMiss)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.Set("Content-Security-Policy", faviconCSP)
	h.Set("Cache-Control", faviconCacheOK)
	h.Set("ETag", icon.etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && slices.Contains(strings.Split(strings.ReplaceAll(inm, " ", ""), ","), icon.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", icon.ctype)
	h.Set("Content-Length", strconv.Itoa(len(icon.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(icon.body)
}
