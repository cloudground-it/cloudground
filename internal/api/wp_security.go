package api

import (
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// WordPress sign-in security (SPEC.md §6.2.1, issue #74). The setting
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
		next := cur
		next.Config.WPTwoFactor = mode
		var res rpc.ApplyResult
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
			respondAgentErr(w, 0, "", err)
			return
		}
		stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
			st.Config.WPTwoFactor = mode
			return nil
		})
		if err != nil {
			internalError(w, "MutateSiteApplied", err, "")
			return
		}
		s.recordRendered(stored, res.Files)
		s.audit(c.Actor(), "wp.two_factor", cur.Domain, req.Mode)
	}
	respond(w, http.StatusOK, map[string]string{"mode": req.Mode})
}
