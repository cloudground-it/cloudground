package wp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	defaultLoopback = "127.0.0.1:443"
	userAgent       = "CloudGround-CacheWarmer/1.0"
	agentUserAgent  = "cloudground-agent"
)

// client requests a site's own pages through this server's nginx, whatever
// the domain resolves to (SPEC §9). conns is how many keep-alive connections
// it keeps.
func (o *Ops) client(conns int, timeout time.Duration) *http.Client {
	addr := o.LoopbackAddr
	if addr == "" {
		addr = defaultLoopback
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "tcp", addr)
			},
			// The vhost may still serve the bootstrap certificate. This is
			// a loopback request to our own nginx: the site token, or the
			// cache entry, is what matters, not the certificate's name.
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			TLSHandshakeTimeout: 10 * time.Second,
			// The Accept-Encoding sent is exactly the one set on the request.
			DisableCompression:  true,
			MaxIdleConns:        conns,
			MaxIdleConnsPerHost: conns,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// request is a GET for path (with its query) on the site's own domain. A
// staging site's internal key lets it past the staging password.
func request(ctx context.Context, s state.Site, path, ua string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s.Domain+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	if key := s.InternalKey(); key != "" {
		req.Header.Set("X-CloudGround-Internal", key)
	}
	return req, nil
}

// siteToken is the connector's shared secret, from the site's wp-config.php.
func (o *Ops) siteToken(ctx context.Context, s state.Site) (string, error) {
	out, err := o.Run(ctx, s, "config", "get", "CLOUDGROUND_SITE_TOKEN")
	if err != nil {
		return "", fmt.Errorf("read the site token: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("the site has no connector token")
	}
	return token, nil
}

// callConnector asks the site's connector, running in one of the site's own
// FPM workers, for an action selected by the query flag, authenticated by
// the site token in header. The query string keeps the request out of the
// page cache. The JSON answer is decoded into out.
func (o *Ops) callConnector(ctx context.Context, s state.Site, flag, header string, out any) error {
	token, err := o.siteToken(ctx, s)
	if err != nil {
		return err
	}
	c := o.client(1, 30*time.Second)
	defer c.CloseIdleConnections()
	req, err := request(ctx, s, "/?"+flag+"=1", agentUserAgent)
	if err != nil {
		return err
	}
	req.Header.Set(header, token)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the connector answered %d: %.200s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("the connector's answer is not JSON (%.200s): %w", strings.TrimSpace(string(body)), err)
	}
	return nil
}
