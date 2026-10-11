package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Quarry's data routes (SPEC.md §6.4), /api/quarry/…, over the agent's
// DBStudio* methods (internal/agent/ops/dbstudio/SPEC.md). The agent does
// the work and holds the boundaries: a session opened from the panel works
// through a scoped account on one of the site's databases (read-only for a
// read-only caller); a signed-in database user works as itself, so the
// server's grants decide. Identifiers are looked up, values bound. The
// panel adds the session, the audit trail (every write recorded before it
// is sent, with its action, table and primary key, never row contents),
// and which files a session is shown.

// studioDB is the database the path names, or the refusal: one of the
// site's for a session opened from the panel; for a signed-in user any
// name of the right shape, which the server lets it open or not.
func (s *Server) studioDB(w http.ResponseWriter, r *http.Request, c *call) (string, bool) {
	name := r.PathValue("db")
	if !studioDBName.MatchString(name) {
		respondErr(w, http.StatusBadRequest, "invalid database name")
		return "", false
	}
	if c.Studio.mode == studioLogin {
		return name, true
	}
	if _, err := s.resolveDB(c.Site, name); err != nil {
		fail(w, err)
		return "", false
	}
	return name, true
}

// studioAgentErr answers a failed agent call. For a signed-in user, the
// agent no longer having its session, or the server no longer taking its
// password, ends the Quarry session too: 401, sign in again.
func (s *Server) studioAgentErr(w http.ResponseWriter, c *call, err error) {
	if c.Studio.mode == studioLogin && agentrpc.CodeOf(err) == agentrpc.CodeUnauthorized {
		s.endStudio(w, c.Studio)
		respondErr(w, http.StatusUnauthorized, agentrpc.Message(err, msgStudioSignIn))
		return
	}
	respondAgentErr(w, 0, "", err)
}

func (s *Server) handleStudioDatabases(w http.ResponseWriter, r *http.Request, c *call) {
	if c.Studio.mode == studioLogin {
		var res rpc.DBStudioDatabasesResult
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioDatabases, rpc.DBStudioLoginParams{Site: c.Site, Login: c.login()}, &res); err != nil {
			s.studioAgentErr(w, c, err)
			return
		}
		respond(w, http.StatusOK, res)
		return
	}
	list, err := s.Store.ListSiteDatabases(c.Site.ID)
	if err != nil {
		internalError(w, "ListSiteDatabases", err, "")
		return
	}
	out := rpc.DBStudioDatabasesResult{Databases: []string{}}
	for _, d := range list {
		out.Databases = append(out.Databases, d.Name)
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleStudioTables(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var res rpc.DBStudioTablesResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioTables, rpc.DBStudioTablesParams{
		Site: c.Site, Database: database, Login: c.login(), Columns: r.URL.Query().Get("columns") == "1",
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleStudioStructure(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var res rpc.DBStudioStructure
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioStructure, rpc.DBStudioTableParams{
		Site: c.Site, Database: database, Login: c.login(), Table: r.PathValue("table"),
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleStudioRows(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		Page     int            `json:"page"`
		PageSize int            `json:"page_size"`
		Sort     []rpc.DBSort   `json:"sort"`
		Filters  []rpc.DBFilter `json:"filters"`
		Search   string         `json:"search"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	var res rpc.DBStudioRowsResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioRows, rpc.DBStudioRowsParams{
		Site: c.Site, Database: database, Login: c.login(), Table: r.PathValue("table"),
		Page: req.Page, PageSize: req.PageSize, Sort: req.Sort, Filters: req.Filters, Search: req.Search,
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

// auditKey renders a change's primary key for the audit trail, cut short.
func auditKey(key map[string]string) string {
	names := make([]string, 0, len(key))
	for k := range key {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+key[k])
	}
	out := strings.Join(parts, ",")
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return out
}

// auditOp names a change in the audit trail; anything unknown (which the
// agent refuses) is recorded as such.
func auditOp(op string) string {
	switch op {
	case "insert", "update", "delete":
		return op
	}
	return "change"
}

// studioDetail is an audit detail of a Quarry write: the database, then
// what was done.
func studioDetail(database, rest string) string { return "db=" + database + " " + rest }

func (s *Server) handleStudioEdit(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		Changes []rpc.DBChange `json:"changes"`
		DryRun  bool           `json:"dry_run"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	table := r.PathValue("table")
	if !req.DryRun {
		for _, ch := range req.Changes {
			detail := "table=" + table
			if len(ch.Key) > 0 {
				detail += " key=" + auditKey(ch.Key)
			}
			s.audit(c.Actor(), "quarry."+auditOp(ch.Op), c.Site.Domain, studioDetail(database, detail))
		}
	}
	var res rpc.DBStudioEditResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioEdit, rpc.DBStudioEditParams{
		Site: c.Site, Database: database, Login: c.login(), Table: table, Changes: req.Changes,
		DryRun: req.DryRun, ReadOnly: c.studioReadOnly(),
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleStudioAlter(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		Kind    string           `json:"kind"`
		Column  *rpc.DBNewColumn `json:"column"`
		Index   *rpc.DBNewIndex  `json:"index"`
		Confirm string           `json:"confirm"`
		DryRun  bool             `json:"dry_run"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	table := r.PathValue("table")
	if !req.DryRun {
		action, detail := "quarry.alter", "table="+table
		switch {
		case req.Kind == "truncate" || req.Kind == "drop":
			action = "quarry." + req.Kind
		case req.Column != nil:
			detail += " add_column=" + req.Column.Name
		case req.Index != nil:
			detail += " add_index=" + req.Index.Name
		}
		s.audit(c.Actor(), action, c.Site.Domain, studioDetail(database, detail))
	}
	var res rpc.DBStudioAlterResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioAlter, rpc.DBStudioAlterParams{
		Site: c.Site, Database: database, Login: c.login(), Table: table, Kind: req.Kind, Column: req.Column,
		Index: req.Index, Confirm: req.Confirm, DryRun: req.DryRun, ReadOnly: c.studioReadOnly(),
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

// readStatement reports whether a statement only reads, by its first
// word: a signed-in user's other statements are audited (redacted).
func readStatement(stmt string) bool {
	word := strings.ToUpper(strings.TrimLeft(strings.Fields(stmt + " x")[0], "("))
	switch word {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH", "VALUES", "TABLE":
		return true
	}
	return false
}

// handleStudioQuery is the SQL editor: one statement at a time, capped and
// timed by the agent. From the panel it is read-only unless a manager
// allows writes; a signed-in user writes what its grants let it.
func (s *Server) handleStudioQuery(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		SQL         string `json:"sql"`
		AllowWrites bool   `json:"allow_writes"`
		Explain     bool   `json:"explain"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	stmt := strings.TrimSpace(req.SQL)
	if stmt == "" {
		respondErr(w, http.StatusBadRequest, "type a statement to run")
		return
	}
	if req.Explain {
		stmt = "EXPLAIN " + stmt
	}
	readOnly := true
	switch {
	case c.Studio.mode == studioLogin:
		readOnly = false
		if !readStatement(stmt) {
			s.audit(c.Actor(), "database.write", c.Site.Domain, studioDetail(database, redactForAudit(stmt)))
		}
	case req.AllowWrites && c.studioReadOnly():
		respondErr(w, http.StatusForbidden, "this is a read-only account")
		return
	case req.AllowWrites:
		readOnly = false
		s.audit(c.Actor(), "database.write", c.Site.Domain, studioDetail(database, redactForAudit(stmt)))
	}
	var res rpc.DBQueryResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioQuery, rpc.DBStudioQueryParams{
		Site: c.Site, Database: database, Login: c.login(), SQL: stmt, ReadOnly: readOnly,
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, struct {
		rpc.DBQueryResult
		ReadOnly bool `json:"read_only"`
	}{res, readOnly})
}

// handleStudioSimulate runs a write statement in a transaction the agent
// always rolls back (ops/dbstudio SPEC §12). Nothing stays, so it needs no
// allow_writes, but a read-only panel account may not; a signed-in user's
// grants decide on the server. Audited database.simulate, before the call.
func (s *Server) handleStudioSimulate(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		SQL string `json:"sql"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	stmt := strings.TrimSpace(req.SQL)
	if stmt == "" {
		respondErr(w, http.StatusBadRequest, "type a statement to simulate")
		return
	}
	if c.Studio.mode != studioLogin && c.studioReadOnly() {
		respondErr(w, http.StatusForbidden, "this is a read-only account")
		return
	}
	s.audit(c.Actor(), "database.simulate", c.Site.Domain, studioDetail(database, redactForAudit(stmt)))
	var res rpc.DBStudioSimulateResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioSimulate, rpc.DBStudioSimulateParams{
		Site: c.Site, Database: database, Login: c.login(), SQL: stmt,
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleStudioSearch(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		Term   string   `json:"term"`
		Tables []string `json:"tables"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	var res rpc.DBStudioSearchResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioSearch, rpc.DBStudioSearchParams{
		Site: c.Site, Database: database, Login: c.login(), Term: req.Term, Tables: req.Tables,
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

// studioJob starts a Quarry task and lets the session follow it. The task
// comes back named in the reader's language: a Quarry session has no panel
// account to read GET /api/tasks/names with.
func (s *Server) studioJob(w http.ResponseWriter, r *http.Request, c *call, req jobReq, fn jobWork) {
	task, err := s.startJob(req, fn)
	if err != nil {
		respondJobErr(w, err)
		return
	}
	c.Studio.addTask(task.ID)
	localizeTask(&task, readerLanguage(r, c))
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

func (s *Server) handleStudioExport(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		Format string   `json:"format"`
		Tables []string `json:"tables"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Format != "sql" && req.Format != "csv" {
		respondErr(w, http.StatusBadRequest, "the format is sql or csv")
		return
	}
	if req.Format == "csv" && len(req.Tables) != 1 {
		respondErr(w, http.StatusBadRequest, "a CSV export is of one table")
		return
	}
	st, login := c.Studio, c.login()
	s.audit(c.Actor(), "quarry.export", c.Site.Domain, studioDetail(database, fmt.Sprintf("format=%s tables=%s", req.Format, strings.Join(req.Tables, ","))))
	s.studioJob(w, r, c, jobReq{kind: "dbstudio.export", siteID: c.Site.ID}, func(ctx context.Context, site state.Site, progress func(int, string)) error {
		progress(20, "Exporting "+database)
		var res rpc.DBStudioExportResult
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.DBStudioExport, rpc.DBStudioExportParams{
			Site: site, Database: database, Login: login, Format: req.Format, Tables: req.Tables,
		}, &res); err != nil {
			return err
		}
		s.studio.own(st, "exports", res.Name)
		progress(100, fmt.Sprintf("Exported %s (%d bytes)", res.Name, res.SizeBytes))
		return nil
	})
}

// handleStudioFiles lists the exports and uploads the session may see: all
// of the site's from the panel, a signed-in user's own otherwise.
func (s *Server) handleStudioFiles(w http.ResponseWriter, r *http.Request, c *call) {
	var res rpc.DBStudioFilesResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioFiles, rpc.DBStudioSiteParams{Site: c.Site}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	keep := func(dir string, list []rpc.DBStudioFile) []rpc.DBStudioFile {
		out := []rpc.DBStudioFile{}
		for _, f := range list {
			if c.Studio.owns(dir, f.Name) {
				out = append(out, f)
			}
		}
		return out
	}
	res.Exports, res.Imports = keep("exports", res.Exports), keep("imports", res.Imports)
	respond(w, http.StatusOK, res)
}

// dbFileRe is the agent's rule for a Quarry file name, checked here too
// so a bad name never reaches a header or a task.
var dbFileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,150}\.(sql|csv|SQL|CSV)$`)

// downloadChunk is one read of a streamed download: the API never holds
// more than this of a file, whatever its size.
const downloadChunk = 4 << 20

// handleStudioDownload streams an export to the browser, a chunk at a time
// from the agent.
func (s *Server) handleStudioDownload(w http.ResponseWriter, r *http.Request, c *call) {
	name := r.PathValue("name")
	if !dbFileRe.MatchString(name) {
		respondErr(w, http.StatusBadRequest, "invalid file name")
		return
	}
	if !c.Studio.owns("exports", name) {
		respondErr(w, http.StatusNotFound, "no such file")
		return
	}
	read := func(off int64) (rpc.DBStudioChunkResult, error) {
		var res rpc.DBStudioChunkResult
		err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioReadChunk, rpc.DBStudioChunkParams{
			Site: c.Site, Name: name, Offset: off, Length: downloadChunk,
		}, &res)
		return res, err
	}
	first, err := read(0)
	if err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	s.audit(c.Actor(), "quarry.download", c.Site.Domain, name)
	ctype := "application/sql"
	if strings.HasSuffix(strings.ToLower(name), ".csv") {
		ctype = "text/csv"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	h.Set("Content-Length", strconv.FormatInt(first.Size, 10))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	size, off, chunk := first.Size, int64(0), first
	for {
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		off += int64(len(chunk.Data))
		if off >= size || len(chunk.Data) == 0 {
			return
		}
		if chunk, err = read(off); err != nil {
			// The headers are gone: a short body is how the browser learns
			// the download failed.
			if s.Log != nil {
				s.Log.Warn("an export download stopped", "site", c.Site.Domain, "file", name, "offset", off, "err", err)
			}
			return
		}
	}
}

func (s *Server) handleStudioDeleteFile(w http.ResponseWriter, r *http.Request, c *call) {
	dir, name := r.PathValue("dir"), r.PathValue("name")
	if !c.Studio.owns(dir, name) {
		respondErr(w, http.StatusNotFound, "no such file")
		return
	}
	var out map[string]string
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioDeleteFile, rpc.DBStudioDeleteParams{
		Site: c.Site, Dir: dir, Name: name,
	}, &out); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	c.Studio.dropFile(dir, name)
	s.audit(c.Actor(), "quarry.delete_file", c.Site.Domain, dir+"/"+name)
	respond(w, http.StatusOK, out)
}

// uploadChunk is the largest piece of an upload one request carries: the
// panel's nginx accepts 16 MiB bodies, and the browser sends a file of any
// size as a sequence of these.
const uploadChunk = 8 << 20

// handleStudioUpload stores one chunk of an upload in shared/imports: the
// raw bytes in the body, the file's name and the chunk's offset in the
// query. A signed-in user may not replace a file it did not upload.
func (s *Server) handleStudioUpload(w http.ResponseWriter, r *http.Request, c *call) {
	name := r.URL.Query().Get("name")
	if !dbFileRe.MatchString(name) {
		respondErr(w, http.StatusBadRequest, "invalid file name: letters, digits, . _ and -, ending in .sql or .csv")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		respondErr(w, http.StatusBadRequest, "invalid offset")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		respondErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("send at most %d MiB per chunk", uploadChunk>>20))
		return
	}
	if !c.Studio.owns("imports", name) {
		if offset != 0 {
			respondErr(w, http.StatusNotFound, "no such upload: start it again")
			return
		}
		var files rpc.DBStudioFilesResult
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioFiles, rpc.DBStudioSiteParams{Site: c.Site}, &files); err != nil {
			s.studioAgentErr(w, c, err)
			return
		}
		for _, f := range files.Imports {
			if f.Name == name {
				respondErr(w, http.StatusConflict, "a file of that name is already there: rename yours")
				return
			}
		}
		// Another signed-in user may be starting the same name right now
		// (both found no file): only one of them gets it, or the other
		// could replace this upload and have it imported with this
		// user's grants.
		if !s.studio.claim(c.Studio, "imports", name) {
			respondErr(w, http.StatusConflict, "a file of that name is already there: rename yours")
			return
		}
	}
	if offset == 0 {
		s.audit(c.Actor(), "quarry.upload", c.Site.Domain, name)
	}
	var res rpc.DBStudioUploadResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioUploadChunk, rpc.DBStudioUploadParams{
		Site: c.Site, Name: name, Offset: offset, Data: data,
	}, &res); err != nil {
		s.studioAgentErr(w, c, err)
		return
	}
	respond(w, http.StatusOK, res)
}

// handleStudioImport imports an upload into the database, as a task: a
// .sql file (the database's name typed to confirm) through the sandboxed
// client — from the panel with a rollback point (DBImport), as a signed-in
// user as that user (DBStudioImportSQL) —, a .csv file into one table in
// one transaction.
func (s *Server) handleStudioImport(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.studioDB(w, r, c)
	if !ok {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Format  string `json:"format"`
		Table   string `json:"table"`
		Confirm string `json:"confirm"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if !dbFileRe.MatchString(req.Name) || !strings.HasSuffix(strings.ToLower(req.Name), "."+req.Format) {
		respondErr(w, http.StatusBadRequest, "choose an uploaded .sql or .csv file of the same format")
		return
	}
	if !c.Studio.owns("imports", req.Name) {
		respondErr(w, http.StatusNotFound, "no such file")
		return
	}
	login, readOnly := c.login(), c.studioReadOnly()
	var run func(ctx context.Context, site state.Site) (string, error)
	switch req.Format {
	case "sql":
		if req.Confirm != database {
			respondErr(w, http.StatusBadRequest, "type the database's name to confirm the import")
			return
		}
		run = func(ctx context.Context, site state.Site) (string, error) {
			var res rpc.DBImportResult
			var err error
			if login != "" {
				err = agentrpc.Into(ctx, s.Agent, agentrpc.DBStudioImportSQL, rpc.DBStudioImportSQLParams{
					Site: site, Database: database, Login: login, Name: req.Name,
				}, &res)
			} else {
				err = agentrpc.Into(ctx, s.Agent, agentrpc.DBImport, rpc.DBImportParams{
					Site: site, Database: database, RelPath: "shared/imports/" + req.Name,
				}, &res)
			}
			if err != nil || len(res.Warnings) == 0 {
				return "", err
			}
			return "Warning: " + strings.Join(res.Warnings, "; "), nil
		}
	case "csv":
		if req.Table == "" {
			respondErr(w, http.StatusBadRequest, "choose the table to import into")
			return
		}
		run = func(ctx context.Context, site state.Site) (string, error) {
			return "", agentrpc.Into(ctx, s.Agent, agentrpc.DBStudioImportCSV, rpc.DBStudioImportCSVParams{
				Site: site, Database: database, Login: login, Table: req.Table, Name: req.Name, ReadOnly: readOnly,
			}, nil)
		}
	default:
		respondErr(w, http.StatusBadRequest, "the format is sql or csv")
		return
	}
	detail := req.Name
	if req.Format == "csv" {
		detail += " table=" + req.Table
	}
	s.audit(c.Actor(), "quarry.import", c.Site.Domain, studioDetail(database, detail))
	s.studioJob(w, r, c, jobReq{kind: "dbstudio.import", siteID: c.Site.ID, locks: siteLocks(c.Site.ID)}, func(ctx context.Context, site state.Site, progress func(int, string)) error {
		progress(15, "Importing "+req.Name)
		warned, err := run(ctx, site)
		if err != nil {
			return err
		}
		if warned != "" {
			progress(95, warned)
		}
		progress(100, "Imported "+req.Name)
		return nil
	})
}

// handleStudioTask is one of the tasks the session started.
func (s *Server) handleStudioTask(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !c.Studio.hasTask(id) {
		respondErr(w, http.StatusNotFound, "task not found")
		return
	}
	j, err := s.Store.GetJob(id)
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		internalError(w, "GetJob", err, "could not read the task")
		return
	}
	respond(w, http.StatusOK, s.taskView(j, readerLanguage(r, c)))
}
