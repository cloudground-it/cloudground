package api

import (
	"context"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Drift (SPEC.md §7.1): the files the panel rendered, checked against the
// hash it recorded, and what a person does about a difference. Nothing is
// ever overwritten without being asked.

// storeError is a failed read or write of the panel's own records during
// a drift operation: the request answers 500 with what.
func storeError(s *Server, err error, op, what string) error {
	s.logIfErr(err, op)
	return decline(http.StatusInternalServerError, "%s", what)
}

// scanDrift checks every managed file against the agent, opens an event for
// each new difference, closes the ones that went away, and returns the
// open set, newest first. The store announces drift.changed only when an
// event opens or closes, so a refetch prompted by one scans once and stops.
// GET /api/system/drift and the drift.scan job call it.
func (s *Server) scanDrift(ctx context.Context) ([]state.DriftEvent, error) {
	const failed = "could not scan for drift"
	tracked, err := s.Store.ManagedFiles()
	if err != nil {
		return nil, storeError(s, err, "ManagedFiles", failed)
	}
	var found rpc.DriftResult
	if len(tracked) > 0 {
		if found, err = agentrpc.Call(ctx, s.Agent, agentrpc.CheckDrift, rpc.DriftParams{Expected: tracked}); err != nil {
			return nil, err
		}
		for _, p := range found.NotManaged {
			s.logIfErr(s.Store.RemoveManagedFile(p), "RemoveManagedFile")
			delete(tracked, p)
		}
	}
	drifted := make(map[string]bool, len(found.Drifted))
	for _, d := range found.Drifted {
		drifted[d.Path] = true
	}
	events, err := s.Store.ListDriftEvents(false)
	if err != nil {
		return nil, storeError(s, err, "ListDriftEvents", failed)
	}
	open := []state.DriftEvent{}
	covered := map[string]bool{} // path and hash with an open event
	for _, e := range events {
		_, still := tracked[e.Path]
		switch {
		case !still:
			// Its site went, or a render stopped writing it: moot.
			s.logIfErr(s.Store.ResolveDriftEvent(e.ID, state.DriftAccepted), "ResolveDriftEvent")
		case !drifted[e.Path]:
			// A render or an upgrade put the panel's file back.
			s.logIfErr(s.Store.ResolveDriftEvent(e.ID, state.DriftRestored), "ResolveDriftEvent")
		default:
			covered[e.Path+"\x00"+e.ActualHash] = true
			open = append(open, e)
		}
	}
	var fresh []state.DriftEvent
	for _, d := range found.Drifted {
		if covered[d.Path+"\x00"+d.ActualHash] {
			continue
		}
		e, err := s.Store.CreateDriftEvent(state.DriftEvent{Path: d.Path, ExpectedHash: d.ExpectedHash, ActualHash: d.ActualHash})
		if err != nil {
			s.logIfErr(err, "CreateDriftEvent")
			continue
		}
		fresh = append(fresh, e)
	}
	// Newest first: the new events, latest created first, then the open
	// ones the store listed newest first.
	for i, j := 0, len(fresh)-1; i < j; i, j = i+1, j-1 {
		fresh[i], fresh[j] = fresh[j], fresh[i]
	}
	return append(append(make([]state.DriftEvent, 0, len(fresh)+len(open)), fresh...), open...), nil
}

func (s *Server) handleListDrift(w http.ResponseWriter, r *http.Request) {
	open, err := s.scanDrift(r.Context())
	answer(w, http.StatusOK, open, err)
}

// openDrift is the open event id, or a refusal.
func (s *Server) openDrift(id int64) (state.DriftEvent, error) {
	events, err := s.Store.ListDriftEvents(false)
	if err != nil {
		return state.DriftEvent{}, storeError(s, err, "ListDriftEvents", "could not read the drift events")
	}
	for _, e := range events {
		if e.ID == id {
			return e, nil
		}
	}
	return state.DriftEvent{}, decline(http.StatusNotFound, "drift event not found or already resolved")
}

// acceptDrift keeps the change: the file's hash now is what the panel
// expects, or a file that is gone is no longer tracked.
func (s *Server) acceptDrift(e state.DriftEvent) {
	if e.ActualHash == "" {
		s.logIfErr(s.Store.RemoveManagedFile(e.Path), "RemoveManagedFile")
	} else {
		s.logIfErr(s.Store.RecordManagedFile(e.Path, e.ActualHash), "RecordManagedFile")
	}
	s.logIfErr(s.Store.ResolveDriftEvent(e.ID, state.DriftAccepted), "ResolveDriftEvent")
}

// restoreDrift renders the site that owns the file again, as it is stored
// now and under its lock: a render is idempotent, so the panel's file comes
// back.
func (s *Server) restoreDrift(ctx context.Context, e state.DriftEvent) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return storeError(s, err, "ListSites", "could not read the sites")
	}
	for _, listed := range sites {
		if !siteOwnsPath(listed, e.Path) {
			continue
		}
		site, unlock, err := s.freshSite(listed.ID)
		if err != nil {
			unlock()
			return storeError(s, err, "GetSite", "could not read the site")
		}
		res, err := agentrpc.Call(ctx, s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: site})
		s.logIfErr(s.Store.MarkApplied(site.ID, site.DesiredGeneration, err), "MarkApplied")
		unlock()
		if err != nil {
			return err
		}
		for _, f := range res.Files {
			s.logIfErr(s.Store.RecordManagedFile(f.Path, f.SHA256), "RecordManagedFile")
		}
		s.logIfErr(s.Store.ResolveDriftEvent(e.ID, state.DriftRestored), "ResolveDriftEvent")
		return nil
	}
	return decline(http.StatusUnprocessableEntity, "no site owns this file; accept the change or remove the file manually")
}

// msgAcceptNeedsSession refuses an API token the acceptance of drift.
const msgAcceptNeedsSession = "an API token cannot accept drift: accepting a changed file needs a signed-in session (a token may restore it)"

func (s *Server) handleResolveDrift(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		Action string `json:"action"` // restore | accept
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	// Accepting makes a changed managed file the expected one: a token
	// could bless a change it planted itself. Restoring only puts the
	// panel's own render back, so a token may (SPEC.md §1.2.1).
	if req.Action == "accept" && c.ViaToken() {
		respondErr(w, http.StatusForbidden, msgAcceptNeedsSession)
		return
	}
	e, err := s.openDrift(id)
	if err != nil {
		fail(w, err)
		return
	}
	switch req.Action {
	case "accept":
		s.acceptDrift(e)
		s.audit(c.Actor(), "drift.accept", e.Path, "")
		respond(w, http.StatusOK, map[string]string{"resolved": "accepted"})
	case "restore":
		if err := s.restoreDrift(r.Context(), e); err != nil {
			fail(w, err)
			return
		}
		s.audit(c.Actor(), "drift.restore", e.Path, "")
		respond(w, http.StatusOK, map[string]string{"resolved": "restored"})
	default:
		respondErr(w, http.StatusBadRequest, "action must be restore or accept")
	}
}
