package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's functions (SPEC.md §3.20, issue #143): Lambda-style Node.js and
// Python handlers at https://<domain>/.functions/<name>, one process per
// invocation, scheduled by a minute sweep.

// functionView is one function as the routes answer it.
type functionView struct {
	state.SiteFunction
	URL      string   `json:"url"`
	Code     *string  `json:"code,omitempty"`
	Releases []string `json:"releases,omitempty"`
}

func functionViewOf(st state.Site, f state.SiteFunction) functionView {
	if f.Env == nil {
		f.Env = map[string]string{}
	}
	if f.Secrets == nil {
		f.Secrets = []string{}
	}
	return functionView{SiteFunction: f, URL: "https://" + st.Domain + "/.functions/" + f.Name}
}

func fnSecret(siteID int64, name, v string) string {
	return state.SiteSecret(siteID, "fn."+name+"."+v)
}

func findFunction(st state.Site, name string) (int, bool) {
	i := slices.IndexFunc(st.Config.Functions, func(f state.SiteFunction) bool { return f.Name == name })
	return i, i >= 0
}

func (s *Server) handleListFunctions(w http.ResponseWriter, r *http.Request, c *call) {
	out := []functionView{}
	for _, f := range c.Site.Config.Functions {
		out = append(out, functionViewOf(c.Site, f))
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleGetFunction(w http.ResponseWriter, r *http.Request, c *call) {
	i, ok := findFunction(c.Site, r.PathValue("name"))
	if !ok {
		respondErr(w, http.StatusNotFound, "no such function")
		return
	}
	f := c.Site.Config.Functions[i]
	v := functionViewOf(c.Site, f)
	var code rpc.FunctionCodeResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionCode, rpc.FunctionRef{Site: c.Site, Name: f.Name}, &code); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	v.Code, v.Releases, v.Release = &code.Code, code.Releases, code.Release
	respond(w, http.StatusOK, v)
}

// functionReq is the body of a create or a change.
type functionReq struct {
	Name       string             `json:"name"`
	Runtime    string             `json:"runtime"`
	Version    *string            `json:"version"`
	MemoryMB   *int               `json:"memory_mb"`
	TimeoutSec *int               `json:"timeout_sec"`
	Schedule   *string            `json:"schedule"`
	Code       *string            `json:"code"`
	Env        *map[string]string `json:"env"`
	Secrets    map[string]string  `json:"secrets"`
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

// saveFunction creates or changes a function (§3.20): secrets, the agent's
// code, environment and units, the vhost, then the record.
func (s *Server) saveFunction(w http.ResponseWriter, r *http.Request, c *call, req functionReq, create bool) {
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	i, exists := findFunction(cur, req.Name)
	var f state.SiteFunction
	switch {
	case create && exists:
		respondErr(w, http.StatusConflict, "the site already has a function "+req.Name)
		return
	case create && len(cur.Config.Functions) >= functions.MaxPerSite:
		respondErr(w, http.StatusBadRequest, fmt.Sprintf("a site has at most %d functions", functions.MaxPerSite))
		return
	case create:
		f = state.SiteFunction{Name: req.Name, Runtime: req.Runtime}
	case !exists:
		respondErr(w, http.StatusNotFound, "no such function")
		return
	default:
		f = cur.Config.Functions[i]
	}
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
	if req.Env != nil {
		f.Env, changed = *req.Env, append(changed, "env")
		if len(f.Env) == 0 {
			f.Env = nil
		}
	}
	if create {
		f = functions.Defaults(f)
	}
	writes := map[string]string{}
	secrets := slices.Clone(f.Secrets)
	names := make([]string, 0, len(req.Secrets))
	for n := range req.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
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
			writes[n] = ""
		default:
			if err := runtimes.CheckValue(n, v); err != nil {
				respondErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if !slices.Contains(secrets, n) {
				secrets = append(secrets, n)
			}
			writes[n] = v
		}
		changed = append(changed, "secret "+n)
	}
	sort.Strings(secrets)
	f.Secrets = secrets
	if len(f.Secrets) == 0 {
		f.Secrets = nil
	}
	if err := functions.Check(f); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	code := ""
	if req.Code != nil {
		code, changed = *req.Code, append(changed, "code")
		if strings.TrimSpace(code) == "" {
			respondErr(w, http.StatusBadRequest, "the code is empty")
			return
		}
		if err := functions.CheckCode(code); err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if create || req.Version != nil {
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
	prior := map[string]string{}
	for n, v := range writes {
		old, err := s.Store.Secret(fnSecret(cur.ID, f.Name, n))
		if err != nil {
			internalError(w, "Secret", err, "")
			return
		}
		prior[n] = old
		if err := s.Store.SetSecret(fnSecret(cur.ID, f.Name, n), v); err != nil {
			s.restoreFnSecrets(cur.ID, f.Name, prior)
			internalError(w, "SetSecret", err, "could not save the secret")
			return
		}
	}
	env := map[string]string{}
	for k, v := range f.Env {
		env[k] = v
	}
	for _, n := range f.Secrets {
		v, err := s.Store.Secret(fnSecret(cur.ID, f.Name, n))
		if err != nil {
			s.restoreFnSecrets(cur.ID, f.Name, prior)
			internalError(w, "Secret", err, "")
			return
		}
		env[n] = v
	}
	var res rpc.FunctionResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SetFunction, rpc.SetFunctionParams{Site: cur, Function: f, Code: code, Env: env}, &res); err != nil {
		s.restoreFnSecrets(cur.ID, f.Name, prior)
		respondAgentErr(w, 0, "", err)
		return
	}
	f.Release = res.Release
	next := cur
	next.Config.Functions = slices.Clone(cur.Config.Functions)
	if exists {
		next.Config.Functions[i] = f
	} else {
		next.Config.Functions = append(next.Config.Functions, f)
	}
	stored, ok := s.applyFunctions(w, r, cur, next.Config.Functions)
	if !ok {
		s.restoreFnSecrets(cur.ID, f.Name, prior)
		if create {
			s.logIfErr(agentrpc.Into(context.WithoutCancel(r.Context()), s.Agent, agentrpc.DeleteFunction, rpc.FunctionRef{Site: cur, Name: f.Name}, nil), "DeleteFunction")
		}
		return
	}
	action, status := "site.function.update", http.StatusOK
	if create {
		action, status = "site.function.create", http.StatusCreated
	}
	s.audit(c.Actor(), action, cur.Domain, f.Name+": "+strings.Join(changed, ", "))
	j, _ := findFunction(stored, f.Name)
	respond(w, status, functionViewOf(stored, stored.Config.Functions[j]))
}

// applyFunctions renders the site with list as its functions and stores it
// once nginx has it; false when it answered already.
func (s *Server) applyFunctions(w http.ResponseWriter, r *http.Request, cur state.Site, list []state.SiteFunction) (state.Site, bool) {
	next := cur
	next.Config.Functions = list
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next, ReloadOnlyIfChanged: true}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return state.Site{}, false
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.Functions = list
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return state.Site{}, false
	}
	s.recordRendered(stored, res.Files)
	return stored, true
}

func (s *Server) restoreFnSecrets(siteID int64, name string, prior map[string]string) {
	for n, v := range prior {
		s.logIfErr(s.Store.SetSecret(fnSecret(siteID, name, n), v), "SetSecret")
	}
}

func (s *Server) handleDeleteFunction(w http.ResponseWriter, r *http.Request, c *call) {
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	name := r.PathValue("name")
	i, ok := findFunction(cur, name)
	if !ok {
		respondErr(w, http.StatusNotFound, "no such function")
		return
	}
	list := slices.Delete(slices.Clone(cur.Config.Functions), i, i+1)
	// The route first, so nginx never proxies to a socket that is gone.
	if _, ok := s.applyFunctions(w, r, cur, list); !ok {
		return
	}
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DeleteFunction, rpc.FunctionRef{Site: cur, Name: name}, nil); err != nil {
		s.Log.Warn("could not remove a deleted function's units", "site", cur.Domain, "function", name, "err", err)
	}
	for _, n := range cur.Config.Functions[i].Secrets {
		s.logIfErr(s.Store.SetSecret(fnSecret(cur.ID, name, n), ""), "SetSecret")
	}
	s.audit(c.Actor(), "site.function.delete", cur.Domain, name)
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
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	name := r.PathValue("name")
	i, ok := findFunction(cur, name)
	if !ok {
		respondErr(w, http.StatusNotFound, "no such function")
		return
	}
	var res rpc.FunctionResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionRollback, rpc.FunctionRollbackParams{Site: cur, Name: name, Release: req.Release}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		if j, ok := findFunction(*st, name); ok {
			st.Config.Functions[j].Release = res.Release
		}
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.audit(c.Actor(), "site.function.rollback", cur.Domain, name+": "+res.Release)
	v := functionViewOf(stored, stored.Config.Functions[i])
	v.Releases = res.Releases
	respond(w, http.StatusOK, v)
}

func (s *Server) handleInvokeFunction(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Body string `json:"body"`
	}
	if err := decode(r, &req); err != nil && r.ContentLength > 0 {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	i, ok := findFunction(c.Site, r.PathValue("name"))
	if !ok {
		respondErr(w, http.StatusNotFound, "no such function")
		return
	}
	f := c.Site.Config.Functions[i]
	var res rpc.InvokeResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.InvokeFunction, rpc.InvokeFunctionParams{Site: c.Site, Name: f.Name, Source: "test", Body: req.Body, TimeoutSec: f.TimeoutSec}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	s.audit(c.Actor(), "site.function.invoke", c.Site.Domain, f.Name)
	respond(w, http.StatusOK, res)
}

func (s *Server) handleFunctionLogs(w http.ResponseWriter, r *http.Request, c *call) {
	if _, ok := findFunction(c.Site, r.PathValue("name")); !ok {
		respondErr(w, http.StatusNotFound, "no such function")
		return
	}
	var res rpc.FunctionLogsResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.FunctionLogs, rpc.FunctionRef{Site: c.Site, Name: r.PathValue("name")}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
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

// runFunctionsSweep is functions.sweep (§3.20): each due function of each
// active site, once per minute, in the background.
func (s *Server) runFunctionsSweep(ctx context.Context, r *jobs.Run) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	now := time.Now()
	minute := now.Truncate(time.Minute).Format(time.RFC3339)
	n := 0
	for _, st := range sites {
		if st.Status != state.SiteActive {
			continue
		}
		for _, f := range st.Config.Functions {
			if f.Schedule == "" || !functions.Due(f.Schedule, now) {
				continue
			}
			if !s.fnRuns.first(fmt.Sprintf("%d/%s/%s", st.ID, f.Name, minute), now) {
				continue
			}
			n++
			go func(st state.Site, f state.SiteFunction) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(f.TimeoutSec+30)*time.Second)
				defer cancel()
				var res rpc.InvokeResult
				err := agentrpc.Into(ctx, s.Agent, agentrpc.InvokeFunction, rpc.InvokeFunctionParams{Site: st, Name: f.Name, Source: "schedule", TimeoutSec: f.TimeoutSec}, &res)
				switch {
				case err != nil:
					s.Log.Warn("a scheduled function could not be invoked", "site", st.Domain, "function", f.Name, "err", err)
				case res.Status == 0 || res.Status >= 500:
					s.Log.Warn("a scheduled function failed", "site", st.Domain, "function", f.Name, "status", res.Status, "error", res.Error)
				}
			}(st, f)
		}
	}
	r.Progress(100, fmt.Sprintf("%d scheduled functions invoked", n))
	return nil
}
