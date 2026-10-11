package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The site's database tools: the table list, the SQL console, export and
// import (SPEC.md §6.3), each on one of the site's databases (the request's
// `database`, "" its primary). The agent runs each statement as an account
// granted on that database alone, and read-only mode is an account with
// SELECT and SHOW VIEW, so the server refuses a write however it is
// spelled; the panel decides who may ask and records the writes. The
// site's databases and accounts themselves are site_databases.go.

var errNoManagedDB = decline(http.StatusPreconditionFailed, "this site has no local managed database")

// localDB reports whether the site has a primary database the agent
// manages: the one its application is configured with.
func localDB(site state.Site) bool {
	return site.Config.Database.Enabled && !site.Config.Database.External
}

// managedDB answers 412 for a site without a primary database the agent
// manages, and reports whether the request may go on (the WordPress tools,
// which work on the primary).
func managedDB(w http.ResponseWriter, site state.Site) bool {
	if !localDB(site) {
		fail(w, errNoManagedDB)
		return false
	}
	return true
}

// resolveDB is the site's database a request names: requested when it is
// one of the site's, its primary when requested is "".
func (s *Server) resolveDB(site state.Site, requested string) (string, error) {
	if requested == "" {
		if !localDB(site) {
			return "", errNoManagedDB
		}
		return site.Config.Database.Name, nil
	}
	if _, err := s.Store.SiteDatabaseByName(site.ID, requested); errors.Is(err, state.ErrNotFound) {
		return "", decline(http.StatusNotFound, "this site has no database %q", requested)
	} else if err != nil {
		return "", stored(err)
	}
	return requested, nil
}

// requestDB resolves the request's ?database= (GET database), or
// answers the refusal.
func (s *Server) requestDB(w http.ResponseWriter, r *http.Request, site state.Site) (string, bool) {
	name, err := s.resolveDB(site, r.URL.Query().Get("database"))
	if err != nil {
		fail(w, err)
		return "", false
	}
	return name, true
}

// siteDB is the database tools' service.
type siteDB struct{ s *Server }

func (s *Server) db() siteDB { return siteDB{s} }

// dbOverview is GET /database: the database, its account, its tables both
// as the console's grid (shown before any query runs) and as a list.
type dbOverview struct {
	Database string            `json:"database"`
	User     string            `json:"user"`
	Tables   rpc.DBQueryResult `json:"tables"`
	List     []rpc.DBTable     `json:"list"`
}

func (d siteDB) overview(ctx context.Context, site state.Site, database string) (dbOverview, error) {
	res, err := agentrpc.Call(ctx, d.s.Agent, agentrpc.DBTables, rpc.DBTablesParams{Site: site, Database: database})
	if err != nil {
		return dbOverview{}, err
	}
	grid := rpc.DBQueryResult{Columns: []string{"Table", "Rows", "Data MB"}, Rows: [][]string{}, Truncated: res.Truncated}
	for _, t := range res.Tables {
		mb := strconv.FormatFloat(float64(t.DataBytes)/(1<<20), 'f', 2, 64)
		grid.Rows = append(grid.Rows, []string{t.Name, strconv.FormatInt(t.Rows, 10), mb})
	}
	if len(grid.Rows) == 0 {
		grid.Message = "The database has no tables yet."
	}
	user := ""
	if database == site.Config.Database.Name {
		user = site.Config.Database.User
	}
	return dbOverview{Database: database, User: user, Tables: grid, List: res.Tables}, nil
}

// query runs one console statement, read-only unless writes are allowed.
// A write is audited before it is sent: it must be on record even if the
// answer never comes back.
func (d siteDB) query(ctx context.Context, actor string, site state.Site, database, sql string, allowWrites bool) (rpc.DBQueryResult, error) {
	if strings.TrimSpace(sql) == "" {
		return rpc.DBQueryResult{}, decline(http.StatusBadRequest, "type a statement to run")
	}
	if allowWrites {
		d.s.audit(actor, "database.write", site.Domain, database+": "+redactForAudit(sql))
	}
	return agentrpc.Call(ctx, d.s.Agent, agentrpc.DBQuery, rpc.DBQueryParams{
		Site: site, Database: database, SQL: sql, ReadOnly: !allowWrites,
	})
}

// export dumps the database into the site's shared/exports.
func (d siteDB) export(site state.Site, database string) (jobs.Task, error) {
	agent := d.s.Agent
	return d.s.runJob(jobReq{kind: "database.export", siteID: site.ID}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		step(30, "Exporting "+database)
		res, err := agentrpc.Call(ctx, agent, agentrpc.DBExport, rpc.DBExportParams{Site: site, Database: database})
		if err != nil {
			return err
		}
		step(100, "Exported to shared/exports/"+path.Base(res.Path)+" (browse it in Files)")
		return nil
	})
}

// replace imports a .sql file of the site's tree over one of its
// databases; the agent saves a rollback point first. confirm must be the
// site's domain.
func (d siteDB) replace(site state.Site, database, file, confirm string) (jobs.Task, error) {
	if confirm != site.Domain {
		return jobs.Task{}, decline(http.StatusBadRequest, "type the site domain to confirm database replacement")
	}
	file = strings.TrimSpace(file)
	if file == "" {
		return jobs.Task{}, decline(http.StatusBadRequest, "SQL file path is required")
	}
	agent := d.s.Agent
	return d.s.runJob(jobReq{kind: "database.import", siteID: site.ID, locks: siteLocks(site.ID)}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		step(15, "Saving a rollback point, then importing "+file+" into "+database)
		var res rpc.DBImportResult
		if err := agentrpc.Into(ctx, agent, agentrpc.DBImport, rpc.DBImportParams{
			Site: site, Database: database, RelPath: file,
		}, &res); err != nil {
			return err
		}
		if len(res.Warnings) > 0 {
			step(95, "Warning: "+strings.Join(res.Warnings, "; "))
		}
		step(100, "Database imported from "+file)
		return nil
	})
}

func (s *Server) handleDatabaseInfo(w http.ResponseWriter, r *http.Request, c *call) {
	database, ok := s.requestDB(w, r, c.Site)
	if !ok {
		return
	}
	res, err := s.db().overview(r.Context(), c.Site, database)
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleDatabaseQuery(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Database    string `json:"database"`
		SQL         string `json:"sql"`
		AllowWrites bool   `json:"allow_writes"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	database, err := s.resolveDB(c.Site, req.Database)
	if err != nil {
		fail(w, err)
		return
	}
	res, err := s.db().query(r.Context(), c.Actor(), c.Site, database, req.SQL, req.AllowWrites)
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleDatabaseExport(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Database string `json:"database"`
	}
	// The body is optional: {} or none exports the primary.
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	database, err := s.resolveDB(c.Site, req.Database)
	if err != nil {
		fail(w, err)
		return
	}
	task, err := s.db().export(c.Site, database)
	if err == nil {
		s.audit(c.Actor(), "database.export", c.Site.Domain, database)
	}
	accepted(w, task, err)
}

func (s *Server) handleDatabaseImport(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Database string `json:"database"`
		Path     string `json:"path"`
		Confirm  string `json:"confirm"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	database, err := s.resolveDB(c.Site, req.Database)
	if err != nil {
		fail(w, err)
		return
	}
	task, err := s.db().replace(c.Site, database, req.Path, req.Confirm)
	if err == nil {
		s.audit(c.Actor(), "database.import", c.Site.Domain, database+": "+strings.TrimSpace(req.Path))
	}
	accepted(w, task, err)
}
