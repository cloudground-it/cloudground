package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Downloads of a site's whole access and error logs (SPEC.md §3.19, #84).
// The log viewer shows the last lines; these are the files nginx writes and
// logrotate keeps, current and rotated, compressed or not, for the accounts
// that manage the site.

// rotatedLogRe tells a rotated log (whose size no longer changes) from the
// one nginx is writing.
var rotatedLogRe = regexp.MustCompile(`\.log\.[0-9]`)

// handleListSiteLogs lists the site's downloadable logs.
func (s *Server) handleListSiteLogs(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.ListSiteLogs, rpc.SiteLogsParams{Site: c.Site})
	answer(w, http.StatusOK, res, err)
}

// handleDownloadSiteLog streams one log, a chunk at a time from the agent
// (the API never holds more than one). The name is the agent's rule
// (rpc.ValidSiteLogName) checked here too, so nothing but a log of the
// site's own directory is ever asked for, and the domain of the route's
// site names the directory.
func (s *Server) handleDownloadSiteLog(w http.ResponseWriter, r *http.Request, c *call) {
	name := r.URL.Query().Get("file")
	if !rpc.ValidSiteLogName(name) {
		respondErr(w, http.StatusBadRequest, "file must be an access or error log, such as access.log, access.log.1 or error.log.2.gz")
		return
	}
	read := func(off int64) (rpc.SiteLogChunkResult, error) {
		return agentrpc.Call(r.Context(), s.Agent, agentrpc.ReadSiteLogChunk, rpc.SiteLogChunkParams{
			Site: c.Site, Name: name, Offset: off, Length: downloadChunk,
		})
	}
	first, err := read(0)
	if err != nil {
		if agentrpc.CodeOf(err) == agentrpc.CodeNotFound {
			respondErr(w, http.StatusNotFound, "no such log")
			return
		}
		replyFailure(w, err)
		return
	}
	s.audit(c.Actor(), "site.logs.download", c.Site.Domain, name)
	h := w.Header()
	if strings.HasSuffix(name, ".gz") {
		h.Set("Content-Type", "application/gzip")
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", c.Site.Domain+"-"+name))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	// The file nginx is writing grows (or is truncated by a rotation) while it
	// is sent, so it has no length up front; a rotated one is final.
	if rotatedLogRe.MatchString(name) {
		h.Set("Content-Length", strconv.FormatInt(first.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	off, chunk := int64(0), first
	for {
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		off += int64(len(chunk.Data))
		if len(chunk.Data) == 0 || off >= chunk.Size && rotatedLogRe.MatchString(name) {
			return
		}
		if chunk, err = read(off); err != nil {
			// The headers are gone: a short body is how the browser learns
			// the download failed.
			if s.Log != nil {
				s.Log.Warn("a log download stopped", "site", c.Site.Domain, "file", name, "offset", off, "err", err)
			}
			return
		}
	}
}
