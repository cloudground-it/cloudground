package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// WordPress sign-in security and hardening (SPEC.md §6.2.1 and §6.2.2,
// issues #74 and #75). The setting
// lives in the site's PHP master (its pool's environment), so a change
// renders the site at once, as the protected paths do (protected_paths.go):
// it is stored only once the agent has applied it.

// twoFactorReq is the body of PUT wp/two-factor.
type twoFactorReq struct {
	Mode string `json:"mode"`
}

// handleSetWPTwoFactor turns two-factor sign-in off, makes it optional or
// requires it for administrators and shop managers.
func (s *Server) handleSetWPTwoFactor(w http.ResponseWriter, r *http.Request, c *call) {
	var req twoFactorReq
	if !decodeOr400(w, r, &req) {
		return
	}
	mode := req.Mode
	switch mode {
	case "off":
		mode = ""
	case state.TwoFactorOptional, state.TwoFactorRequired:
	default:
		respondErr(w, http.StatusBadRequest, "mode must be off, optional or required")
		return
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if !isWordPress(cur.Type) {
		fail(w, errNotWordPress)
		return
	}
	if cur.Config.WPTwoFactor != mode {
		if !s.applyWP(w, r, cur, func(cfg *state.SiteConfig) { cfg.WPTwoFactor = mode }) {
			return
		}
		s.audit(c.Actor(), "wp.two_factor", cur.Domain, req.Mode)
	}
	respond(w, http.StatusOK, map[string]string{"mode": req.Mode})
}

// applyWP renders cur with change and stores the change once the agent has
// applied it; false when it answered an error.
func (s *Server) applyWP(w http.ResponseWriter, r *http.Request, cur state.Site, change func(*state.SiteConfig)) bool {
	next := cur
	change(&next.Config)
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return false
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		change(&st.Config)
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return false
	}
	s.recordRendered(stored, res.Files)
	return true
}

// handleSetWPHardening replaces the site's WordPress hardening: each item
// on or off on its own, the XML-RPC exceptions and the sign-in path.
func (s *Server) handleSetWPHardening(w http.ResponseWriter, r *http.Request, c *call) {
	var h state.WPHardening
	if !decodeOr400(w, r, &h) {
		return
	}
	if h.LoginPath != "" && !site.ValidLoginPath(h.LoginPath) {
		respondErr(w, http.StatusBadRequest, "login_path must be 3–64 lowercase letters, digits and hyphens, not one of WordPress's or WooCommerce's own paths")
		return
	}
	if len(h.XMLRPCAllow) > site.MaxXMLRPCAllow {
		respondErr(w, http.StatusBadRequest, "at most 50 XML-RPC exceptions")
		return
	}
	allow := []string{}
	for _, a := range h.XMLRPCAllow {
		n, err := site.NormalizeXMLRPCAllow(a)
		var se *site.Error
		if errors.As(err, &se) {
			respondErr(w, http.StatusBadRequest, se.Msg)
			return
		}
		if !slices.Contains(allow, n) {
			allow = append(allow, n)
		}
	}
	h.XMLRPCAllow = allow
	if len(allow) == 0 {
		h.XMLRPCAllow = nil
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if !isWordPress(cur.Type) {
		fail(w, errNotWordPress)
		return
	}
	if !hardeningEqual(cur.Config.WPHardening, h) {
		if !s.applyWP(w, r, cur, func(cfg *state.SiteConfig) { cfg.WPHardening = h }) {
			return
		}
		// Cached pages still carry the version and the old sign-in links.
		// Best effort: they expire with the TTL anyway.
		next := cur
		next.Config.WPHardening = h
		_ = agentrpc.Into(r.Context(), s.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: next}, nil)
		s.audit(c.Actor(), "wp.hardening", cur.Domain, hardeningSummary(h))
	}
	respond(w, http.StatusOK, h)
}

func hardeningEqual(a, b state.WPHardening) bool {
	return a.XMLRPCOff == b.XMLRPCOff && slices.Equal(a.XMLRPCAllow, b.XMLRPCAllow) && a.FileEditOff == b.FileEditOff &&
		a.HideVersion == b.HideVersion && a.BlockReadme == b.BlockReadme && a.LoginPath == b.LoginPath
}

// hardeningSummary is the audit detail: the items that are on.
func hardeningSummary(h state.WPHardening) string {
	var on []string
	for _, it := range []struct {
		on   bool
		name string
	}{{h.XMLRPCOff, "xmlrpc"}, {h.FileEditOff, "file_edit"}, {h.HideVersion, "version"}, {h.BlockReadme, "readme"}, {h.LoginPath != "", "login_path"}} {
		if it.on {
			on = append(on, it.name)
		}
	}
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, ",")
}
