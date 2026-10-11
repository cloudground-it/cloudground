package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// WordPress tools and the page cache (SPEC.md §6.2).

// isWordPress reports whether a site of type t runs WordPress.
func isWordPress(t state.SiteType) bool {
	switch t {
	case state.SiteWordPress, state.SiteWooCommerce:
		return true
	}
	return false
}

var errNotWordPress = decline(http.StatusBadRequest, "not a WordPress site")

// wordPress is the WordPress tools' service. Every method but the cache's
// refuses a site that does not run WordPress.
type wordPress struct{ s *Server }

func (s *Server) wp() wordPress { return wordPress{s} }

func (p wordPress) loginLink(ctx context.Context, site state.Site) (rpc.WPLoginResult, error) {
	if !isWordPress(site.Type) {
		return rpc.WPLoginResult{}, errNotWordPress
	}
	return agentrpc.Call(ctx, p.s.Agent, agentrpc.WPMagicLogin, rpc.WPParams{Site: site})
}

func (p wordPress) plugins(ctx context.Context, site state.Site) (rpc.WPPluginsResult, error) {
	if !isWordPress(site.Type) {
		return rpc.WPPluginsResult{}, errNotWordPress
	}
	return agentrpc.Call(ctx, p.s.Agent, agentrpc.WPPlugins, rpc.WPParams{Site: site})
}

// update starts the update of core, plugins, themes or all of them, and
// empties the page cache after it: new code renders new pages.
func (p wordPress) update(site state.Site, what string) (jobs.Task, error) {
	if !isWordPress(site.Type) {
		return jobs.Task{}, errNotWordPress
	}
	agent := p.s.Agent
	return p.s.runJob(jobReq{kind: "wp.update", siteID: site.ID, locks: siteLocks(site.ID)}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		step(30, "Updating "+what+" on "+site.Domain)
		if err := agentrpc.Into(ctx, agent, agentrpc.WPUpdate, rpc.WPParams{Site: site, What: what}, nil); err != nil {
			return err
		}
		_ = agentrpc.Into(ctx, agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: site}, nil) // best effort
		step(100, "Update complete")
		return nil
	})
}

// The object-cache switch is a site setting: object_cache in PATCH
// /api/sites/{id} (site_settings.go).

func (p wordPress) cacheStats(ctx context.Context, site state.Site) (rpc.CacheStatsResult, error) {
	return agentrpc.Call(ctx, p.s.Agent, agentrpc.CacheStats, rpc.WPParams{Site: site})
}

// warm crawls the site's sitemap into the page cache.
func (p wordPress) warm(site state.Site) (jobs.Task, error) {
	agent := p.s.Agent
	return p.s.runJob(jobReq{kind: "cache.warm", siteID: site.ID}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		step(30, "Warming "+site.Domain+" from sitemap")
		res, err := agentrpc.Call(ctx, agent, agentrpc.WarmCache, rpc.WarmParams{Site: site})
		if err != nil {
			return err
		}
		step(100, fmt.Sprintf("Warmed %d pages (%d now cached)", res.Warmed, res.Cached))
		return nil
	})
}

// warmLock lets one background re-warm run at a time, so they take at most
// one of the maintenance slots.
const warmLock = "cache:rewarm"

// warmInBackground queues the quiet re-warm of a site's page cache after a
// change emptied it (a promote, a rollback, a migration). One already
// queued for the site is joined.
func (s *Server) warmInBackground(site state.Site) {
	_, err := s.Jobs.Enqueue(jobs.Spec{
		Kind: "cache.rewarm", SiteID: site.ID, Locks: []string{warmLock}, Background: true,
		DedupKey: "cache.rewarm:" + strconv.FormatInt(site.ID, 10),
		Payload:  sitePayload{SiteID: site.ID},
	})
	if err != nil && s.Log != nil {
		s.Log.Warn("could not queue the cache re-warm", "site", site.Domain, "err", err)
	}
}

// runRewarm is the cache.rewarm job: the site as stored now, if it is
// still there.
func (s *Server) runRewarm(ctx context.Context, r *jobs.Run) error {
	site, err := s.Store.GetSite(r.Job().SiteID)
	if errors.Is(err, state.ErrNotFound) || (err == nil && site.Status != state.SiteActive) {
		return nil
	}
	if err != nil {
		return err
	}
	return agentrpc.Into(ctx, s.Agent, agentrpc.WarmCache, rpc.WarmParams{Site: site}, nil)
}

func (s *Server) handleWPMagicLogin(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.wp().loginLink(r.Context(), c.Site)
	if err == nil {
		s.audit(c.Actor(), "wp.magic_login", c.Site.Domain, "")
	}
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleWPPlugins(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.wp().plugins(r.Context(), c.Site)
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleWPUpdate(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	var req struct {
		What string `json:"what"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	task, err := s.wp().update(c.Site, req.What)
	if err == nil {
		s.audit(c.Actor(), "wp.update", c.Site.Domain, req.What)
	}
	accepted(w, task, err)
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.wp().cacheStats(r.Context(), c.Site)
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleWarmCache(w http.ResponseWriter, r *http.Request, c *call) {
	task, err := s.wp().warm(c.Site)
	if err == nil {
		s.audit(c.Actor(), "cache.warm", c.Site.Domain, "")
	}
	accepted(w, task, err)
}
