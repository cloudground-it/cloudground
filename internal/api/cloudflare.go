package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/cfips"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Cloudflare (SPEC.md §3.20, #22): the ranges nginx takes a visitor's
// address from, kept current by a daily job, and the purge of Cloudflare's
// cache that rides on the panel's own purges.

const (
	cloudflareAPIBase = "https://api.cloudflare.com/client/v4"
	cfFetchTimeout    = 10 * time.Second
	cfBodyMax         = 64 << 10
	cfPurgeTimeout    = 15 * time.Second
	// cfPurgeBatch is how many URLs or names go in one purge request: the
	// limit of every plan.
	cfPurgeBatch = 30
)

// cfRangesInUse is the list the panel uses; a zero at is the shipped one.
type cfRangesInUse struct {
	r  cfips.Ranges
	at time.Time
}

// cloudflareRangesInUse is the stored list when it still passes the
// check, else the shipped one; read once, then kept.
func (s *Server) cloudflareRangesInUse() cfRangesInUse {
	s.cfMu.Lock()
	defer s.cfMu.Unlock()
	if s.cfRanges != nil {
		return *s.cfRanges
	}
	in := cfRangesInUse{r: cfips.Shipped()}
	stored, ok, err := s.Store.CloudflareRanges()
	switch {
	case err != nil:
		s.Log.Warn("could not read Cloudflare's ranges: using the shipped list", "err", err)
	case ok:
		if r, err := cfips.Parse(stored.V4, stored.V6); err == nil {
			in = cfRangesInUse{r: r, at: stored.FetchedAt}
		} else {
			s.Log.Warn("the stored Cloudflare ranges do not pass the check: using the shipped list", "err", err)
		}
	}
	s.cfRanges = &in
	return in
}

// cloudflareRanges is the list in use.
func (s *Server) cloudflareRanges() cfips.Ranges { return s.cloudflareRangesInUse().r }

// pushCloudflareRanges sends the list in use to the agent, which rewrites
// its file only when it changed.
func (s *Server) pushCloudflareRanges(ctx context.Context) (bool, error) {
	v4, v6 := s.cloudflareRanges().Strings()
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.SetCloudflareRanges, rpc.CloudflareRangesParams{V4: v4, V6: v6})
	return res.Changed, err
}

func (s *Server) cfAPI() string {
	if s.cloudflareAPI != "" {
		return s.cloudflareAPI
	}
	return cloudflareAPIBase
}

func (s *Server) cfClient() *http.Client {
	if s.cloudflareClient != nil {
		return s.cloudflareClient
	}
	// No redirects: the token goes to api.cloudflare.com and nowhere else.
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// cfEnvelope is the part of every answer of Cloudflare's API we read.
type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

// cfDo sends one request to Cloudflare's API and reads its envelope; a
// failure is Cloudflare's first error message, or the transport's.
func (s *Server) cfDo(req *http.Request) (cfEnvelope, error) {
	resp, err := s.cfClient().Do(req)
	if err != nil {
		return cfEnvelope{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfBodyMax+1))
	if err != nil {
		return cfEnvelope{}, err
	}
	if len(body) > cfBodyMax {
		return cfEnvelope{}, errors.New("Cloudflare's answer is too large")
	}
	var env cfEnvelope
	jerr := json.Unmarshal(body, &env)
	if resp.StatusCode/100 == 2 && jerr == nil && env.Success {
		return env, nil
	}
	if jerr == nil && len(env.Errors) > 0 && env.Errors[0].Message != "" {
		return env, fmt.Errorf("Cloudflare: %s (%d)", env.Errors[0].Message, env.Errors[0].Code)
	}
	return env, fmt.Errorf("Cloudflare answered %s", resp.Status)
}

// fetchCloudflareRanges reads Cloudflare's published ranges and checks them.
func (s *Server) fetchCloudflareRanges(ctx context.Context) (cfips.Ranges, error) {
	ctx, cancel := context.WithTimeout(ctx, cfFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfAPI()+"/ips", nil)
	if err != nil {
		return cfips.Ranges{}, err
	}
	env, err := s.cfDo(req)
	if err != nil {
		return cfips.Ranges{}, err
	}
	var res struct {
		V4 []string `json:"ipv4_cidrs"`
		V6 []string `json:"ipv6_cidrs"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return cfips.Ranges{}, fmt.Errorf("Cloudflare's list of ranges: %w", err)
	}
	return cfips.Parse(res.V4, res.V6)
}

// anyRealIP reports whether a site takes visitor addresses from Cloudflare.
func (s *Server) anyRealIP() (bool, error) {
	sites, err := s.Store.ListSites()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(sites, func(st state.Site) bool { return st.Config.Cloudflare.RealIP }), nil
}

// runCloudflareRanges is the cloudflare.ranges job (SPEC.md §3.20): while a
// site needs them, fetch the ranges, store a good list and give it to the
// agent.
func (s *Server) runCloudflareRanges(ctx context.Context, r *jobs.Run) error {
	need, err := s.anyRealIP()
	if err != nil {
		return err
	}
	if !need {
		r.Progress(100, "no site takes visitor addresses from Cloudflare")
		return nil
	}
	rng, err := s.fetchCloudflareRanges(ctx)
	if err != nil {
		s.Log.Warn("could not refresh Cloudflare's ranges: keeping the list in use", "err", err)
		return fmt.Errorf("refresh Cloudflare's ranges: %w", err)
	}
	now := time.Now().UTC()
	v4, v6 := rng.Strings()
	if err := s.Store.SetCloudflareRanges(state.CloudflareRanges{V4: v4, V6: v6, FetchedAt: now}); err != nil {
		return err
	}
	s.cfMu.Lock()
	s.cfRanges = &cfRangesInUse{r: rng, at: now}
	s.cfMu.Unlock()
	changed, err := s.pushCloudflareRanges(ctx)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("%d IPv4 and %d IPv6 ranges, unchanged", len(v4), len(v6))
	if changed {
		msg = fmt.Sprintf("%d IPv4 and %d IPv6 ranges, nginx updated", len(v4), len(v6))
		s.Log.Info("Cloudflare's ranges changed", "v4", len(v4), "v6", len(v6))
	}
	r.Progress(100, msg)
	return nil
}

// cdnPurge is how a purge of Cloudflare's cache went: "" when the site
// sends none, else "purged" or "failed" with the reason.
type cdnPurge struct {
	CDN      string `json:"cdn,omitempty"`
	CDNError string `json:"cdn_error,omitempty"`
}

// purgeCloudflare empties Cloudflare's cache of the site, after the
// server's (SPEC.md §3.20): every one of its names, or only the URLs on
// them. Nothing without a zone or without the purge token.
func (s *Server) purgeCloudflare(ctx context.Context, site state.Site, urls []string, all bool) cdnPurge {
	zone := site.Config.Cloudflare.ZoneID
	if zone == "" {
		return cdnPurge{}
	}
	token, err := s.Store.Secret(state.SecretCloudflarePurgeToken)
	if err != nil {
		s.logIfErr(err, "Secret")
		return cdnPurge{CDN: "failed", CDNError: "could not read the Cloudflare token"}
	}
	if token == "" {
		return cdnPurge{}
	}
	var key string
	var items []string
	if all {
		key, items = "hosts", append([]string{site.Domain}, site.Aliases...)
	} else {
		key, items = "files", ownURLs(site, urls)
	}
	if len(items) == 0 {
		return cdnPurge{}
	}
	ctx, cancel := context.WithTimeout(ctx, cfPurgeTimeout)
	defer cancel()
	for batch := range slices.Chunk(items, cfPurgeBatch) {
		body, _ := json.Marshal(map[string][]string{key: batch})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfAPI()+"/zones/"+url.PathEscape(zone)+"/purge_cache", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			_, err = s.cfDo(req)
		}
		if err != nil {
			s.Log.Warn("could not purge Cloudflare's cache", "site", site.Domain, "err", err)
			return cdnPurge{CDN: "failed", CDNError: err.Error()}
		}
	}
	return cdnPurge{CDN: "purged"}
}
