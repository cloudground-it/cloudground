package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's databases and database accounts (SPEC.md §6.4, Fase 9b): zero
// or more of each, an account granted all privileges or read only on some
// of the site's databases. The store records them (state "Site
// databases"); the agent makes them and keeps its own record of whose each
// name is, refusing anything else (ops/db SPEC §2). The primary database
// and account, which the site's application is configured with, cannot be
// deleted, and the primary account keeps all privileges on the primary
// database.

// Password bounds for a database account the panel is given.
const (
	minDBPassword = 12
	maxDBPassword = 256
)

// generatedDBPassword is a new account's password when none is chosen: 32
// characters.
func generatedDBPassword() string { return randomToken(24) }

// dbDatabaseView is one database in GET /databases.
type dbDatabaseView struct {
	Name      string    `json:"name"`
	ServerID  int64     `json:"server_id"`
	Primary   bool      `json:"primary"`
	SizeBytes int64     `json:"size_bytes"`
	Tables    int64     `json:"tables"`
	CreatedAt time.Time `json:"created_at"`
}

// dbUserView is one account in GET /databases.
type dbUserView struct {
	Name      string          `json:"name"`
	ServerID  int64           `json:"server_id"`
	Primary   bool            `json:"primary"`
	Grants    []state.DBGrant `json:"grants"`
	CreatedAt time.Time       `json:"created_at"`
}

// dbListView is GET /api/sites/{id}/databases.
type dbListView struct {
	Databases []dbDatabaseView `json:"databases"`
	Users     []dbUserView     `json:"users"`
	// Servers are the connections the site's databases use, so the page can
	// say where an application connects (id 0: this server).
	Servers []dbServerRef `json:"servers"`
	// Available are the registered servers a database may be put on (name
	// only: a person who manages the site picks one, the host is the
	// administrator's).
	Available  []dbServerChoice `json:"available_servers"`
	StatsError string           `json:"stats_error,omitempty"`
}

// dbServerChoice is a server to pick.
type dbServerChoice struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// dbServerRef is where a server is, for an application's configuration.
type dbServerRef struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	TLS  string `json:"tls"`
}

// siteDatabases is the service.
type siteDatabases struct{ s *Server }

func (s *Server) databases() siteDatabases { return siteDatabases{s} }

func primaryDatabase(site state.Site, name string) bool {
	return localDB(site) && site.Config.Database.Name == name
}

func primaryUser(site state.Site, name string) bool {
	return localDB(site) && site.Config.Database.User == name
}

// list is the site's databases with their sizes (from the agent; -1 when
// it cannot say) and its accounts with their grants.
func (d siteDatabases) list(ctx context.Context, site state.Site) (dbListView, error) {
	st := d.s.Store
	dbs, err := st.ListSiteDatabases(site.ID)
	if err != nil {
		return dbListView{}, stored(err)
	}
	users, err := st.ListDBUsers(site.ID)
	if err != nil {
		return dbListView{}, stored(err)
	}
	out := dbListView{Databases: []dbDatabaseView{}, Users: []dbUserView{}, Servers: []dbServerRef{}, Available: []dbServerChoice{}}
	if all, err := st.ListDBServers(); err == nil {
		for _, sv := range all {
			if sv.Kind == "postgres" && !siteval.SupportsPostgres(site.Type) {
				continue
			}
			out.Available = append(out.Available, dbServerChoice{ID: sv.ID, Name: sv.Name, Kind: sv.Kind})
		}
	}
	stats := map[string]rpc.DBInfo{}
	if len(dbs) > 0 {
		res, err := agentrpc.Call(ctx, d.s.Agent, agentrpc.ListDatabases, rpc.DBListParams{Site: site})
		switch {
		case err != nil:
			out.StatsError = "the sizes could not be read from the server"
		default:
			out.StatsError = res.StatsError
			for _, i := range res.Databases {
				stats[i.Name] = i
			}
		}
	}
	for _, db := range dbs {
		v := dbDatabaseView{Name: db.Name, ServerID: db.ServerID, Primary: primaryDatabase(site, db.Name), SizeBytes: -1, Tables: -1, CreatedAt: db.CreatedAt}
		if i, ok := stats[db.Name]; ok {
			v.SizeBytes, v.Tables = i.SizeBytes, i.Tables
		}
		out.Databases = append(out.Databases, v)
	}
	for _, u := range users {
		out.Users = append(out.Users, dbUserView{Name: u.Name, ServerID: u.ServerID, Primary: primaryUser(site, u.Name), Grants: u.Grants, CreatedAt: u.CreatedAt})
	}
	seen := map[int64]bool{}
	for _, id := range append(serverIDs(dbs), userServerIDs(users)...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		ref := dbServerRef{ID: id, Kind: "mariadb", Host: "localhost", TLS: state.DBServerTLSOff}
		if id != 0 {
			if sv, err := st.GetDBServer(id); err == nil {
				ref = dbServerRef{ID: id, Kind: sv.Kind, Name: sv.Name, Host: sv.Host, Port: sv.Port, TLS: sv.TLS}
			}
		}
		out.Servers = append(out.Servers, ref)
	}
	return out, nil
}

func serverIDs(dbs []state.SiteDatabase) []int64 {
	out := make([]int64, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, d.ServerID)
	}
	return out
}

func userServerIDs(us []state.DBUser) []int64 {
	out := make([]int64, 0, len(us))
	for _, u := range us {
		out = append(out, u.ServerID)
	}
	return out
}

// knownServer refuses a server id that is not the box's own or a registered
// one, and a PostgreSQL server for a site of a type that cannot use it.
func (d siteDatabases) knownServer(site state.Site, id int64) error {
	if id == 0 {
		return nil
	}
	if id < 0 {
		return decline(http.StatusBadRequest, "unknown database server")
	}
	sv, err := d.s.Store.GetDBServer(id)
	if errors.Is(err, state.ErrNotFound) {
		return decline(http.StatusBadRequest, "unknown database server")
	} else if err != nil {
		return stored(err)
	}
	if sv.Kind == "postgres" && !siteval.SupportsPostgres(site.Type) {
		return decline(http.StatusBadRequest, "PostgreSQL is offered for PHP and Laravel sites")
	}
	return nil
}

// grantsServer is the one server the grants' databases are on; ok is false
// when there is no grant. Databases on two servers are 400.
func (d siteDatabases) grantsServer(site state.Site, grants []state.DBGrant) (id int64, ok bool, err error) {
	for _, g := range grants {
		db, e := d.s.Store.SiteDatabaseByName(site.ID, g.Database)
		if e != nil {
			continue // checkGrants has said so
		}
		if ok && db.ServerID != id {
			return 0, false, decline(http.StatusBadRequest, "a user's databases must be on one server")
		}
		id, ok = db.ServerID, true
	}
	return id, ok, nil
}

// checkName refuses a name the site may not take.
func checkName(site state.Site, name string) error {
	if err := siteval.ClaimableBy(name, site.ID); err != nil {
		return decline(http.StatusBadRequest, "%s", err)
	}
	return nil
}

// createDB records a new database of the site and queues its creation. A
// name in use anywhere on the server is 409.
func (d siteDatabases) createDB(actor string, site state.Site, name string, serverID int64) (jobs.Task, error) {
	name = strings.TrimSpace(name)
	if err := checkName(site, name); err != nil {
		return jobs.Task{}, err
	}
	if err := d.knownServer(site, serverID); err != nil {
		return jobs.Task{}, err
	}
	st := d.s.Store
	if _, err := st.AddSiteDatabaseOn(site.ID, name, serverID); errors.Is(err, state.ErrDBNameTaken) {
		return jobs.Task{}, decline(http.StatusConflict, "a database called %s already exists on this server", name)
	} else if err != nil {
		return jobs.Task{}, stored(err)
	}
	agent := d.s.Agent
	task, err := d.s.runJob(jobReq{kind: "database.create", siteID: site.ID}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		step(20, "Creating the database "+name)
		if err := agentrpc.Into(ctx, agent, agentrpc.CreateDatabase, rpc.SiteDatabaseParams{Site: site, Name: name, ServerID: serverID}, nil); err != nil {
			d.s.logIfErr(st.RemoveSiteDatabase(site.ID, name), "RemoveSiteDatabase")
			return err
		}
		st.AnnounceDatabases(site.ID, name)
		step(100, "Database "+name+" created")
		return nil
	})
	if err != nil {
		d.s.logIfErr(st.RemoveSiteDatabase(site.ID, name), "RemoveSiteDatabase")
		return jobs.Task{}, err
	}
	d.s.audit(actor, "database.create", site.Domain, name)
	return task, nil
}

// dropDB queues the removal of one of the site's databases. confirm must
// be its name. Its backups stay: the snapshots are the site's, and
// retention removes them.
func (d siteDatabases) dropDB(actor string, site state.Site, name, confirm string) (jobs.Task, error) {
	if confirm != name {
		return jobs.Task{}, decline(http.StatusBadRequest, "type the database's name to confirm its deletion")
	}
	if _, err := d.s.Store.SiteDatabaseByName(site.ID, name); errors.Is(err, state.ErrNotFound) {
		return jobs.Task{}, decline(http.StatusNotFound, "this site has no database %q", name)
	} else if err != nil {
		return jobs.Task{}, stored(err)
	}
	if primaryDatabase(site, name) {
		return jobs.Task{}, decline(http.StatusConflict, "%s is the site's primary database, which its application uses", name)
	}
	st, agent := d.s.Store, d.s.Agent
	task, err := d.s.runJob(jobReq{kind: "database.drop", siteID: site.ID, locks: siteLocks(site.ID)}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		step(20, "Deleting the database "+name)
		if err := agentrpc.Into(ctx, agent, agentrpc.DropDatabase, rpc.SiteDatabaseParams{Site: site, Name: name}, nil); err != nil {
			return err
		}
		if err := st.RemoveSiteDatabase(site.ID, name); err != nil && !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("forget the database: %w", err)
		}
		step(100, "Database "+name+" deleted; its backups are kept until retention removes them")
		return nil
	})
	if err != nil {
		return jobs.Task{}, err
	}
	d.s.audit(actor, "database.delete", site.Domain, name)
	return task, nil
}

// password is the account's password to set: password itself, checked, or
// a generated one (generated says which).
func dbPassword(password string) (pw string, generated bool, err error) {
	if password == "" {
		return generatedDBPassword(), true, nil
	}
	if msg := dbPasswordProblem(password); msg != "" {
		return "", false, decline(http.StatusBadRequest, "%s", msg)
	}
	return password, false, nil
}

// dbPasswordProblem says what is wrong with a chosen database password, or
// "": the one rule of a site's creation (database_password) and of the
// Database tab (a new account, a new password). 12 to 256 characters, no
// control characters, none of ' " \ and no space at either end. The
// characters: wp-cli writes the primary account's password into
// wp-config.php and quotes neither right (measured with wp-cli 2.x:
// config create writes " as \", so WordPress gets another password than
// the account's; config set writes \' as \\', which ends the PHP string
// and lets the rest of the password run as code). The spaces: a password
// copied out of the summary or a text file easily loses them.
func dbPasswordProblem(pw string) string {
	switch {
	case len(pw) < minDBPassword || len(pw) > maxDBPassword:
		return fmt.Sprintf("the database password must be %d to %d characters", minDBPassword, maxDBPassword)
	case strings.ContainsFunc(pw, unicode.IsControl):
		return "the database password contains control characters"
	case strings.ContainsAny(pw, `'"\`):
		return `the database password may not contain ', " or \`
	case strings.TrimSpace(pw) != pw:
		return "the database password may not start or end with a space"
	}
	return ""
}

// checkGrants refuses a level other than all or read, a database granted
// twice and one the site does not have.
func (d siteDatabases) checkGrants(site state.Site, grants []state.DBGrant) error {
	seen := map[string]bool{}
	for _, g := range grants {
		if !siteval.ValidGrant(g.Level) {
			return decline(http.StatusBadRequest, "a grant is all or read, not %q", g.Level)
		}
		if seen[g.Database] {
			return decline(http.StatusBadRequest, "%s is granted twice", g.Database)
		}
		seen[g.Database] = true
		if _, err := d.s.resolveDB(site, g.Database); err != nil || g.Database == "" {
			return decline(http.StatusBadRequest, "this site has no database %q", g.Database)
		}
	}
	return nil
}

// newDBUser is POST /databases-users.
type newDBUser struct {
	Name     string          `json:"name"`
	Password string          `json:"password"`
	Grants   []state.DBGrant `json:"grants"`
	// ServerID is the server of an account with no grants; with grants it
	// is the server of their databases.
	ServerID int64 `json:"server_id"`
}

// dbUserCreated answers a new account; Password is set only when the panel
// generated it, and shown once.
type dbUserCreated struct {
	User     dbUserView `json:"user"`
	Password string     `json:"password,omitempty"`
}

// createUser makes one of the site's accounts. A name in use anywhere on
// the server is 409.
func (d siteDatabases) createUser(ctx context.Context, actor string, site state.Site, req newDBUser) (dbUserCreated, error) {
	name := strings.TrimSpace(req.Name)
	if err := checkName(site, name); err != nil {
		return dbUserCreated{}, err
	}
	pw, generated, err := dbPassword(req.Password)
	if err != nil {
		return dbUserCreated{}, err
	}
	if err := d.checkGrants(site, req.Grants); err != nil {
		return dbUserCreated{}, err
	}
	server := req.ServerID
	if gs, ok, err := d.grantsServer(site, req.Grants); err != nil {
		return dbUserCreated{}, err
	} else if ok {
		if req.ServerID != 0 && req.ServerID != gs {
			return dbUserCreated{}, decline(http.StatusBadRequest, "a user's databases must be on one server")
		}
		server = gs
	}
	if err := d.knownServer(site, server); err != nil {
		return dbUserCreated{}, err
	}
	st := d.s.Store
	u, err := st.AddDBUserOn(site.ID, name, state.DBUserSecret(site.ID, name), pw, req.Grants, server)
	if errors.Is(err, state.ErrDBNameTaken) {
		return dbUserCreated{}, decline(http.StatusConflict, "a database user called %s already exists on this server", name)
	}
	if err != nil {
		return dbUserCreated{}, stored(err)
	}
	if err := agentrpc.Into(ctx, d.s.Agent, agentrpc.CreateDBUser, rpc.DBUserParams{Site: site, User: name, Password: pw, Grants: req.Grants, ServerID: server}, nil); err != nil {
		d.s.logIfErr(st.RemoveDBUser(site.ID, name), "RemoveDBUser")
		return dbUserCreated{}, err
	}
	d.s.audit(actor, "database.user.create", site.Domain, name)
	out := dbUserCreated{User: dbUserView{Name: u.Name, ServerID: u.ServerID, Grants: u.Grants, CreatedAt: u.CreatedAt}}
	if generated {
		out.Password = pw
	}
	return out, nil
}

// user reads one of the site's accounts, 404 when it has none of that name.
func (d siteDatabases) user(site state.Site, name string) (state.DBUser, error) {
	u, err := d.s.Store.DBUserByName(site.ID, name)
	if errors.Is(err, state.ErrNotFound) {
		return state.DBUser{}, decline(http.StatusNotFound, "this site has no database user %q", name)
	}
	return u, stored(err)
}

// dropUser deletes one of the site's accounts. confirm must be its name.
func (d siteDatabases) dropUser(ctx context.Context, actor string, site state.Site, name, confirm string) error {
	if confirm != name {
		return decline(http.StatusBadRequest, "type the user's name to confirm its deletion")
	}
	if _, err := d.user(site, name); err != nil {
		return err
	}
	if primaryUser(site, name) {
		return decline(http.StatusConflict, "%s is the account the site's application signs in with", name)
	}
	if err := agentrpc.Into(ctx, d.s.Agent, agentrpc.DropDBUser, rpc.DBUserParams{Site: site, User: name}, nil); err != nil {
		return err
	}
	if err := d.s.Store.RemoveDBUser(site.ID, name); err != nil && !errors.Is(err, state.ErrNotFound) {
		return stored(err)
	}
	d.s.audit(actor, "database.user.delete", site.Domain, name)
	return nil
}

// setPassword gives one of the site's accounts a new password, chosen or
// generated; the generated one is returned, once. The primary account of a
// WordPress site gets it in wp-config.php too (the agent's).
func (d siteDatabases) setPassword(ctx context.Context, actor string, site state.Site, name, password string) (string, error) {
	if _, err := d.user(site, name); err != nil {
		return "", err
	}
	pw, generated, err := dbPassword(password)
	if err != nil {
		return "", err
	}
	if err := agentrpc.Into(ctx, d.s.Agent, agentrpc.SetDBPassword, rpc.DBUserParams{Site: site, User: name, Password: pw}, nil); err != nil {
		return "", err
	}
	if err := d.s.Store.SetDBUserPassword(site.ID, name, pw); err != nil {
		return "", stored(err)
	}
	d.s.audit(actor, "database.user.password", site.Domain, name)
	if generated {
		return pw, nil
	}
	return "", nil
}

// setGrants replaces one of the site's accounts' grants.
func (d siteDatabases) setGrants(ctx context.Context, actor string, site state.Site, name string, grants []state.DBGrant) (dbUserView, error) {
	user, err := d.user(site, name)
	if err != nil {
		return dbUserView{}, err
	}
	if err := d.checkGrants(site, grants); err != nil {
		return dbUserView{}, err
	}
	if gs, ok, err := d.grantsServer(site, grants); err != nil {
		return dbUserView{}, err
	} else if ok && gs != user.ServerID {
		return dbUserView{}, decline(http.StatusBadRequest, "a user's databases must be on the user's server")
	}
	if primaryUser(site, name) {
		keeps := false
		for _, g := range grants {
			keeps = keeps || g.Database == site.Config.Database.Name && g.Level == siteval.GrantAll
		}
		if !keeps {
			return dbUserView{}, decline(http.StatusBadRequest, "%s must keep all privileges on %s, the site's primary database", name, site.Config.Database.Name)
		}
	}
	if err := agentrpc.Into(ctx, d.s.Agent, agentrpc.SetDBGrants, rpc.DBUserParams{Site: site, User: name, Grants: grants}, nil); err != nil {
		return dbUserView{}, err
	}
	u, err := d.s.Store.SetDBUserGrants(site.ID, name, grants)
	if err != nil {
		return dbUserView{}, stored(err)
	}
	d.s.audit(actor, "database.user.grants", site.Domain, name)
	return dbUserView{Name: u.Name, ServerID: u.ServerID, Primary: primaryUser(site, u.Name), Grants: u.Grants, CreatedAt: u.CreatedAt}, nil
}

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request, c *call) {
	res, err := s.databases().list(r.Context(), c.Site)
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Name     string `json:"name"`
		ServerID int64  `json:"server_id"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	task, err := s.databases().createDB(c.Actor(), c.Site, req.Name, req.ServerID)
	accepted(w, task, err)
}

func (s *Server) handleDropDatabase(w http.ResponseWriter, r *http.Request, c *call) {
	task, err := s.databases().dropDB(c.Actor(), c.Site, r.PathValue("name"), r.URL.Query().Get("confirm"))
	accepted(w, task, err)
}

func (s *Server) handleCreateDBUser(w http.ResponseWriter, r *http.Request, c *call) {
	var req newDBUser
	if !decodeOr400(w, r, &req) {
		return
	}
	res, err := s.databases().createUser(r.Context(), c.Actor(), c.Site, req)
	answer(w, http.StatusCreated, res, err)
}

func (s *Server) handleDropDBUser(w http.ResponseWriter, r *http.Request, c *call) {
	name := r.PathValue("name")
	err := s.databases().dropUser(r.Context(), c.Actor(), c.Site, name, r.URL.Query().Get("confirm"))
	answer(w, http.StatusOK, map[string]string{"deleted": name}, err)
}

func (s *Server) handleSetDBUserPassword(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	pw, err := s.databases().setPassword(r.Context(), c.Actor(), c.Site, r.PathValue("name"), req.Password)
	answer(w, http.StatusOK, map[string]string{"password": pw}, err)
}

func (s *Server) handleSetDBUserGrants(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Grants []state.DBGrant `json:"grants"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	u, err := s.databases().setGrants(r.Context(), c.Actor(), c.Site, r.PathValue("name"), req.Grants)
	answer(w, http.StatusOK, map[string]any{"user": u}, err)
}
