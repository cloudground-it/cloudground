package api

import (
	"net/http"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's routing block, the "Vhost" tab (internal/engine SPEC §7, R-5.4).
// Both rows are an administrator's, from a session: the block is nginx
// configuration, and whoever writes it decides what the site serves.

// routingBody bounds the request: the block's own limit plus JSON escaping.
const routingBody = 2*nginx.MaxRoutingBytes + 1<<10

// routingView is what the tab shows.
type routingView struct {
	Routing  string `json:"routing"`
	Default  string `json:"default"`
	Custom   bool   `json:"custom"`
	Editable bool   `json:"editable"`
	Before   string `json:"before"`
	After    string `json:"after"`
}

func viewOf(v rpc.RoutingView) routingView {
	routing := v.Current
	if routing == "" {
		routing = v.Default
	}
	return routingView{Routing: routing, Default: v.Default, Custom: v.Current != "", Editable: v.Default != "",
		Before: v.Before, After: v.After}
}

// routingContext is the panel's quick check of a block: the fixed rules,
// without this machine's listening ports, addresses or name resolution.
func routingContext(s state.Site) nginx.RoutingContext {
	rc := nginx.RoutingContext{SiteRoot: s.RootPath, Domains: append([]string{s.Domain}, s.Aliases...)}
	if s.Type != state.SiteProxy {
		rc.DocRoot = filepath.Join(s.RootPath, "current", s.Config.PublicRoot)
	}
	return rc
}

// handleGetRouting is the site's routing block, the type's default and the
// panel's part of the vhost around it.
func (s *Server) handleGetRouting(w http.ResponseWriter, r *http.Request, c *call) {
	var v rpc.RoutingView
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SiteRouting, rpc.SiteRef{Site: c.Site}, &v); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, viewOf(v))
}

// handleSetRouting replaces the block: {"routing": "..."}; "" restores the
// default. The agent checks it, renders it and has nginx test it; it is
// stored only once it is live, and a refusal leaves the site as it was.
func (s *Server) handleSetRouting(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Routing *string `json:"routing"`
	}
	if err := decode(r, &req); err != nil || req.Routing == nil {
		respondErr(w, http.StatusBadRequest, "routing required")
		return
	}
	text := nginx.NormalizeRouting(*req.Routing)
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if text != "" {
		// The agent checks again with this machine's ports, addresses and
		// names; this answers without a render.
		if err := nginx.CheckRouting(text, routingContext(cur)); err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var res rpc.RoutingResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SetSiteRouting, rpc.RoutingParams{Site: cur, Routing: text}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	site, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.Routing = res.Routing
		return nil
	})
	s.logIfErr(err, "MutateSite")
	if err == nil {
		s.recordRendered(site, res.Files)
	}
	detail := "default"
	if res.Routing != "" {
		detail = "custom"
	}
	s.audit(c.Actor(), "site.routing", cur.Domain, detail)
	var v rpc.RoutingView
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SiteRouting, rpc.SiteRef{Site: site}, &v); err != nil {
		// Applied and stored; only the fresh view is missing.
		respond(w, http.StatusOK, routingView{Routing: res.Routing, Custom: res.Routing != "", Editable: true})
		return
	}
	respond(w, http.StatusOK, viewOf(v))
}
