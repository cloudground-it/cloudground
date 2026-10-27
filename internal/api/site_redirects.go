package api

import (
	"net/http"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's canonical host and redirect rules (SPEC.md §3.18, issue #83,
// internal/engine SPEC §4.10). Like the maintenance page, a change renders
// the site at once and is stored only once nginx has it. The page cache is
// emptied after it: a redirect is decided before the cache is read, but what
// the cache holds for a name or path that was redirected is out of date once
// the redirect goes.

// redirectsView is what the routes answer.
type redirectsView struct {
	// Canonical is the name every other name is sent to ("" for none).
	Canonical string               `json:"canonical"`
	Rules     []state.RedirectRule `json:"rules"`
	// Names are the site's domain and aliases, the choices for Canonical.
	Names []string `json:"names"`
	// Counterpart is the www name of a bare domain (or the bare name of a www
	// one) and CounterpartServed whether the site already answers to it: if
	// not, it is an alias away, and the certificate is issued again for it
	// with the alias (SPEC §3.4).
	Counterpart       string `json:"counterpart"`
	CounterpartServed bool   `json:"counterpart_served"`
}

func redirectsOf(st state.Site) redirectsView {
	r := st.Config.Redirects
	v := redirectsView{Canonical: r.Canonical, Rules: r.Rules, Names: append([]string{st.Domain}, st.Aliases...)}
	if v.Rules == nil {
		v.Rules = []state.RedirectRule{}
	}
	if strings.HasPrefix(st.Domain, "www.") {
		v.Counterpart = strings.TrimPrefix(st.Domain, "www.")
	} else {
		v.Counterpart = "www." + st.Domain
	}
	for _, n := range v.Names {
		v.CounterpartServed = v.CounterpartServed || n == v.Counterpart
	}
	return v
}

// handleGetRedirects answers the site's canonical host and rules.
func (s *Server) handleGetRedirects(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, redirectsOf(c.Site))
}

// handleSetRedirects changes them (a field left out keeps its value; rules
// replace the whole list).
func (s *Server) handleSetRedirects(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Canonical *string               `json:"canonical"`
		Rules     *[]state.RedirectRule `json:"rules"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	next := cur.Config.Redirects
	if req.Canonical != nil {
		next.Canonical = strings.ToLower(strings.TrimSpace(*req.Canonical))
	}
	if req.Rules != nil {
		next.Rules = *req.Rules
	}
	next = site.NormalizeRedirects(next)
	if err := site.CheckRedirects(next, append([]string{cur.Domain}, cur.Aliases...)); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := cur
	cfg.Config.Redirects = next
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: cfg}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.Redirects = next
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.recordRendered(stored, res.Files)
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: stored}, nil); err != nil {
		s.Log.Warn("could not empty the page cache after a redirect change", "site", stored.Domain, "err", err)
	}
	s.warmInBackground(stored)
	detail := "rules"
	if cur.Config.Redirects.Canonical != next.Canonical {
		detail = "canonical"
		if next.Canonical == "" {
			detail = "canonical off"
		}
	}
	s.audit(c.Actor(), "site.redirects", cur.Domain, detail)
	respond(w, http.StatusOK, redirectsOf(stored))
}
