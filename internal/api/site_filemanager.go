package api

import (
	"context"
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The file manager (SPEC.md §6.1). The agent confines every path beneath
// the site's root; the panel decides who may ask and keeps the trail.

// maxPanelTransfer is the largest file the panel moves in one request
// either way; SFTP is for anything bigger.
const maxPanelTransfer = 16 << 20

// fileManager is the file manager's service.
type fileManager struct{ agent AgentCaller }

func (s *Server) files() fileManager { return fileManager{s.Agent} }

func (f fileManager) list(ctx context.Context, site state.Site, rel string) (rpc.ListFilesResult, error) {
	return agentrpc.Call(ctx, f.agent, agentrpc.ListFiles, rpc.ListFilesParams{Site: site, RelPath: rel})
}

func (f fileManager) read(ctx context.Context, site state.Site, rel string) (rpc.ReadFileResult, error) {
	return agentrpc.Call(ctx, f.agent, agentrpc.ReadFile, rpc.ReadFileParams{Site: site, RelPath: rel})
}

func (f fileManager) write(ctx context.Context, site state.Site, rel, content string) (map[string]string, error) {
	return agentrpc.Call(ctx, f.agent, agentrpc.WriteFile, rpc.WriteFileParams{Site: site, RelPath: rel, Content: content})
}

func (f fileManager) download(ctx context.Context, site state.Site, rel string) (rpc.TransferFileResult, error) {
	return agentrpc.Call(ctx, f.agent, agentrpc.TransferFile, rpc.TransferFileParams{Site: site, RelPath: rel})
}

func (f fileManager) upload(ctx context.Context, site state.Site, rel string, data []byte) (rpc.TransferFileResult, error) {
	return agentrpc.Call(ctx, f.agent, agentrpc.TransferFile, rpc.TransferFileParams{Site: site, RelPath: rel, Data: data, Upload: true})
}

func (f fileManager) manage(ctx context.Context, site state.Site, op, rel, dest string) (map[string]string, error) {
	return agentrpc.Call(ctx, f.agent, agentrpc.ManageFile, rpc.ManageFileParams{Site: site, Operation: op, RelPath: rel, DestPath: dest})
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.files().list(r.Context(), c.Site, r.URL.Query().Get("path"))
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.files().read(r.Context(), c.Site, r.URL.Query().Get("path"))
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	out, err := s.files().write(r.Context(), c.Site, req.Path, req.Content)
	if err == nil {
		s.audit(c.Actor(), "file.write", c.Site.Domain, req.Path)
	}
	answer(w, http.StatusOK, out, err)
}

// handleDownloadFile sends the file as an attachment under its own name.
func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.files().download(r.Context(), c.Site, r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(res.Name)))
	h.Set("Content-Length", strconv.Itoa(len(res.Data)))
	_, _ = w.Write(res.Data)
}

// handleUploadFile stores the raw body at the path; the router has bounded
// it at maxPanelTransfer.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request, c *call) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		respondErr(w, http.StatusRequestEntityTooLarge, "upload exceeds 16 MB; use SFTP for larger files")
		return
	}
	rel := r.URL.Query().Get("path")
	res, err := s.files().upload(r.Context(), c.Site, rel, data)
	if err == nil {
		s.audit(c.Actor(), "file.upload", c.Site.Domain, rel)
	}
	answer(w, http.StatusCreated, res, err)
}

// handleManageFile makes a directory, deletes or renames. The agent knows
// the operations; one it refuses is never audited.
func (s *Server) handleManageFile(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Operation string `json:"operation"`
		Path      string `json:"path"`
		Dest      string `json:"dest,omitempty"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	out, err := s.files().manage(r.Context(), c.Site, req.Operation, req.Path, req.Dest)
	if err == nil {
		s.audit(c.Actor(), "file."+req.Operation, c.Site.Domain, req.Path)
	}
	answer(w, http.StatusOK, out, err)
}
