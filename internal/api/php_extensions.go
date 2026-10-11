package api

import (
	"context"
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A PHP version's extensions (SPEC.md §7.3, issue #29): listed for anyone
// signed in (the site's PHP page shows its version's), installed by an
// administrator only, since the packages are the whole server's and the
// install restarts every site of that version.

// aptLock is the lock of a job that installs packages: apt takes one
// install at a time anyway, and two jobs would restart the same masters.
const aptLock = "apt"

func (s *Server) handlePHPExtensions(w http.ResponseWriter, r *http.Request, c *call) {
	p := rpc.PHPExtensionsParams{Version: r.PathValue("version")}
	if err := p.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.PHPExtensions, p)
	answer(w, http.StatusOK, out, err)
}

// handleInstallPHPExtension starts php.extension: the install, then a
// restart of every site of that version so it loads the extension.
func (s *Server) handleInstallPHPExtension(w http.ResponseWriter, r *http.Request, c *call) {
	var body struct {
		Extension string `json:"extension"`
	}
	if !decodeOr400(w, r, &body) {
		return
	}
	p := rpc.PHPExtensionInstallParams{Version: r.PathValue("version"), Extension: body.Extension}
	if err := p.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	agent := s.Agent
	task, err := s.runJob(jobReq{kind: "php.extension", locks: []string{aptLock},
		payload: p, dedupKey: "php.extension:" + p.Version + ":" + p.Extension},
		func(ctx context.Context, _ state.Site, step func(int, string)) error {
			step(10, "Installing php"+p.Version+"-"+p.Extension)
			return agentrpc.Into(ctx, agent, agentrpc.PHPExtInstall, p, nil)
		})
	if err == nil {
		s.audit(c.Actor(), "php.extension", "PHP "+p.Version, p.Extension)
	}
	accepted(w, task, err)
}
