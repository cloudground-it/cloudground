package api

// The HTTP side of the sites domain (SPEC.md §3): every handler here
// decodes, calls one method of siteService or stagingService and writes
// what it answers.

import (
	"errors"
	"io"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// problemKind is the class of a refusal; it decides the status (§3.8).
type problemKind int

const (
	probInvalid problemKind = iota
	probForbidden
	probNotFound
	probConflict
	probPrecondition
)

var problemStatus = map[problemKind]int{
	probInvalid:      http.StatusBadRequest,
	probForbidden:    http.StatusForbidden,
	probNotFound:     http.StatusNotFound,
	probConflict:     http.StatusConflict,
	probPrecondition: http.StatusPreconditionFailed,
}

// problem is a request the sites domain refuses. fields, when set, says
// what is wrong with each named field of the request.
type problem struct {
	kind   problemKind
	msg    string
	fields map[string]string
}

func (p *problem) Error() string { return p.msg }

func refuse(kind problemKind, msg string) *problem { return &problem{kind: kind, msg: msg} }

// agentFailure is an error that came from the agent (or from asking it for
// its capabilities): it answers as §1.7 says.
type agentFailure struct{ err error }

func (a agentFailure) Error() string { return a.err.Error() }
func (a agentFailure) Unwrap() error { return a.err }

// queueFailure is a job the queue would not take.
type queueFailure struct{ err error }

func (q queueFailure) Error() string { return q.err.Error() }
func (q queueFailure) Unwrap() error { return q.err }

// replyFailure writes the answer for an error a service returned.
func replyFailure(w http.ResponseWriter, err error) {
	var (
		p  *problem
		af agentFailure
		qf queueFailure
	)
	switch {
	case errors.As(err, &p):
		body := map[string]any{"error": p.msg}
		if len(p.fields) > 0 {
			body["fields"] = p.fields
		}
		respond(w, problemStatus[p.kind], body)
	case errors.As(err, &qf):
		respondJobErr(w, qf.err)
	case errors.As(err, &af), agentrpc.AsError(err) != nil:
		respondAgentErr(w, 0, "", err)
	default:
		internalError(w, "sites", err, "")
	}
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.siteSvc().List(c.User)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, c.siteViews(list))
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, c.siteView(c.Site))
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request, c *call) {
	var req newSite
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	out, err := s.siteSvc().create(r.Context(), c.Actor(), req, r.Host)
	if err != nil {
		replyFailure(w, err)
		return
	}
	// The summary carries the database password: this answer is the only
	// one that ever does (§3.2).
	respond(w, http.StatusAccepted, map[string]any{"site": c.siteView(out.site), "task": out.task, "summary": out.summary})
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request, c *call) {
	task, err := s.siteSvc().Delete(c.Actor(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// handlePatchSite is PATCH /api/sites/{id}: any editable setting, one job.
func (s *Server) handlePatchSite(w http.ResponseWriter, r *http.Request, c *call) {
	var p sitePatch
	if err := decode(r, &p); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	up, err := s.siteSvc().Update(r.Context(), c.Site, p, updateBy{
		actor: c.Actor(), admin: c.User.Role == roleAdmin,
	})
	if err != nil {
		replyFailure(w, err)
		return
	}
	body := map[string]any{"site": c.siteView(up.site), "task": up.task}
	if len(up.warnings) > 0 {
		body["warnings"] = up.warnings
	}
	respond(w, http.StatusAccepted, body)
}

// Staging (SPEC.md §3.7).

func (s *Server) handleCreateStaging(w http.ResponseWriter, r *http.Request, c *call) {
	stg, task, err := s.stagingSvc().Create(r.Context(), c.Actor(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"staging": c.siteView(stg), "task": task})
}

func (s *Server) handleRecreateStaging(w http.ResponseWriter, r *http.Request, c *call) {
	task, err := s.stagingSvc().Recreate(r.Context(), c.Actor(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

func (s *Server) handleStagingCredentials(w http.ResponseWriter, r *http.Request, c *call) {
	cred, err := s.stagingSvc().Credentials(c.Actor(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, cred)
}

func (s *Server) handleStagingDiff(w http.ResponseWriter, r *http.Request, c *call) {
	diff, err := s.stagingSvc().Diff(r.Context(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, diff)
}

func (s *Server) handleStagingTables(w http.ResponseWriter, r *http.Request, c *call) {
	stg, tables, err := s.stagingSvc().Tables(r.Context(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"tables": tables, "staging": stg.Domain})
}

func (s *Server) handlePushStagingDatabase(w http.ResponseWriter, r *http.Request, c *call) {
	var req stagingPush
	if err := decode(r, &req); err != nil {
		// Answered as an unconfirmed push, after the staging check.
		req = stagingPush{}
	}
	task, err := s.stagingSvc().PushDatabase(r.Context(), c.Actor(), c.Site, req)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// Cache purge (SPEC.md §3.9).

func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request, c *call) {
	var b struct {
		URLs []string `json:"urls"`
	}
	// No body purges everything; a body that does not parse purges nothing.
	if err := decode(r, &b); err != nil && !errors.Is(err, io.EOF) {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	res, err := s.siteSvc().Purge(r.Context(), c.Actor(), c.Site, b.URLs)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, res)
}

// siteSvc and stagingSvc are the domain services over this server's store,
// agent and queue.
func (s *Server) siteSvc() siteService       { return siteService{s} }
func (s *Server) stagingSvc() stagingService { return stagingService{s} }

// siteView is a site as this caller may see it (SPEC.md §3.1): never the
// staging password hash, and the routing block only for an administrator's
// session, exactly who may read GET /api/sites/{id}/routing. The block is
// nginx configuration, and may carry what an administrator keeps from the
// other accounts (an upstream's address, an allowlist).
func (c *call) siteView(site state.Site) state.Site {
	site = site.Public()
	if c.User.Role != roleAdmin || c.ViaToken() {
		site.Config.Routing = ""
	}
	return site
}

// siteViews is siteView over a list; an empty list is [].
func (c *call) siteViews(list []state.Site) []state.Site {
	out := make([]state.Site, 0, len(list))
	for _, site := range list {
		out = append(out, c.siteView(site))
	}
	return out
}
