package api

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site behind Cloudflare (SPEC.md §3.20, issue #22): the visitor's
// address from CF-Connecting-IP, trusted from Cloudflare's ranges only,
// and the zone its purges also empty. As the maintenance page, a change
// renders the site at once and is stored only once nginx has it.

// zoneIDRe is a Cloudflare zone ID: 32 hexadecimal digits.
var zoneIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// cloudflareView is what the routes answer.
type cloudflareView struct {
	RealIP bool   `json:"real_ip"`
	ZoneID string `json:"zone_id"`
	// PurgeReady: a zone and the purge token are both set.
	PurgeReady bool `json:"purge_ready"`
	// RangesUpdatedAt is when the ranges in use were fetched; absent, the
	// shipped list.
	RangesUpdatedAt *time.Time `json:"ranges_updated_at,omitempty"`
}

func (s *Server) cloudflareOf(st state.Site) cloudflareView {
	cf := st.Config.Cloudflare
	v := cloudflareView{RealIP: cf.RealIP, ZoneID: cf.ZoneID}
	if cf.ZoneID != "" {
		token, err := s.Store.Secret(state.SecretCloudflarePurgeToken)
		s.logIfErr(err, "Secret")
		v.PurgeReady = token != ""
	}
	if at := s.cloudflareRangesInUse().at; !at.IsZero() {
		v.RangesUpdatedAt = &at
	}
	return v
}

func (s *Server) handleGetCloudflare(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, s.cloudflareOf(c.Site))
}

// handleSetCloudflare changes the settings (a field left out keeps its
// value).
func (s *Server) handleSetCloudflare(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		RealIP *bool   `json:"real_ip"`
		ZoneID *string `json:"zone_id"`
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
	cf := cur.Config.Cloudflare
	if req.RealIP != nil {
		cf.RealIP = *req.RealIP
	}
	if req.ZoneID != nil {
		zone := strings.ToLower(strings.TrimSpace(*req.ZoneID))
		if zone != "" && !zoneIDRe.MatchString(zone) {
			respondErr(w, http.StatusBadRequest, "zone_id must be the zone's 32-character ID, from the Overview page of the domain in Cloudflare's dashboard")
			return
		}
		cf.ZoneID = zone
	}
	if cf.RealIP && !cur.Config.Cloudflare.RealIP {
		// The agent's file holds the panel's ranges before a vhost names it.
		if _, err := s.pushCloudflareRanges(r.Context()); err != nil {
			respondAgentErr(w, 0, "", err)
			return
		}
	}
	next := cur
	next.Config.Cloudflare = cf
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next, ReloadOnlyIfChanged: true}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.Cloudflare = cf
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.recordRendered(stored, res.Files)
	onOff, zone := "off", "none"
	if cf.RealIP {
		onOff = "on"
	}
	if cf.ZoneID != "" {
		zone = "set"
	}
	s.audit(c.Actor(), "site.cloudflare", cur.Domain, "real_ip="+onOff+" zone="+zone)
	respond(w, http.StatusOK, s.cloudflareOf(stored))
}
