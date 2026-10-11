package wp

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	defaultWarmURLs = 200
	maxWarmURLs     = 500
	warmTimeout     = 30 * time.Second
	maxSitemap      = 8 << 20
	maxChildMaps    = 50
	maxPageDrain    = 32 << 20
)

// sitemaps are tried in this order until one yields pages: WordPress core,
// the common plugins, then core's plain-permalink form.
var sitemaps = []string{"/wp-sitemap.xml", "/sitemap_index.xml", "/sitemap.xml", "/?sitemap=index"}

// WarmCache fills a site's page cache before visitors do: one GET per page,
// from a worker pool (SPEC §10).
func (o *Ops) WarmCache(ctx context.Context, p rpc.WarmParams) (rpc.WarmResult, error) {
	limit := p.MaxURLs
	if limit <= 0 || limit > maxWarmURLs {
		limit = defaultWarmURLs
	}
	workers := o.warmWorkers()
	c := o.client(workers, warmTimeout)
	defer c.CloseIdleConnections()

	var urls []string
	if isWordPress(p.Site.Type) {
		var err error
		if urls, err = o.wordpressURLs(ctx, p.Site, limit); err != nil {
			o.log().Warn("cache warmer: wp-cli gave no URL list; trying the sitemap", "site", p.Site.Domain, "err", err)
		}
	}
	paths := warmPaths(urls, p.Site.Domain, limit)
	if len(paths) == 1 {
		paths = warmPaths(o.sitemapURLs(ctx, c, p.Site, limit), p.Site.Domain, limit)
	}
	res := warm(ctx, c, p.Site, paths, workers)
	return res, ctx.Err()
}

func (o *Ops) warmWorkers() int {
	if o.WarmWorkers > 0 {
		return o.WarmWorkers
	}
	return min(max(2*runtime.NumCPU(), 2), 8)
}

// wordpressURLs lists the permalinks of published posts, pages and
// WooCommerce products.
func (o *Ops) wordpressURLs(ctx context.Context, s state.Site, limit int) ([]string, error) {
	out, err := o.Run(ctx, s, "post", "list", "--post_type=post,page,product", "--post_status=publish",
		"--field=url", "--posts_per_page="+strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// warmPaths turns URLs into the request paths to warm: "/" first, then each
// URL that names this site, once, at most limit after "/".
func warmPaths(urls []string, domain string, limit int) []string {
	out := []string{"/"}
	seen := map[string]bool{"/": true}
	for _, u := range urls {
		if len(out) > limit {
			break
		}
		p, ok := ownPath(u, domain)
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ownPath returns the path and query of a URL that names this site: a path,
// or https://<domain>[:443]/… with no user info (SPEC §10.2). The URL comes
// from the site (its sitemap, its database), and the warmer runs as root:
// nothing in it decides where a request goes.
func ownPath(raw, domain string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		u, err := url.ParseRequestURI(raw)
		if err != nil || u.Host != "" {
			return "", false
		}
		return u.RequestURI(), true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || !strings.EqualFold(u.Hostname(), domain) {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	return u.RequestURI(), true
}

// warm requests each path once, workers at a time.
func warm(ctx context.Context, c *http.Client, s state.Site, paths []string, workers int) rpc.WarmResult {
	var warmed, cached atomic.Int64
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				got, took := warmOne(ctx, c, s, p)
				if got {
					warmed.Add(1)
				}
				if took {
					cached.Add(1)
				}
			}
		}()
	}
feed:
	for _, p := range paths {
		select {
		case jobs <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return rpc.WarmResult{Warmed: int(warmed.Load()), Cached: int(cached.Load())}
}

// warmOne makes the single request for a page: gzip, the one variant the
// page cache stores (it gunzips for clients without gzip). It reports
// whether a response came back and whether the cache took it.
func warmOne(ctx context.Context, c *http.Client, s state.Site, path string) (got, took bool) {
	req, err := request(ctx, s, path, userAgent)
	if err != nil {
		return false, false
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := c.Do(req)
	if err != nil {
		return false, false
	}
	// Read the page through: the connection is reused, and nginx finishes
	// the cache entry either way.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPageDrain))
	resp.Body.Close()
	return true, cacheTook(resp)
}

// cacheTook reports whether the page cache serves this page from now on: a
// HIT, or a MISS the cache stores under the vhost's rules (cacheable status,
// no cookie set, not marked private or no-store).
func cacheTook(resp *http.Response) bool {
	st, _, _ := strings.Cut(strings.TrimSpace(resp.Header.Get("X-CloudGround-Cache")), " ")
	switch strings.ToUpper(st) {
	case "HIT", "STALE", "UPDATING", "REVALIDATED":
		return true
	case "MISS", "EXPIRED":
	default:
		return false
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound:
	default:
		return false
	}
	if len(resp.Header.Values("Set-Cookie")) > 0 {
		return false
	}
	cc := strings.ToLower(resp.Header.Get("Cache-Control"))
	return !strings.Contains(cc, "no-store") && !strings.Contains(cc, "private")
}

// sitemapDoc is a sitemap or a sitemap index; namespaces are ignored.
type sitemapDoc struct {
	URLs []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

// sitemapURLs collects page URLs from the first sitemap that has any,
// following an index one level deep (SPEC §10.1).
func (o *Ops) sitemapURLs(ctx context.Context, c *http.Client, s state.Site, limit int) []string {
	for _, sm := range sitemaps {
		doc, ok := fetchSitemap(ctx, c, s, sm)
		if !ok {
			continue
		}
		var out []string
		for _, u := range doc.URLs {
			out = append(out, u.Loc)
		}
		for i, child := range doc.Sitemaps {
			if i >= maxChildMaps || len(out) >= limit {
				break
			}
			p, ok := ownPath(child.Loc, s.Domain)
			if !ok {
				continue
			}
			if sub, ok := fetchSitemap(ctx, c, s, p); ok {
				for _, u := range sub.URLs {
					out = append(out, u.Loc)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func fetchSitemap(ctx context.Context, c *http.Client, s state.Site, path string) (sitemapDoc, bool) {
	var doc sitemapDoc
	req, err := request(ctx, s, path, userAgent)
	if err != nil {
		return doc, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return doc, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSitemap))
		return doc, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSitemap+1))
	if err != nil || len(body) > maxSitemap {
		return doc, false
	}
	if xml.NewDecoder(bytes.NewReader(body)).Decode(&doc) != nil {
		return doc, false
	}
	return doc, true
}
