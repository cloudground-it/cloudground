package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's development mode (SPEC.md §3.15, issue #80): the page cache is
// bypassed for a while and the panel switches it back on by itself. Like
// the protected paths and the refused visitors, a change renders the site
// at once and is stored only once nginx has it.

// devModeMinutes are the lengths a site may be put in development mode for.
var devModeMinutes = []int{15, 30, 60}

// devModeView is what the route answers: the end of the mode, null when off.
type devModeView struct {
	Until *time.Time `json:"dev_mode_until"`
}

func devModeOf(st state.Site) devModeView {
	if st.Config.DevModeUntil.IsZero() {
		return devModeView{}
	}
	u := st.Config.DevModeUntil
	return devModeView{Until: &u}
}

// handleSetDevMode starts development mode for 15, 30 or 60 minutes (again
// while it runs: the time starts over), or ends it with 0.
func (s *Server) handleSetDevMode(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Minutes != 0 && !slices.Contains(devModeMinutes, req.Minutes) {
		respondErr(w, http.StatusBadRequest, "minutes must be 15, 30 or 60, or 0 to end development mode")
		return
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if req.Minutes == 0 && cur.Config.DevModeUntil.IsZero() {
		respond(w, http.StatusOK, devModeOf(cur))
		return
	}
	if req.Minutes > 0 && !cacheable(cur) {
		respondErr(w, http.StatusConflict, "this site has no page cache to bypass")
		return
	}
	var until time.Time
	if req.Minutes > 0 {
		until = time.Now().UTC().Add(time.Duration(req.Minutes) * time.Minute).Truncate(time.Second)
	}
	stored, err := s.setDevMode(r.Context(), cur, until)
	if err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	detail := "ended"
	if req.Minutes > 0 {
		detail = fmt.Sprintf("%d minutes", req.Minutes)
	}
	s.audit(c.Actor(), "site.devmode", cur.Domain, detail)
	respond(w, http.StatusOK, devModeOf(stored))
}

// cacheable reports whether the site has a page cache for the mode to
// bypass.
func cacheable(st state.Site) bool {
	return st.Config.CacheEnabled && st.Type != state.SiteStatic && st.Type != state.SiteProxy && !st.Type.IsApp()
}

// setDevMode renders the site with the mode set to until (zero ends it) and
// stores it once nginx has it. The caller holds the site's lock. Ending it
// empties the page cache, which may hold pages from before the work, and
// queues the re-warm that refills it.
func (s *Server) setDevMode(ctx context.Context, cur state.Site, until time.Time) (state.Site, error) {
	next := cur
	next.Config.DevModeUntil = until
	var res rpc.ApplyResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
		return state.Site{}, err
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.DevModeUntil = until
		return nil
	})
	if err != nil {
		return state.Site{}, err
	}
	s.recordRendered(stored, res.Files)
	if until.IsZero() {
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: stored}, nil); err != nil {
			s.Log.Warn("could not empty the page cache after development mode", "site", stored.Domain, "err", err)
		}
		s.warmInBackground(stored)
	}
	return stored, nil
}

// runDevModeSweep is the devmode.sweep job (every minute): it ends the
// development mode of each site whose time has come. A site it cannot
// render stays in the mode and is tried again at the next sweep.
func (s *Server) runDevModeSweep(ctx context.Context, r *jobs.Run) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	ended := 0
	for _, st := range sites {
		if ctx.Err() != nil {
			break
		}
		if st.Status != state.SiteActive || st.Config.DevModeUntil.IsZero() || st.Config.DevModeUntil.After(time.Now()) {
			continue
		}
		if s.endDevMode(ctx, st.ID) {
			ended++
		}
	}
	r.Progress(100, fmt.Sprintf("%d sites out of development mode", ended))
	return nil
}

// endDevMode ends the mode of one site as stored now, if its time has come
// (it may have been extended or ended since the list was read).
func (s *Server) endDevMode(ctx context.Context, id int64) bool {
	cur, unlock, err := s.freshSite(id)
	defer unlock()
	if err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			s.logIfErr(err, "GetSite")
		}
		return false
	}
	if cur.Status != state.SiteActive || cur.Config.DevModeUntil.IsZero() || cur.Config.DevModeUntil.After(time.Now()) {
		return false
	}
	if _, err := s.setDevMode(ctx, cur, time.Time{}); err != nil {
		s.Log.Warn("ending development mode failed", "site", cur.Domain, "err", err)
		s.audit("system", "site.devmode_end_failed", cur.Domain, err.Error())
		return false
	}
	s.audit("system", "site.devmode", cur.Domain, "ended")
	return true
}
