package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Standalone functions (SPEC.md §17, issue #143 v0.9.0): Lambda-style
// Node.js and Python handlers, a server-wide resource with its own user,
// code, addresses and schedule.

// reservedFunctionNames are names a function cannot take: they are routes
// of their own beneath /api/functions/.
var reservedFunctionNames = []string{"domain"}

// functionView is one function as the routes answer it (§17.1).
type functionView struct {
	state.Function
	URLs     []string          `json:"urls"`
	HasToken bool              `json:"has_token"`
	Last     *rpc.Invocation   `json:"last,omitempty"`
	Entry    string            `json:"entry,omitempty"`
	Files    map[string]string `json:"files,omitempty"`
	Releases []string          `json:"releases,omitempty"`
	Token    string            `json:"token,omitempty"`
}

func (s *Server) functionViewOf(f state.Function, domain string) functionView {
	tok, _ := s.Store.Secret(state.FunctionTokenSecret(f.ID))
	return functionView{Function: f, URLs: functions.URLs(f, domain), HasToken: tok != ""}
}

func (s *Server) functionsDomain() string {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return ""
	}
	return ps.FunctionsDomain
}

// getFunction reads a function, answering 404 or 500 itself.
func (s *Server) getFunction(w http.ResponseWriter, name string) (state.Function, bool) {
	f, err := s.Store.GetFunction(name)
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "no such function")
		return f, false
	}
	if err != nil {
		internalError(w, "GetFunction", err, "could not read the function")
		return f, false
	}
	return f, true
}

func (s *Server) handleListFunctions(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.Store.ListFunctions()
	if err != nil {
		internalError(w, "ListFunctions", err, "could not read the functions")
		return
	}
	domain := s.functionsDomain()
	var last rpc.FunctionsSummaryResult
	if len(list) > 0 {
		names := make([]string, 0, len(list))
		for _, f := range list {
			names = append(names, f.Name)
		}
		// The newest invocations are a courtesy: the list answers without.
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionsSummary, rpc.FunctionsSummaryParams{Names: names}, &last); err != nil {
			s.Log.Warn("could not read the functions' invocations", "err", err)
		}
	}
	out := make([]functionView, 0, len(list))
	for _, f := range list {
		v := s.functionViewOf(f, domain)
		if inv, ok := last.Last[f.Name]; ok {
			v.Last = &inv
		}
		out = append(out, v)
	}
	respond(w, http.StatusOK, map[string]any{"functions": out, "domain": domain})
}

func (s *Server) handleGetFunction(w http.ResponseWriter, r *http.Request, c *call) {
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	v := s.functionViewOf(f, s.functionsDomain())
	var code rpc.FunctionCodeResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionCode, rpc.FunctionRef{ID: f.ID, Name: f.Name}, &code); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	v.Entry, v.Files, v.Releases = functions.Entry(f.Runtime), code.Files, code.Releases
	if v.Files == nil {
		v.Files = map[string]string{}
	}
	if code.Release != "" {
		v.Release = code.Release
	}
	respond(w, http.StatusOK, v)
}

// functionReq is the body of a create or a change (§17.2).
type functionReq struct {
	Name        string             `json:"name"`
	Runtime     string             `json:"runtime"`
	Version     *string            `json:"version"`
	MemoryMB    *int               `json:"memory_mb"`
	TimeoutSec  *int               `json:"timeout_sec"`
	Schedule    *string            `json:"schedule"`
	Auth        *string            `json:"auth"`
	CORSOrigins *[]string          `json:"cors_origins"`
	Env         *map[string]string `json:"env"`
	Secrets     map[string]string  `json:"secrets"`
	Files       map[string]string  `json:"files"`
	Code        *string            `json:"code"`
	Template    string             `json:"template"`
}

func (s *Server) handleCreateFunction(w http.ResponseWriter, r *http.Request, c *call) {
	var req functionReq
	if !decodeOr400(w, r, &req) {
		return
	}
	s.saveFunction(w, r, c, req, true)
}

func (s *Server) handleUpdateFunction(w http.ResponseWriter, r *http.Request, c *call) {
	var req functionReq
	if !decodeOr400(w, r, &req) {
		return
	}
	req.Name = r.PathValue("name")
	s.saveFunction(w, r, c, req, false)
}

// fnSecretWrite is one secret a change writes ("" deletes it), and what it
// was before, to be put back on a refusal.
type fnSecretWrite struct{ name, value, prior string }

// writeFnSecrets writes each secret, keeping what was there; on a failure
// it puts back what it had written.
func (s *Server) writeFnSecrets(writes []fnSecretWrite) ([]fnSecretWrite, error) {
	done := make([]fnSecretWrite, 0, len(writes))
	for _, wr := range writes {
		old, err := s.Store.Secret(wr.name)
		if err != nil {
			s.restoreFnSecrets(done)
			return nil, err
		}
		wr.prior = old
		if err := s.Store.SetSecret(wr.name, wr.value); err != nil {
			s.restoreFnSecrets(done)
			return nil, err
		}
		done = append(done, wr)
	}
	return done, nil
}

func (s *Server) restoreFnSecrets(done []fnSecretWrite) {
	for _, wr := range done {
		s.logIfErr(s.Store.SetSecret(wr.name, wr.prior), "SetSecret")
	}
}

// saveFunction creates or changes a function (§17.3).
func (s *Server) saveFunction(w http.ResponseWriter, r *http.Request, c *call, req functionReq, create bool) {
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	var f state.Function
	if create {
		if slices.Contains(reservedFunctionNames, req.Name) {
			respondErr(w, http.StatusBadRequest, req.Name+" is a reserved name")
			return
		}
		if _, err := s.Store.GetFunction(req.Name); err == nil {
			respondErr(w, http.StatusConflict, "a function named "+req.Name+" exists")
			return
		}
		all, err := s.Store.ListFunctions()
		if err != nil {
			internalError(w, "ListFunctions", err, "")
			return
		}
		if len(all) >= functions.MaxFunctions {
			respondErr(w, http.StatusBadRequest, fmt.Sprintf("a server has at most %d functions", functions.MaxFunctions))
			return
		}
		f = state.Function{Name: req.Name, Runtime: req.Runtime}
	} else {
		var ok bool
		if f, ok = s.getFunction(w, req.Name); !ok {
			return
		}
	}
	prevVersion := f.Version
	var changed []string
	if req.Version != nil {
		f.Version, changed = strings.TrimSpace(*req.Version), append(changed, "version")
	}
	if req.MemoryMB != nil {
		f.MemoryMB, changed = *req.MemoryMB, append(changed, "memory")
	}
	if req.TimeoutSec != nil {
		f.TimeoutSec, changed = *req.TimeoutSec, append(changed, "timeout")
	}
	if req.Schedule != nil {
		f.Schedule, changed = strings.Join(strings.Fields(*req.Schedule), " "), append(changed, "schedule")
	}
	if req.Auth != nil {
		f.Auth, changed = *req.Auth, append(changed, "auth")
	}
	if req.CORSOrigins != nil {
		f.CORSOrigins, changed = slices.Clone(*req.CORSOrigins), append(changed, "cors")
	}
	if req.Env != nil {
		f.Env, changed = *req.Env, append(changed, "env")
	}
	if create {
		f = functions.Defaults(f)
	}
	secrets := slices.Clone(f.Secrets)
	var writes []fnSecretWrite
	names := make([]string, 0, len(req.Secrets))
	for n := range req.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	pending := map[string]string{}
	for _, n := range names {
		v := req.Secrets[n]
		switch {
		case v == secretMask:
			if !slices.Contains(secrets, n) {
				respondErr(w, http.StatusBadRequest, n+" has no value to keep")
				return
			}
		case v == "":
			secrets = slices.DeleteFunc(secrets, func(x string) bool { return x == n })
			pending[n] = ""
		default:
			if err := runtimes.CheckValue(n, v); err != nil {
				respondErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if !slices.Contains(secrets, n) {
				secrets = append(secrets, n)
			}
			pending[n] = v
		}
		changed = append(changed, "secret "+n)
	}
	sort.Strings(secrets)
	f.Secrets = secrets
	if err := functions.Check(f); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var files map[string]string
	switch {
	case req.Files != nil:
		files, changed = req.Files, append(changed, "code")
	case req.Code != nil:
		files, changed = map[string]string{}, append(changed, "code")
		if !create {
			var live rpc.FunctionCodeResult
			if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionCode, rpc.FunctionRef{ID: f.ID, Name: f.Name}, &live); err == nil {
				for p, v := range live.Files {
					files[p] = v
				}
			}
		}
		files[functions.Entry(f.Runtime)] = *req.Code
	case create:
		tmpl := req.Template
		if tmpl == "" {
			tmpl = functions.TemplateHTTP
		}
		if tmpl != functions.TemplateHTTP && tmpl != functions.TemplateScheduled {
			respondErr(w, http.StatusBadRequest, "the template is http or scheduled")
			return
		}
		files = functions.Starter(f.Runtime, tmpl)
	}
	if files != nil {
		if err := functions.CheckFiles(f.Runtime, files); err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if create || (req.Version != nil && f.Version != prevVersion) {
		caps, err := s.capabilities(r.Context())
		if err != nil {
			respondAgentErr(w, 0, "", err)
			return
		}
		if !caps.Runtimes.Has(f.Runtime, f.Version) {
			respondErr(w, http.StatusPreconditionFailed, "install "+runtimes.Label(f.Runtime)+" "+f.Version+" on the server first (Services → Runtimes)")
			return
		}
	}
	if create {
		created, err := s.Store.CreateFunction(f)
		if errors.Is(err, state.ErrFunctionNameTaken) {
			respondErr(w, http.StatusConflict, "a function named "+f.Name+" exists")
			return
		}
		if err != nil {
			internalError(w, "CreateFunction", err, "could not save the function")
			return
		}
		f.ID, f.CreatedAt, f.UpdatedAt, f.Domains = created.ID, created.CreatedAt, created.UpdatedAt, created.Domains
	}
	// A create that fails from here takes its row, its secrets and what
	// the agent made with it.
	agentMade := false
	dropCreated := func() {
		if !create {
			return
		}
		if agentMade {
			s.logIfErr(agentrpc.Into(context.WithoutCancel(r.Context()), s.Agent, agentrpc.DeleteFunction, rpc.FunctionRef{ID: f.ID, Name: f.Name}, nil), "DeleteFunction")
		}
		s.logIfErr(s.Store.DeleteFunction(f.ID), "DeleteFunction")
	}
	for _, n := range names {
		if v, ok := pending[n]; ok {
			writes = append(writes, fnSecretWrite{name: state.FunctionSecret(f.ID, n), value: v})
		}
	}
	token := ""
	if f.Auth == state.FunctionToken {
		if cur, _ := s.Store.Secret(state.FunctionTokenSecret(f.ID)); cur == "" {
			t, err := functions.NewToken()
			if err != nil {
				dropCreated()
				internalError(w, "NewToken", err, "")
				return
			}
			token = t
			writes = append(writes, fnSecretWrite{name: state.FunctionTokenSecret(f.ID), value: t})
		}
	}
	done, err := s.writeFnSecrets(writes)
	if err != nil {
		dropCreated()
		internalError(w, "SetSecret", err, "could not save the secrets")
		return
	}
	env, err := s.functionEnv(f)
	if err != nil {
		s.restoreFnSecrets(done)
		dropCreated()
		internalError(w, "Secret", err, "")
		return
	}
	var res rpc.FunctionResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SetFunction, rpc.SetFunctionParams{Function: f, Files: files, Env: env}, &res); err != nil {
		s.restoreFnSecrets(done)
		// SetFunction makes the user and the units before the code: a
		// refused create leaves them, and they go with the row.
		agentMade = true
		dropCreated()
		respondAgentErr(w, 0, "", err)
		return
	}
	agentMade = true
	f.Release = res.Release
	if err := s.renderFunctions(r.Context(), renderWith{fn: &f}); err != nil {
		s.restoreFnSecrets(done)
		dropCreated()
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.UpdateFunction(f.Name, func(st *state.Function) error {
		st.Runtime, st.Version, st.MemoryMB, st.TimeoutSec, st.Schedule = f.Runtime, f.Version, f.MemoryMB, f.TimeoutSec, f.Schedule
		st.Env, st.Secrets, st.Auth, st.CORSOrigins, st.Release = f.Env, f.Secrets, f.Auth, f.CORSOrigins, f.Release
		return nil
	})
	if err != nil {
		internalError(w, "UpdateFunction", err, "could not save the function")
		return
	}
	action, status := "function.update", http.StatusOK
	if create {
		action, status = "function.create", http.StatusCreated
	}
	s.audit(c.Actor(), action, f.Name, strings.Join(changed, ", "))
	v := s.functionViewOf(stored, s.functionsDomain())
	v.Token = token
	if res.Releases != nil {
		v.Releases = res.Releases
	}
	respond(w, status, v)
}

// functionEnv is a function's whole environment: its variables and its
// secrets' values.
func (s *Server) functionEnv(f state.Function) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range f.Env {
		env[k] = v
	}
	for _, n := range f.Secrets {
		v, err := s.Store.Secret(state.FunctionSecret(f.ID, n))
		if err != nil {
			return nil, err
		}
		env[n] = v
	}
	return env, nil
}

// renderWith is what a render differs in from what is stored: a function
// as it will be (fn), one left out (without), the functions domain.
type renderWith struct {
	fn      *state.Function
	without string
	domain  *string
}

// renderFunctions renders every function's nginx files (§17.3 step 6) and
// records them for drift. The caller holds fnMu.
func (s *Server) renderFunctions(ctx context.Context, with renderWith) error {
	list, err := s.Store.ListFunctions()
	if err != nil {
		return err
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return err
	}
	p := rpc.RenderFunctionsParams{Domain: ps.FunctionsDomain, Functions: []rpc.FunctionRoute{}}
	if with.domain != nil {
		p.Domain = *with.domain
	}
	seen := false
	for _, f := range list {
		if f.Name == with.without {
			continue
		}
		if with.fn != nil && f.Name == with.fn.Name {
			f, seen = *with.fn, true
		}
		if f.Release == "" {
			continue // a create still being set up has no code to route to
		}
		route, err := s.functionRoute(f)
		if err != nil {
			return err
		}
		p.Functions = append(p.Functions, route)
	}
	if with.fn != nil && !seen && with.fn.Release != "" {
		route, err := s.functionRoute(*with.fn)
		if err != nil {
			return err
		}
		p.Functions = append(p.Functions, route)
	}
	var res rpc.RenderFunctionsResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RenderFunctions, p, &res); err != nil {
		return err
	}
	s.recordFunctionFiles(res.Files)
	return nil
}

func (s *Server) functionRoute(f state.Function) (rpc.FunctionRoute, error) {
	route := rpc.FunctionRoute{ID: f.ID, Name: f.Name, TimeoutSec: f.TimeoutSec, CORSOrigins: f.CORSOrigins, Domains: f.Domains}
	if f.Auth == state.FunctionToken {
		tok, err := s.Store.Secret(state.FunctionTokenSecret(f.ID))
		if err != nil {
			return route, err
		}
		route.Token = tok
	}
	return route, nil
}

// recordFunctionFiles records the files a render wrote and forgets the
// function files it no longer writes.
func (s *Server) recordFunctionFiles(files []rpc.ManagedFile) {
	wrote := map[string]bool{}
	for _, f := range files {
		wrote[f.Path] = true
		s.logIfErr(s.Store.RecordManagedFile(f.Path, f.SHA256), "RecordManagedFile")
	}
	all, err := s.Store.ManagedFiles()
	if err != nil {
		return
	}
	for p := range all {
		if !wrote[p] && nginx.IsFunctionsFile(baseName(p)) {
			s.logIfErr(s.Store.RemoveManagedFile(p), "RemoveManagedFile")
		}
	}
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *Server) handleDeleteFunction(w http.ResponseWriter, r *http.Request, c *call) {
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	// The routes first, so nginx never proxies to a socket that is gone.
	if err := s.renderFunctions(r.Context(), renderWith{without: f.Name}); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DeleteFunction, rpc.FunctionRef{ID: f.ID, Name: f.Name}, nil); err != nil {
		s.Log.Warn("could not remove everything of a deleted function", "function", f.Name, "err", err)
	}
	if err := s.Store.DeleteFunction(f.ID); err != nil && !errors.Is(err, state.ErrNotFound) {
		internalError(w, "DeleteFunction", err, "could not delete the function")
		return
	}
	s.audit(c.Actor(), "function.delete", f.Name, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRollbackFunction(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Release string `json:"release"`
	}
	if err := decode(r, &req); err != nil && r.ContentLength > 0 {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	var res rpc.FunctionResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.RollbackFunction, rpc.FunctionRollbackParams{ID: f.ID, Name: f.Name, Release: req.Release}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.UpdateFunction(f.Name, func(st *state.Function) error {
		st.Release = res.Release
		return nil
	})
	if err != nil {
		internalError(w, "UpdateFunction", err, "")
		return
	}
	s.audit(c.Actor(), "function.rollback", f.Name, res.Release)
	v := s.functionViewOf(stored, s.functionsDomain())
	v.Releases = res.Releases
	respond(w, http.StatusOK, v)
}

func (s *Server) handleInvokeFunction(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}
	if err := decode(r, &req); err != nil && r.ContentLength > 0 {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	p := rpc.InvokeFunctionParams{Name: f.Name, Source: "test", Method: strings.ToUpper(req.Method), Path: req.Path, Body: req.Body, TimeoutSec: f.TimeoutSec}
	if err := p.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var res rpc.InvokeResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.InvokeFunction, p, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	s.audit(c.Actor(), "function.invoke", f.Name, "")
	respond(w, http.StatusOK, res)
}

func (s *Server) handleFunctionLogs(w http.ResponseWriter, r *http.Request, c *call) {
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	var res rpc.FunctionLogsResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionLogs, rpc.FunctionRef{ID: f.ID, Name: f.Name}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}

// handleFunctionToken makes a new token (§17.2): the old one is refused
// from the render on.
func (s *Server) handleFunctionToken(w http.ResponseWriter, r *http.Request, c *call) {
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	tok, err := functions.NewToken()
	if err != nil {
		internalError(w, "NewToken", err, "")
		return
	}
	done, err := s.writeFnSecrets([]fnSecretWrite{{name: state.FunctionTokenSecret(f.ID), value: tok}})
	if err != nil {
		internalError(w, "SetSecret", err, "could not save the token")
		return
	}
	f.Auth = state.FunctionToken
	if err := s.renderFunctions(r.Context(), renderWith{fn: &f}); err != nil {
		s.restoreFnSecrets(done)
		respondAgentErr(w, 0, "", err)
		return
	}
	if _, err := s.Store.UpdateFunction(f.Name, func(st *state.Function) error {
		st.Auth = state.FunctionToken
		return nil
	}); err != nil {
		internalError(w, "UpdateFunction", err, "")
		return
	}
	s.audit(c.Actor(), "function.token", f.Name, "")
	respond(w, http.StatusOK, map[string]string{"token": tok})
}

// fnRuns remembers which function ran in which minute (the sweep's key).
type fnRuns struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// first reports whether key is new, and forgets keys older than an hour.
func (f *fnRuns) first(key string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]time.Time{}
	}
	if _, ok := f.seen[key]; ok {
		return false
	}
	for k, t := range f.seen {
		if now.Sub(t) > time.Hour {
			delete(f.seen, k)
		}
	}
	f.seen[key] = now
	return true
}

// runFunctionsSweep is functions.sweep (§17.6): each due function, once per
// minute, in the background.
func (s *Server) runFunctionsSweep(ctx context.Context, r *jobs.Run) error {
	list, err := s.Store.ListFunctions()
	if err != nil {
		return err
	}
	now := time.Now()
	minute := now.Truncate(time.Minute).Format(time.RFC3339)
	n := 0
	for _, f := range list {
		if f.Schedule == "" || f.Release == "" || !functions.Due(f.Schedule, now) {
			continue
		}
		if !s.fnRuns.first(fmt.Sprintf("%d/%s", f.ID, minute), now) {
			continue
		}
		n++
		go func(f state.Function) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(f.TimeoutSec+30)*time.Second)
			defer cancel()
			var res rpc.InvokeResult
			err := agentrpc.Into(ctx, s.Agent, agentrpc.InvokeFunction, rpc.InvokeFunctionParams{Name: f.Name, Source: "schedule", TimeoutSec: f.TimeoutSec}, &res)
			switch {
			case err != nil:
				s.Log.Warn("a scheduled function could not be invoked", "function", f.Name, "err", err)
			case res.Status == 0 || res.Status >= 500:
				s.Log.Warn("a scheduled function failed", "function", f.Name, "status", res.Status, "error", res.Error)
			}
		}(f)
	}
	r.Progress(100, fmt.Sprintf("%d scheduled functions invoked", n))
	return nil
}
