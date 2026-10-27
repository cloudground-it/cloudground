package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The backup routes (SPEC.md §5). Each checks the request and queues the
// job (backup_service.go); only the repository test talks to the agent
// directly, as it changes nothing.

// backupConfig answers for the handler when backups are not configured
// (412) or the settings cannot be read (500).
func (s *Server) backupConfig(w http.ResponseWriter) (repo, password string, ok bool) {
	repo, password, err := s.backupRepository()
	if err != nil {
		internalError(w, "backupRepository", err, "could not read the backup settings")
		return "", "", false
	}
	if repo == "" || password == "" {
		respondErr(w, http.StatusPreconditionFailed, "configure backup_repository and backup_password in settings first")
		return "", "", false
	}
	return repo, password, true
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.Store.ListBackups(c.Site.ID, 50)
	if err != nil {
		internalError(w, "ListBackups", err, "")
		return
	}
	// A repository URL can hold the off-site store's credentials, and
	// operators and read-only accounts read this list.
	for i := range list {
		list[i].Repository = maskRepositoryCredentials(list[i].Repository)
	}
	respond(w, http.StatusOK, list)
}

func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request, c *call) {
	if !backedUp(c.Site.Type) {
		respondErr(w, http.StatusConflict, "a reverse-proxy site has nothing to back up: its content is its upstream's")
		return
	}
	repo, _, ok := s.backupConfig(w)
	if !ok {
		return
	}
	s.audit(c.Actor(), "backup.run", c.Site.Domain, "")
	task, err := s.enqueue(jobs.Spec{Kind: "backup.run", SiteID: c.Site.ID, Locks: []string{jobs.RepoLock(repo)},
		Payload: sitePayload{SiteID: c.Site.ID}})
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// handleTestBackupRepo checks the configured repository can be reached
// (and initialises it when new), so an operator knows before relying on it.
func (s *Server) handleTestBackupRepo(w http.ResponseWriter, r *http.Request, c *call) {
	repo, password, ok := s.backupConfig(w)
	if !ok {
		return
	}
	var out map[string]any
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.TestBackup, rpc.TestBackupParams{Repository: repo, Password: password, Env: s.backupEnv()}, &out); err != nil {
		respondAgentErr(w, 0, "repository not reachable", err)
		return
	}
	s.audit(c.Actor(), "backup.test", maskRepositoryCredentials(repo), "")
	respond(w, http.StatusOK, out)
}

// handleVerifyBackup queues a snapshot's restore test again.
func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request, c *call) {
	if _, _, ok := s.backupConfig(w); !ok {
		return
	}
	s.audit(c.Actor(), "backup.verify", c.Site.Domain, c.Backup.SnapshotID)
	task, err := s.enqueueVerify(c.Backup, false)
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// handleRestoreBackup restores a snapshot over the live site, after a
// tested safety snapshot.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request, c *call) {
	b, site := c.Backup, c.Site
	var req struct {
		Confirm string `json:"confirm"`
		Mode    string `json:"mode"`
		// Databases restores only these of the backup's databases.
		Databases []string `json:"databases"`
		// Paths restores only these files or folders (§5.6).
		Paths []string `json:"paths"`
	}
	if err := decode(r, &req); err != nil || req.Confirm != site.Domain {
		respondErr(w, http.StatusBadRequest, "type the site domain to confirm restoration")
		return
	}
	switch req.Mode {
	case "":
		req.Mode = "full"
	case "full", "files", "database":
	default:
		respondErr(w, http.StatusBadRequest, "restore mode must be full, files, or database")
		return
	}
	if req.Mode == "files" && len(req.Databases) > 0 {
		respondErr(w, http.StatusBadRequest, "a files restore puts back no database")
		return
	}
	if len(req.Paths) > 0 && req.Mode != "files" {
		respondErr(w, http.StatusBadRequest, "only a files restore takes paths")
		return
	}
	if len(req.Paths) > rpc.MaxRestorePaths {
		respondErr(w, http.StatusBadRequest, "at most 20 paths")
		return
	}
	for i, raw := range req.Paths {
		p, err := rpc.RestorePath(raw)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Paths[i] = p
	}
	for _, name := range req.Databases {
		if _, err := s.resolveDB(site, name); err != nil || name == "" {
			respondErr(w, http.StatusBadRequest, "this site has no database "+strconv.Quote(name))
			return
		}
		// A record from before databases were kept (nil) leaves the
		// check to the agent, which reads the snapshot.
		if b.Databases != nil && !slices.Contains(b.Databases, name) {
			respondErr(w, http.StatusBadRequest, "snapshot "+b.SnapshotID+" holds no database "+strconv.Quote(name))
			return
		}
	}
	_, password, ok := s.backupConfig(w)
	if !ok {
		return
	}
	detail := b.SnapshotID
	if len(req.Paths) > 0 {
		detail += " paths " + strings.Join(req.Paths, ", ")
	}
	s.audit(c.Actor(), "backup.restore", site.Domain, detail)
	task, err := s.startJob(jobReq{kind: "backup.restore", siteID: site.ID,
		locks:   append(siteLocks(site.ID), jobs.RepoLock(b.Repository)),
		payload: map[string]any{"backup_id": b.ID, "mode": req.Mode, "databases": req.Databases, "paths": req.Paths},
	}, s.restoreWork(b, req.Mode, password, req.Databases, req.Paths))
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// handleDeleteBackup deletes one snapshot from the repository (R-4.3).
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request, c *call) {
	if _, _, ok := s.backupConfig(w); !ok {
		return
	}
	why, err := s.deleteRefusal(c.Backup, c.Site.Domain)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not read the site's backups")
		return
	}
	if why != "" {
		respondErr(w, http.StatusConflict, why)
		return
	}
	s.audit(c.Actor(), "backup.delete", c.Site.Domain, c.Backup.SnapshotID)
	task, err := s.enqueueForget(c.Backup)
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}
