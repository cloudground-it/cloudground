package guard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Endpoint is one side of a measurement: a virtual host on the target.
type Endpoint struct {
	Host string
	// ProbeKey unlocks the connector's metrics footer (ProbeKeyFor).
	ProbeKey string
	// InternalKey is a staging site's password header (state.Site.InternalKey).
	InternalKey string
}

// ProbeKeyFor is the probe header value a site's connector accepts
// (SPEC.md §7).
func ProbeKeyFor(connectorToken string) string {
	if connectorToken == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(connectorToken))
	io.WriteString(mac, "guard-probe")
	return hex.EncodeToString(mac.Sum(nil))
}

// MaxProbePaths bounds the paths one comparison may measure.
const MaxProbePaths = 10

// DefaultProbePaths are measured when the caller names none.
var DefaultProbePaths = []string{"/", "/?cloudground-probe=uncached"}

// ValidateProbePaths accepts only plain paths on the site (SPEC.md §1.1).
func ValidateProbePaths(paths []string) error {
	if len(paths) > MaxProbePaths {
		return fmt.Errorf("at most %d probe paths", MaxProbePaths)
	}
	for _, p := range paths {
		plain := len(p) <= 512 && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") &&
			!strings.ContainsAny(p, "@\\ \t\r\n")
		if !plain {
			return fmt.Errorf("probe path %q must be a plain path such as /shop/", p)
		}
		if _, err := url.ParseRequestURI(p); err != nil {
			return fmt.Errorf("probe path %q: %v", p, err)
		}
	}
	return nil
}

// requestURL builds the URL from the target's scheme and host and the path's
// own path and query, so nothing in the path can change the host.
func requestURL(target, path, nonce string) (string, error) {
	t, err := url.Parse(target)
	if err != nil || t.Host == "" || (t.Scheme != "http" && t.Scheme != "https") {
		return "", fmt.Errorf("invalid probe target %q", target)
	}
	p, err := url.ParseRequestURI(path)
	if err != nil || p.Host != "" || p.User != nil || !strings.HasPrefix(p.Path, "/") || strings.HasPrefix(p.Path, "//") {
		return "", fmt.Errorf("invalid probe path %q", path)
	}
	q := p.RawQuery
	if nonce != "" {
		if q != "" {
			q += "&"
		}
		q += "cg-guard=" + nonce
	}
	u := url.URL{Scheme: t.Scheme, Host: t.Host, Path: p.Path, RawPath: p.RawPath, RawQuery: q}
	return u.String(), nil
}

// sharedClient is the one client every measurement uses (SPEC.md §2).
var sharedClient = &http.Client{
	Timeout: 60 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // the server itself over loopback
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
	},
}

// maxBody bounds what one response may make the panel read.
const maxBody = 4 << 20

// footerRe matches the connector's metrics footer (connector/SPEC.md §2.7).
var footerRe = regexp.MustCompile(`<!-- cloudground queries=(\d+) time=(\d+(?:\.\d+)?) mem=(\d+) -->`)

// observation is what one response yields (SPEC.md §2).
type observation struct {
	status   int
	failed   bool
	cached   bool
	serverMs float64 // 0 without the footer
	clientMs float64
	queries  float64
	mem      int64
	footer   bool
}

// fetch performs one measurement request.
func fetch(ctx context.Context, client *http.Client, target string, ep Endpoint, path, nonce string) observation {
	u, err := requestURL(target, path, nonce)
	if err != nil {
		return observation{failed: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return observation{failed: true}
	}
	req.Host = ep.Host
	if ep.ProbeKey != "" {
		req.Header.Set("X-CloudGround-Probe", ep.ProbeKey)
	}
	if ep.InternalKey != "" {
		req.Header.Set("X-CloudGround-Internal", ep.InternalKey)
	}
	began := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return observation{failed: true}
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	o := observation{
		status:   resp.StatusCode,
		clientMs: float64(time.Since(began).Microseconds()) / 1000,
		cached:   servedFromCache(resp.Header.Get("X-CloudGround-Cache")),
		failed:   resp.StatusCode >= 500 || (readErr != nil && !errors.Is(readErr, io.EOF)),
	}
	// The last footer: wp_footer prints last, and anything printed before it
	// (a post's content on staging, say) could carry a fake one.
	if all := footerRe.FindAllSubmatch(body, -1); len(all) > 0 {
		m := all[len(all)-1]
		secs, _ := strconv.ParseFloat(string(m[2]), 64)
		o.footer = true
		o.serverMs = secs * 1000
		o.queries, _ = strconv.ParseFloat(string(m[1]), 64)
		o.mem, _ = strconv.ParseInt(string(m[3]), 10, 64)
	}
	return o
}

// servedFromCache reads nginx's $upstream_cache_status: only a response PHP
// rendered for this request (MISS, BYPASS, or no cache in front) is
// measured; HIT, STALE, UPDATING, REVALIDATED, EXPIRED and anything else are
// discarded.
func servedFromCache(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "", "MISS", "BYPASS":
		return false
	}
	return true
}
