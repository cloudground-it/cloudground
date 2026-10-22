package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/netip"
	"slices"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's maintenance page (SPEC.md §3.16, issue #79): a 503 with
// Retry-After for visitors, the site as ever for the addresses listed and
// for a browser holding the key's cookie, the cache bypassed meanwhile.
// Like the protected paths, a change renders the site at once and is
// stored only once nginx has it.

// maintenanceView is what the routes answer.
type maintenanceView struct {
	Enabled       bool     `json:"enabled"`
	Title         string   `json:"title"`
	Message       string   `json:"message"`
	RetryAfterSec int      `json:"retry_after_sec"`
	Allow         []string `json:"allow"`
	// PreviewURL, while the mode is on, is the address that sets the cookie
	// letting a browser through.
	PreviewURL string `json:"preview_url,omitempty"`
}

func maintenanceOf(st state.Site) maintenanceView {
	m := st.Config.Maintenance
	v := maintenanceView{Enabled: m.Enabled, Title: m.Title, Message: m.Message, RetryAfterSec: m.RetryAfterSec, Allow: m.Allow}
	if v.Allow == nil {
		v.Allow = []string{}
	}
	if m.Enabled && m.Key != "" {
		v.PreviewURL = "https://" + st.Domain + "/.cg-maintenance/" + m.Key
	}
	return v
}

// handleGetMaintenance answers the page's settings and, while it is on,
// the address that lets a browser through.
func (s *Server) handleGetMaintenance(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, maintenanceOf(c.Site))
}

// handleSetMaintenance changes the page's settings (a field left out keeps
// its value) and turns it on or off.
func (s *Server) handleSetMaintenance(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Enabled       *bool     `json:"enabled"`
		Title         *string   `json:"title"`
		Message       *string   `json:"message"`
		RetryAfterSec *int      `json:"retry_after_sec"`
		Allow         *[]string `json:"allow"`
		// AllowMyAddress adds the caller's own address to Allow.
		AllowMyAddress bool `json:"allow_my_address"`
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
	m := cur.Config.Maintenance
	if req.Enabled != nil {
		m.Enabled = *req.Enabled
	}
	if req.Title != nil {
		m.Title = *req.Title
	}
	if req.Message != nil {
		m.Message = *req.Message
	}
	if req.RetryAfterSec != nil {
		m.RetryAfterSec = *req.RetryAfterSec
	}
	if req.Allow != nil || req.AllowMyAddress {
		var list []string
		if req.Allow != nil {
			list = *req.Allow
		}
		if req.AllowMyAddress {
			me, err := netip.ParseAddr(clientIP(r))
			if err != nil {
				respondErr(w, http.StatusBadRequest, "your address is not known here")
				return
			}
			list = append(list, me.Unmap().String())
		}
		if req.Allow == nil {
			list = append(slices.Clone(m.Allow), list...)
		}
		allow := make([]string, 0, len(list))
		for _, a := range list {
			n, err := site.NormalizeXMLRPCAllow(a)
			if err != nil {
				respondErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if !slices.Contains(allow, n) {
				allow = append(allow, n)
			}
		}
		slices.Sort(allow)
		m.Allow = nil
		if len(allow) > 0 {
			m.Allow = allow
		}
	}
	if m.Enabled && m.Key == "" {
		var b [16]byte
		_, _ = rand.Read(b[:]) // crypto/rand does not fail on a working system
		m.Key = hex.EncodeToString(b[:])
	}
	if err := site.CheckMaintenance(m); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	next := cur
	next.Config.Maintenance = m
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.Maintenance = m
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.recordRendered(stored, res.Files)
	if cur.Config.Maintenance.Enabled && !m.Enabled {
		// The cache was bypassed meanwhile: what it holds is from before the
		// work. Empty it and fill it again.
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: stored}, nil); err != nil {
			s.Log.Warn("could not empty the page cache after maintenance", "site", stored.Domain, "err", err)
		}
		s.warmInBackground(stored)
	}
	detail := "text"
	switch {
	case m.Enabled && !cur.Config.Maintenance.Enabled:
		detail = "on"
	case !m.Enabled && cur.Config.Maintenance.Enabled:
		detail = "off"
	}
	s.audit(c.Actor(), "site.maintenance", cur.Domain, detail)
	respond(w, http.StatusOK, maintenanceOf(stored))
}
