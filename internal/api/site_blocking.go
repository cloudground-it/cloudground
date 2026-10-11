package api

import (
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's refused visitors (SPEC.md §3.13, issue #25): addresses, user
// agents and countries the site answers 403. Like the protected paths
// (protected_paths.go), a change renders the site at once and is stored
// only once nginx has it.

// handleSetBlocking replaces the site's rules.
func (s *Server) handleSetBlocking(w http.ResponseWriter, r *http.Request, c *call) {
	var req state.SiteBlocking
	if !decodeOr400(w, r, &req) {
		return
	}
	// Written as the operator typed them; stored as nginx reads them.
	for i, a := range req.Addresses {
		p, err := blocklist.ParsePrefix(a)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Addresses[i] = p.String()
	}
	for i, cc := range req.Countries {
		req.Countries[i] = strings.ToUpper(strings.TrimSpace(cc))
	}
	for i, u := range req.UserAgents {
		req.UserAgents[i] = strings.TrimSpace(u)
	}
	req.Addresses = slices.Compact(sortedCopy(req.Addresses))
	req.Countries = slices.Compact(sortedCopy(req.Countries))
	if err := site.CheckBlocking(req); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ex := blocklist.Exempt{Own: s.ownAddrs()}
	me, meErr := netip.ParseAddr(clientIP(r))
	for _, a := range req.Addresses {
		p := netip.MustParsePrefix(a)
		if meErr == nil && p.Contains(me.Unmap()) {
			respondErr(w, http.StatusBadRequest, a+" holds your own address ("+me.Unmap().String()+"): you would lock yourself out of the site")
			return
		}
		if ex.Overlaps(p) {
			respondErr(w, http.StatusBadRequest, a+" holds this server or loopback: the panel's own checks of the site come from there")
			return
		}
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	next := cur
	next.Config.Blocking = req
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.Blocking = req
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.recordRendered(stored, res.Files)
	detail := "none"
	if req.Any() {
		detail = strings.Join([]string{
			itoaN(len(req.Addresses)) + " addresses",
			itoaN(len(req.UserAgents)) + " user agents",
			countriesDetail(req),
		}, ", ")
	}
	s.audit(c.Actor(), "site.blocking", cur.Domain, detail)
	respond(w, http.StatusOK, stored.Config.Blocking)
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func itoaN(n int) string { return strconv.Itoa(n) }

func countriesDetail(b state.SiteBlocking) string {
	switch {
	case len(b.Countries) == 0:
		return "no countries"
	case b.AllowCountriesOnly:
		return "only " + strings.Join(b.Countries, " ")
	}
	return "countries " + strings.Join(b.Countries, " ")
}
