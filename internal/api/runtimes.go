package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Node.js and Python on the server (SPEC.md §3.21, #28): what is installed,
// and installing a version, for the managed app sites.

// runtimesView is GET /api/runtimes.
type runtimesView struct {
	Installed rpc.RuntimesResult `json:"installed"`
	// Available are the versions that can be installed.
	Available struct {
		Node   []string `json:"node"`
		Python []string `json:"python"`
	} `json:"available"`
}

func (s *Server) handleGetRuntimes(w http.ResponseWriter, r *http.Request) {
	var have rpc.RuntimesResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.Runtimes, agentrpc.None{}, &have); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	var v runtimesView
	v.Installed = have
	v.Available.Node = runtimes.NodeMajors
	// The distro's own Python: the one the server runs, installed or not.
	v.Available.Python = []string{}
	for _, p := range have.Python {
		v.Available.Python = append(v.Available.Python, p.Version)
	}
	if len(v.Available.Python) == 0 {
		v.Available.Python = []string{"3.12"}
	}
	respond(w, http.StatusOK, v)
}

// handleInstallRuntime starts runtime.install.
func (s *Server) handleInstallRuntime(w http.ResponseWriter, r *http.Request, c *call) {
	var p rpc.RuntimeInstallParams
	if !decodeOr400(w, r, &p) {
		return
	}
	if err := p.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	agent := s.Agent
	task, err := s.runJob(jobReq{kind: "runtime.install", locks: []string{aptLock},
		payload: p, dedupKey: "runtime.install:" + p.Runtime + ":" + p.Version},
		func(ctx context.Context, _ state.Site, step func(int, string)) error {
			step(10, "Installing "+runtimes.Label(p.Runtime)+" "+p.Version)
			err := agentrpc.Into(ctx, agent, agentrpc.RuntimeInstall, p, nil)
			s.staleCapabilities()
			return err
		})
	if err == nil {
		s.audit(c.Actor(), "runtime.install", runtimes.Label(p.Runtime), p.Version)
	}
	accepted(w, task, err)
}

// ensureRuntime installs a new managed site's runtime when the server
// lacks it (§3.2, the job's first step).
func (s *Server) ensureRuntime(ctx context.Context, rec state.Site, progress func(int, string)) error {
	rt, v := runtimes.Of(rec.Type), rec.Config.App.Version
	var have rpc.RuntimesResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.Runtimes, agentrpc.None{}, &have); err != nil {
		return err
	}
	if have.Has(rt, v) {
		return nil
	}
	progress(5, fmt.Sprintf("Installing %s %s", runtimes.Label(rt), v))
	err := agentrpc.Into(ctx, s.Agent, agentrpc.RuntimeInstall, rpc.RuntimeInstallParams{Runtime: rt, Version: v}, nil)
	s.staleCapabilities()
	return err
}
