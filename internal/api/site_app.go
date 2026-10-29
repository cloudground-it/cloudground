package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A managed Node.js or Python site's app (SPEC.md §3.21, issue #28): its
// runtime version, start command, environment (plain and sealed), and the
// unit the agent runs it in.

// appView is what GET and PUT answer.
type appView struct {
	Version    string            `json:"version"`
	Start      string            `json:"start"`
	Env        map[string]string `json:"env"`
	Secrets    []string          `json:"secrets"`
	Port       int               `json:"port"`
	HealthPath string            `json:"health_path"`
	Socket     string            `json:"socket"`
}

func appOf(st state.Site) appView {
	a := st.Config.App
	v := appView{Version: a.Version, Start: a.Start, Env: a.Env, Secrets: a.Secrets, Port: a.Port, HealthPath: a.HealthPath,
		Socket: strings.TrimPrefix(runtimes.Upstream(st), "unix:")}
	if v.Env == nil {
		v.Env = map[string]string{}
	}
	if v.Secrets == nil {
		v.Secrets = []string{}
	}
	return v
}

// appSecret is the secret of one variable.
func appSecret(siteID int64, name string) string {
	return state.SiteSecret(siteID, state.SiteSecretAppEnvPrefix+name)
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request, c *call) {
	if !c.Site.Type.IsApp() {
		respondErr(w, http.StatusBadRequest, "not a Node.js or Python site")
		return
	}
	respond(w, http.StatusOK, appOf(c.Site))
}

// appEnv is the whole environment the agent writes: the plain variables and
// the secrets' values.
func (s *Server) appEnv(st state.Site) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range st.Config.App.Env {
		env[k] = v
	}
	for _, n := range st.Config.App.Secrets {
		v, err := s.Store.Secret(appSecret(st.ID, n))
		if err != nil {
			return nil, err
		}
		env[n] = v
	}
	return env, nil
}

// handleSetApp changes the app's settings (a field left out keeps its value).
func (s *Server) handleSetApp(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Version    *string            `json:"version"`
		Start      *string            `json:"start"`
		Env        *map[string]string `json:"env"`
		Secrets    map[string]string  `json:"secrets"`
		Port       *int               `json:"port"`
		HealthPath *string            `json:"health_path"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if !c.Site.Type.IsApp() {
		respondErr(w, http.StatusBadRequest, "not a Node.js or Python site")
		return
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	a := cur.Config.App
	var changed []string
	if req.Version != nil {
		a.Version, changed = strings.TrimSpace(*req.Version), append(changed, "version")
	}
	if req.Start != nil {
		a.Start, changed = strings.TrimSpace(*req.Start), append(changed, "start")
	}
	if req.Env != nil {
		a.Env, changed = *req.Env, append(changed, "env")
		if len(a.Env) == 0 {
			a.Env = nil
		}
	}
	if req.Port != nil {
		a.Port, changed = *req.Port, append(changed, "port")
	}
	if req.HealthPath != nil {
		a.HealthPath, changed = strings.TrimSpace(*req.HealthPath), append(changed, "health_path")
	}
	// Secrets: a value sets, "" removes, the mask keeps.
	writes := map[string]string{}
	secrets := slices.Clone(a.Secrets)
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
	a.Secrets = secrets
	if len(a.Secrets) == 0 {
		a.Secrets = nil
	}
	if err := runtimes.CheckApp(cur.Type, a); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// F-6-2: a new TCP port must lie outside the range the kernel hands out
	// itself, where any process could be given it while the app restarts.
	// An app keeps the port it has.
	if a.Port != 0 && a.Port != cur.Config.App.Port {
		caps, err := s.capabilities(r.Context())
		if err != nil {
			respondAgentErr(w, 0, "", err)
			return
		}
		if lo, hi := caps.EphemeralRange(); a.Port >= lo && a.Port <= hi {
			respondErr(w, http.StatusBadRequest, fmt.Sprintf("port %d is in the range this server gives out for its own connections (%d–%d), so another program could take it while the app restarts: choose a port below %d or above %d", a.Port, lo, hi, lo, hi))
			return
		}
	}
	if a.Version != cur.Config.App.Version {
		caps, err := s.capabilities(r.Context())
		if err != nil {
			respondAgentErr(w, 0, "", err)
			return
		}
		rt := runtimes.Of(cur.Type)
		if !caps.Runtimes.Has(rt, a.Version) {
			respondErr(w, http.StatusPreconditionFailed, "install "+runtimes.Label(rt)+" "+a.Version+" on the server first (Server → Runtimes)")
			return
		}
	}
	// The secrets first, so the environment the agent gets is the new one;
	// a refusal below puts them back.
	prior := map[string]string{}
	for n, v := range writes {
		old, err := s.Store.Secret(appSecret(cur.ID, n))
		if err != nil {
			internalError(w, "Secret", err, "")
			return
		}
		prior[n] = old
		if err := s.Store.SetSecret(appSecret(cur.ID, n), v); err != nil {
			s.restoreAppSecrets(cur.ID, prior)
			internalError(w, "SetSecret", err, "could not save the secret")
			return
		}
	}
	next := cur
	next.Config.App = a
	env, err := s.appEnv(next)
	if err != nil {
		s.restoreAppSecrets(cur.ID, prior)
		internalError(w, "Secret", err, "")
		return
	}
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SetAppEnv, rpc.AppEnvParams{Site: next, Env: env}, nil); err != nil {
		s.restoreAppSecrets(cur.ID, prior)
		respondAgentErr(w, 0, "", err)
		return
	}
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
		s.restoreAppSecrets(cur.ID, prior)
		// The environment file goes back to what the stored settings make.
		if old, eerr := s.appEnv(cur); eerr == nil {
			s.logIfErr(agentrpc.Into(context.WithoutCancel(r.Context()), s.Agent, agentrpc.SetAppEnv, rpc.AppEnvParams{Site: cur, Env: old}, nil), "SetAppEnv")
		}
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.App = a
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.recordRendered(stored, res.Files)
	s.audit(c.Actor(), "site.app", cur.Domain, strings.Join(changed, ", "))
	respond(w, http.StatusOK, appOf(stored))
}

// restoreAppSecrets puts secrets back after a refused change.
func (s *Server) restoreAppSecrets(siteID int64, prior map[string]string) {
	for n, v := range prior {
		s.logIfErr(s.Store.SetSecret(appSecret(siteID, n), v), "SetSecret")
	}
}

func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request, c *call) {
	if !c.Site.Type.IsApp() {
		respondErr(w, http.StatusBadRequest, "not a Node.js or Python site")
		return
	}
	var res rpc.AppStatusResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.AppStatus, rpc.SiteRef{Site: c.Site}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleAppRestart(w http.ResponseWriter, r *http.Request, c *call) {
	if !c.Site.Type.IsApp() {
		respondErr(w, http.StatusBadRequest, "not a Node.js or Python site")
		return
	}
	var res rpc.AppStatusResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.AppControl, rpc.AppControlParams{Site: c.Site, Action: "restart"}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	s.audit(c.Actor(), "site.app.restart", c.Site.Domain, "")
	respond(w, http.StatusOK, res)
}
