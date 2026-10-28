package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The connector's purge (SPEC.md §8): a site's WordPress tells the panel
// which of its pages changed. It authenticates with the site's own token,
// and a token purges only its own site: every URL must name one of the
// site's hosts.

// connectorSite finds the site a bearer token belongs to. A right token is
// never limited: every site purges from the same loopback address, so a
// limit on the address would let one site's PHP stop the others' purges.
// The lookup is one indexed read, so a wrong token costs nothing to answer;
// the per-address limit only changes its answer to 429, and every wrong
// token, from any address, counts towards the token-guessing alert.
func (s *Server) connectorSite(r *http.Request) (state.Site, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	why := "missing token"
	if token != "" {
		site, err := s.Store.SiteForConnectorToken(token)
		if err == nil {
			return site, nil
		}
		if !errors.Is(err, state.ErrNotFound) {
			s.logIfErr(err, "SiteForConnectorToken")
		}
		why = "invalid token"
	}
	if s.guesses.wrong(time.Now()) {
		s.kickAlerts()
	}
	if !s.connectorFailLimiter.Allow(clientIP(r)) {
		return state.Site{}, decline(http.StatusTooManyRequests, "too many invalid tokens")
	}
	return state.Site{}, decline(http.StatusUnauthorized, "%s", why)
}

// purgeRequest is what the connector sends: the URLs that changed, or all.
type purgeRequest struct {
	URLs []string `json:"urls"`
	All  bool     `json:"all"`
}

// ownURLs keeps the URLs whose host is the site's domain or an alias.
// Others are dropped, not refused: the connector sends what WordPress
// lists, which can name another host.
func ownURLs(site state.Site, urls []string) []string {
	hosts := map[string]bool{site.Domain: true}
	for _, a := range site.Aliases {
		hosts[a] = true
	}
	var own []string
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && hosts[u.Hostname()] {
			own = append(own, raw)
		}
	}
	return own
}

// purge empties the site's page cache, or only the URLs of it the request
// names.
func (s *Server) purge(ctx context.Context, site state.Site, req purgeRequest) (rpc.PurgeResult, error) {
	params := rpc.PurgeParams{Site: site}
	if !req.All {
		if params.URLs = ownURLs(site, req.URLs); len(params.URLs) == 0 {
			return rpc.PurgeResult{}, nil
		}
	}
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.PurgeCache, params)
	if err == nil && site.Config.Cloudflare.ZoneID != "" {
		// Cloudflare's cache too, without holding WordPress up (SPEC.md §8).
		s.cdnPurges.add(site, params.URLs, req.All, func(site state.Site, urls []string, all bool) {
			s.purgeCloudflare(context.Background(), site, urls, all)
		})
	}
	return res, err
}

// The connector's Cloudflare purges, per site (SPEC.md §8): one runs at a
// time, what arrives meanwhile waits for the next, at least cdnPurgeGap
// later, and more than cdnPurgeMaxURLs waiting become a purge of the site's
// names. A valid token is never limited, so this is what keeps a site's PHP
// from turning its calls into as many requests on the panel's token.
const (
	cdnPurgeGap     = 10 * time.Second
	cdnPurgeMaxURLs = 10 * cfPurgeBatch
)

type cdnPurges struct {
	mu      sync.Mutex
	pending map[int64]*cdnPending // a site's entry exists while its purges run
	gap     time.Duration         // cdnPurgeGap when zero (tests shorten it)
}

// cdnPending is what waits for a site's next purge.
type cdnPending struct {
	site state.Site
	urls []string
	seen map[string]bool
	all  bool
}

// add queues a purge, and starts the site's runs if none is under way.
func (q *cdnPurges) add(site state.Site, urls []string, all bool, run func(state.Site, []string, bool)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = map[int64]*cdnPending{}
	}
	p, running := q.pending[site.ID]
	if !running {
		p = &cdnPending{}
		q.pending[site.ID] = p
		go q.drain(site.ID, run)
	}
	p.site = site
	if all || p.all {
		p.all, p.urls, p.seen = true, nil, nil
		return
	}
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	for _, u := range urls {
		if !p.seen[u] {
			p.seen[u] = true
			p.urls = append(p.urls, u)
		}
	}
	if len(p.urls) > cdnPurgeMaxURLs {
		p.all, p.urls, p.seen = true, nil, nil
	}
}

// drain runs a site's waiting purges until none is left.
func (q *cdnPurges) drain(id int64, run func(state.Site, []string, bool)) {
	gap := q.gap
	if gap == 0 {
		gap = cdnPurgeGap
	}
	for {
		q.mu.Lock()
		p := q.pending[id]
		if !p.all && len(p.urls) == 0 {
			delete(q.pending, id)
			q.mu.Unlock()
			return
		}
		q.pending[id] = &cdnPending{site: p.site}
		q.mu.Unlock()
		run(p.site, p.urls, p.all)
		time.Sleep(gap)
	}
}

func (s *Server) handleConnectorPurge(w http.ResponseWriter, r *http.Request) {
	site, err := s.connectorSite(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req purgeRequest
	if !decodeOr400(w, r, &req) {
		return
	}
	res, err := s.purge(r.Context(), site, req)
	answer(w, http.StatusOK, res, err)
}

// Token guessing: wrong connector tokens from every address together.
// guessThreshold of them inside guessWindow start a burst, which is an
// alert until guessHold has passed without the threshold being met again.
const (
	guessWindow    = 10 * time.Minute
	guessThreshold = 100
	guessHold      = time.Hour
)

// tokenGuesses counts wrong connector tokens, whatever their address.
type tokenGuesses struct {
	mu    sync.Mutex
	hits  []time.Time // the latest, at most guessThreshold of them
	start time.Time   // when the burst began; zero for none
	last  time.Time   // the last time the threshold was met
	count int         // wrong tokens since the burst began
}

// wrong records a wrong token at now and reports whether it began a burst.
func (g *tokenGuesses) wrong(now time.Time) (began bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	kept := g.hits[:0]
	for _, t := range g.hits {
		if now.Sub(t) < guessWindow {
			kept = append(kept, t)
		}
	}
	g.hits = append(kept, now)
	if len(g.hits) > guessThreshold {
		g.hits = g.hits[len(g.hits)-guessThreshold:]
	}
	inBurst := !g.start.IsZero() && now.Sub(g.last) < guessHold
	if len(g.hits) >= guessThreshold {
		if !inBurst {
			g.start, g.count, began = now, len(g.hits)-1, true
		}
		g.last, inBurst = now, true
	}
	if inBurst {
		g.count++
	}
	return began
}

// condition is the alert of the burst under way at now, if any.
func (g *tokenGuesses) condition(now time.Time) (state.Alert, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.start.IsZero() || now.Sub(g.last) >= guessHold {
		return state.Alert{}, false
	}
	n, since := strconv.Itoa(g.count), g.start.UTC().Format("2006-01-02 15:04 UTC")
	return state.Alert{Key: "connector.token_guessing", Level: "error", Fingerprint: g.start.UTC().Format(time.RFC3339Nano),
		Message: n + " invalid connector tokens since " + since + ": something may be guessing the sites' tokens",
		At:      g.start.UTC().Format(time.RFC3339),
		Text:    state.AlertText{Key: "alert.tokenGuessing", Args: []string{n, since}}}, true
}
