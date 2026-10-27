package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// Traffic statistics (SPEC.md §11.8, #33): the agent reads every site's
// access log on the maintenance tick and keeps aggregates; the panel asks
// it for a site's range. No cookie, no script, no third party.

// runTrafficCollect is the traffic.collect job: one TrafficCollect with
// every site the panel has.
func (s *Server) runTrafficCollect(ctx context.Context, r *jobs.Run) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	var p rpc.TrafficCollectParams
	for _, st := range sites {
		if site.ValidateDomain(st.Domain) == nil {
			p.Sites = append(p.Sites, rpc.TrafficTarget{ID: st.ID, Domain: st.Domain})
		}
	}
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.TrafficCollect, p)
	if err != nil {
		return err
	}
	var read, lines int64
	for _, row := range res.Sites {
		read, lines = read+row.Read, lines+row.Lines
	}
	r.Progress(100, fmt.Sprintf("%d sites, %d lines (%d bytes) read", len(res.Sites), lines, read))
	return nil
}

// handleTraffic is GET /api/sites/{id}/traffic?days=7|30|90.
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request, c *call) {
	days := 7
	switch r.URL.Query().Get("days") {
	case "", "7":
	case "30":
		days = 30
	case "90":
		days = 90
	default:
		respondErr(w, http.StatusBadRequest, "days must be 7, 30 or 90")
		return
	}
	res, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.TrafficStats,
		rpc.TrafficStatsParams{TrafficTarget: rpc.TrafficTarget{ID: c.Site.ID, Domain: c.Site.Domain}, Days: days})
	if err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}
