package api

import (
	"fmt"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The WordPress tools on a site's primary database (SPEC.md §6.4): the
// autoloaded options, a serialized-safe search and replace, the expired
// transients. They run wp-cli as the site's user, which is the panel's to
// allow, so they stay panel routes (/api/sites/{id}/quarry/wp/…), not DB
// Studio's own: a database user signed in with its password reaches only
// what its grants give it.

func (s *Server) handleDBStudioAutoload(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	if !managedDB(w, site) {
		return
	}
	var res rpc.DBStudioAutoloadResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioWPAutoload, rpc.DBStudioSiteParams{Site: site}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleDBStudioSearchReplace(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	if !managedDB(w, site) {
		return
	}
	var req struct {
		From   string `json:"from"`
		To     string `json:"to"`
		DryRun bool   `json:"dry_run"`
	}
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !req.DryRun {
		s.audit(c.Actor(), "quarry.search_replace", site.Domain, fmt.Sprintf("from %d to %d characters", len(req.From), len(req.To)))
	}
	var res rpc.DBStudioSearchReplaceResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioWPSearchReplace, rpc.DBStudioSearchReplaceParams{
		Site: site, From: req.From, To: req.To, DryRun: req.DryRun, ReadOnly: c.ReadOnly(),
	}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleDBStudioTransients(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	if !managedDB(w, site) {
		return
	}
	var req struct {
		DryRun bool `json:"dry_run"`
	}
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !req.DryRun {
		s.audit(c.Actor(), "quarry.transients", site.Domain, "delete expired")
	}
	var res rpc.DBStudioTransientsResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioWPTransients, rpc.DBStudioTransientsParams{
		Site: site, DryRun: req.DryRun, ReadOnly: c.ReadOnly(),
	}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}
