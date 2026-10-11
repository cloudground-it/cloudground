package api

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// abandonCreatedSite handles a provisioning call that failed with err (R-3.13).
// An agent that answered with its error has undone its own steps, and the
// record goes. A call that never got an answer (the agent unreachable,
// restarted, or past its deadline) may have left the site half or fully
// built: the record stays, in error, so the site remains visible and a
// delete, which removes whatever is there, can clean it up. Dropping it
// would leave a site running that no panel page shows, and its domain
// could never be created again.
func (s *Server) abandonCreatedSite(site state.Site, err error) {
	switch agentrpc.CodeOf(err) {
	case agentrpc.CodeUnavailable, agentrpc.CodeTimeout, agentrpc.CodeCanceled:
		s.Log.Warn("site provisioning ended without an answer from the agent; keeping the site in error for a delete to clean up",
			"site", site.Domain, "err", err)
		s.markSiteError(site.ID)
	default:
		s.rollbackCreatedSite(site)
	}
}

// teardownCreatedSite undoes a site the agent provisioned, after a later step
// failed. The record goes only once the agent has removed the site;
// otherwise it stays, in error, for a delete to finish (R-3.13).
func (s *Server) teardownCreatedSite(site state.Site) {
	if err := agentrpc.Into(context.Background(), s.Agent, agentrpc.DeleteSite, rpc.SiteRef{Site: site}, nil); err != nil {
		s.Log.Warn("could not undo a site after its provisioning failed; keeping it in error", "site", site.Domain, "err", err)
		s.markSiteError(site.ID)
		return
	}
	s.rollbackCreatedSite(site)
}

func (s *Server) rollbackCreatedSite(site state.Site) {
	s.forgetSiteFiles(site)
	// The site's secrets go with it.
	s.logIfErr(s.Store.DeleteSite(site.ID), "DeleteSite")
}
