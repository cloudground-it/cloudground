package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// External database servers (SPEC.md §6.5.1, issue #30): other MariaDB or
// MySQL servers a site's databases may be on. An administrator registers
// them; the agent keeps their connections in memory only, and this file
// hands them over whenever they change, when the API starts, and when the
// agent says it does not have one.

var dbServerNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// maskedPassword stands for a stored secret in a request: it keeps it.
const maskedPassword = "••••••••"

// dbServerView is a server as the routes answer: never its password.
type dbServerView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Backup      string `json:"backup"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLS         string `json:"tls"`
	CACert      string `json:"ca_cert"`
	AdminUser   string `json:"admin_user"`
	PasswordSet bool   `json:"password_set"`
	Databases   int    `json:"databases"`
}

func (s *Server) dbServerView(d state.DBServer) dbServerView {
	pw, _ := s.Store.DBServerPassword(d.ID)
	return dbServerView{ID: d.ID, Name: d.Name, Kind: d.Kind, Backup: d.Backup, Host: d.Host, Port: d.Port, TLS: d.TLS, CACert: d.CACert,
		AdminUser: d.AdminUser, PasswordSet: pw != "", Databases: d.Databases}
}

// dbServerRequest is the body of a registration or a change; a field left
// out keeps its value (a change) or its default (a registration).
type dbServerRequest struct {
	Kind          *string `json:"kind"`
	Backup        *string `json:"backup"`
	Name          *string `json:"name"`
	Host          *string `json:"host"`
	Port          *int    `json:"port"`
	TLS           *string `json:"tls"`
	CACert        *string `json:"ca_cert"`
	AdminUser     *string `json:"admin_user"`
	AdminPassword *string `json:"admin_password"`
}

// apply puts the request's fields over d, and returns the password to
// connect with ("" when the request names none).
func (r dbServerRequest) apply(d *state.DBServer) (password string) {
	if r.Backup != nil {
		d.Backup = strings.TrimSpace(*r.Backup)
	}
	if r.Name != nil {
		d.Name = strings.TrimSpace(*r.Name)
	}
	if r.Host != nil {
		d.Host = strings.TrimSpace(*r.Host)
	}
	if r.Port != nil {
		d.Port = *r.Port
	}
	if r.TLS != nil {
		d.TLS = strings.TrimSpace(*r.TLS)
	}
	if r.CACert != nil {
		d.CACert = strings.TrimSpace(*r.CACert)
	}
	if r.AdminUser != nil {
		d.AdminUser = strings.TrimSpace(*r.AdminUser)
	}
	if r.AdminPassword != nil && *r.AdminPassword != maskedPassword {
		return *r.AdminPassword
	}
	return ""
}

// checkDBServer refuses what the rules of §6.5.1 do not allow.
func checkDBServer(d state.DBServer, password string) error {
	if password == "" {
		password = "x" // an update that keeps the stored one
	}
	switch {
	case !dbServerNameRe.MatchString(d.Name):
		return decline(http.StatusBadRequest, "the name is 2 to 32 lowercase letters, digits and hyphens, starting with a letter")
	case d.TLS != state.DBServerTLSRequired && d.TLS != state.DBServerTLSOff:
		return decline(http.StatusBadRequest, "tls is required or off")
	case d.Kind != rpc.KindMariaDB && d.Kind != rpc.KindPostgres:
		return decline(http.StatusBadRequest, "kind is mariadb or postgres")
	case d.Kind == rpc.KindPostgres && d.TLS != state.DBServerTLSRequired:
		return decline(http.StatusBadRequest, "PostgreSQL needs TLS")
	case d.Backup != state.DBServerBackupDump && d.Backup != state.DBServerBackupProvider:
		return decline(http.StatusBadRequest, "backup is dump or provider")
	case d.Kind == rpc.KindMariaDB && d.Backup != state.DBServerBackupDump:
		return decline(http.StatusBadRequest, "a MariaDB server is always dumped by a backup")
	}
	if err := (&rpc.DBServerParams{Host: d.Host, Port: d.Port, AdminUser: d.AdminUser, AdminPassword: password, CACert: d.CACert}).Validate(); err != nil {
		return decline(http.StatusBadRequest, "%s", err)
	}
	return nil
}

// agentParams is what the agent connects with.
func agentParams(d state.DBServer, password string) rpc.DBServerParams {
	return rpc.DBServerParams{Kind: d.Kind, BackupDumps: d.Kind != rpc.KindPostgres || d.Backup == state.DBServerBackupDump, ID: d.ID, Host: d.Host, Port: d.Port, TLS: d.TLS == state.DBServerTLSRequired,
		CACert: d.CACert, AdminUser: d.AdminUser, AdminPassword: password}
}

// testDBServer has the agent connect with the credentials: a refusal is the
// server's own sentence (400); an agent that does not answer is 502.
func (s *Server) testDBServer(ctx context.Context, d state.DBServer, password string) (rpc.DBServerTestResult, error) {
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.TestDBServer, agentParams(d, password))
	var ae *agentrpc.Error
	if errors.As(err, &ae) && ae.Code == agentrpc.CodeInvalid {
		return res, decline(http.StatusBadRequest, "%s", ae.Message)
	}
	return res, err
}

// syncDBServers gives the agent the servers, with their passwords opened:
// the whole list, which replaces what it had.
func (s *Server) syncDBServers(ctx context.Context) error {
	list, err := s.Store.ListDBServers()
	if err != nil {
		return err
	}
	p := rpc.SetDBServersParams{Servers: []rpc.DBServerParams{}}
	for _, d := range list {
		pw, err := s.Store.DBServerPassword(d.ID)
		if err != nil || pw == "" {
			s.logIfErr(err, "DBServerPassword")
			continue // the agent will say it does not know this one
		}
		p.Servers = append(p.Servers, agentParams(d, pw))
	}
	return agentrpc.Into(ctx, s.rawAgent(), agentrpc.SetDBServers, p, nil)
}

func (s *Server) handleListDBServers(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.Store.ListDBServers()
	if err != nil {
		internalError(w, "ListDBServers", err, "")
		return
	}
	out := make([]dbServerView, 0, len(list))
	for _, d := range list {
		out = append(out, s.dbServerView(d))
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) dbServerFromPath(w http.ResponseWriter, r *http.Request) (state.DBServer, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		respondErr(w, http.StatusNotFound, "database server not found")
		return state.DBServer{}, false
	}
	d, err := s.Store.GetDBServer(id)
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "database server not found")
		return d, false
	}
	if err != nil {
		internalError(w, "GetDBServer", err, "")
		return d, false
	}
	return d, true
}

func (s *Server) handleCreateDBServer(w http.ResponseWriter, r *http.Request, c *call) {
	var req dbServerRequest
	if !decodeOr400(w, r, &req) {
		return
	}
	d := state.DBServer{Kind: rpc.KindMariaDB, Port: 3306, TLS: state.DBServerTLSRequired, Backup: state.DBServerBackupDump}
	if req.Kind != nil && strings.TrimSpace(*req.Kind) == rpc.KindPostgres {
		d.Kind, d.Port, d.Backup = rpc.KindPostgres, 5432, state.DBServerBackupProvider
	} else if req.Kind != nil && strings.TrimSpace(*req.Kind) != rpc.KindMariaDB && strings.TrimSpace(*req.Kind) != "" {
		respondErr(w, http.StatusBadRequest, "kind is mariadb or postgres")
		return
	}
	pw := req.apply(&d)
	if err := checkDBServer(d, pw); err != nil {
		fail(w, err)
		return
	}
	if pw == "" {
		respondErr(w, http.StatusBadRequest, "the administrator's password is required")
		return
	}
	if _, err := s.testDBServer(r.Context(), d, pw); err != nil {
		fail(w, err)
		return
	}
	name := d.Name
	d, err := s.Store.CreateDBServer(d, pw)
	if errors.Is(err, state.ErrDBServerNameTaken) {
		respondErr(w, http.StatusConflict, "a database server called "+name+" already exists")
		return
	}
	if err != nil {
		internalError(w, "CreateDBServer", err, "")
		return
	}
	s.logIfErr(s.syncDBServers(r.Context()), "syncDBServers")
	s.audit(c.Actor(), "dbserver.create", d.Name, "")
	respond(w, http.StatusCreated, s.dbServerView(d))
}

func (s *Server) handleUpdateDBServer(w http.ResponseWriter, r *http.Request, c *call) {
	d, ok := s.dbServerFromPath(w, r)
	if !ok {
		return
	}
	var req dbServerRequest
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Kind != nil && strings.TrimSpace(*req.Kind) != d.Kind {
		respondErr(w, http.StatusBadRequest, "the kind of a database server cannot change")
		return
	}
	pw := req.apply(&d)
	if err := checkDBServer(d, pw); err != nil {
		fail(w, err)
		return
	}
	connect := pw
	if connect == "" {
		var err error
		if connect, err = s.Store.DBServerPassword(d.ID); err != nil || connect == "" {
			internalError(w, "DBServerPassword", err, "the server's password could not be read")
			return
		}
	}
	if _, err := s.testDBServer(r.Context(), d, connect); err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.UpdateDBServer(d, pw); errors.Is(err, state.ErrDBServerNameTaken) {
		respondErr(w, http.StatusConflict, "a database server called "+d.Name+" already exists")
		return
	} else if err != nil {
		internalError(w, "UpdateDBServer", err, "")
		return
	}
	s.logIfErr(s.syncDBServers(r.Context()), "syncDBServers")
	s.audit(c.Actor(), "dbserver.update", d.Name, "")
	d, _ = s.Store.GetDBServer(d.ID)
	respond(w, http.StatusOK, s.dbServerView(d))
}

func (s *Server) handleTestDBServer(w http.ResponseWriter, r *http.Request, c *call) {
	d, ok := s.dbServerFromPath(w, r)
	if !ok {
		return
	}
	pw, err := s.Store.DBServerPassword(d.ID)
	if err != nil || pw == "" {
		internalError(w, "DBServerPassword", err, "the server's password could not be read")
		return
	}
	res, err := s.testDBServer(r.Context(), d, pw)
	if err != nil {
		var no *declined
		if errors.As(err, &no) {
			respondErr(w, http.StatusBadGateway, no.msg)
			return
		}
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"ok": true, "version": res.Version})
}

func (s *Server) handleDeleteDBServer(w http.ResponseWriter, r *http.Request, c *call) {
	d, ok := s.dbServerFromPath(w, r)
	if !ok {
		return
	}
	err := s.Store.DeleteDBServer(d.ID)
	if errors.Is(err, state.ErrDBServerInUse) {
		respondErr(w, http.StatusConflict, "drop its databases first")
		return
	}
	if err != nil {
		internalError(w, "DeleteDBServer", err, "")
		return
	}
	s.logIfErr(s.syncDBServers(r.Context()), "syncDBServers")
	s.audit(c.Actor(), "dbserver.delete", d.Name, "")
	respond(w, http.StatusOK, map[string]string{"deleted": d.Name})
}

// syncingAgent repeats a call once after giving the agent the database
// servers, when the agent says it does not know one (it restarted).
type syncingAgent struct {
	AgentCaller
	s *Server
}

// unknownServer reports whether err is the agent not knowing a server.
func unknownServer(err error) bool {
	var ae *agentrpc.Error
	return errors.As(err, &ae) && ae.Code == agentrpc.CodeUnavailable && strings.HasPrefix(ae.Message, "unknown database server")
}

func (a syncingAgent) Call(ctx context.Context, method string, params, out any) error {
	err := a.AgentCaller.Call(ctx, method, params, out)
	if err == nil || !unknownServer(err) || method == agentrpc.SetDBServers.Name() {
		return err
	}
	if serr := a.s.syncDBServers(ctx); serr != nil {
		return err
	}
	return a.AgentCaller.Call(ctx, method, params, out)
}

// rawAgent is the agent without the retry (the sync itself must not retry).
func (s *Server) rawAgent() AgentCaller {
	if a, ok := s.Agent.(syncingAgent); ok {
		return a.AgentCaller
	}
	return s.Agent
}
