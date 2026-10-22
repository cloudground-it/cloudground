package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Ubuntu updates (SPEC.md §7.5): the server's pending package upgrades and
// the upgrade, done by the agent, which takes no package name from here.

// updatesTTL is how long the list is kept: reading it forks apt.
const updatesTTL = 5 * time.Minute

// updatesCache is the last list the agent gave.
type updatesCache struct {
	mu  sync.Mutex
	at  time.Time
	res rpc.SystemUpdatesResult
}

// systemUpdatesView is GET /api/system/updates.
type systemUpdatesView struct {
	rpc.SystemUpdatesResult
	CheckedAt time.Time `json:"checked_at"`
}

func (c *updatesCache) get(now time.Time) (systemUpdatesView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || now.Sub(c.at) > updatesTTL {
		return systemUpdatesView{}, false
	}
	return systemUpdatesView{c.res, c.at}, true
}

func (c *updatesCache) put(res rpc.SystemUpdatesResult, at time.Time) systemUpdatesView {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res, c.at = res, at
	return systemUpdatesView{res, at}
}

// listUpdates is the list, from the cache or the agent.
func (s *Server) listUpdates(ctx context.Context) (systemUpdatesView, error) {
	now := time.Now()
	if v, ok := s.sysUpdates.get(now); ok {
		return v, nil
	}
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.SystemUpdates, rpc.SystemUpdatesParams{})
	if err != nil {
		return systemUpdatesView{}, err
	}
	return s.sysUpdates.put(res, now.UTC()), nil
}

func (s *Server) handleSystemUpdates(w http.ResponseWriter, r *http.Request, c *call) {
	v, err := s.listUpdates(r.Context())
	answer(w, http.StatusOK, v, err)
}

// pendingLine is the progress line after a refresh or an upgrade.
func pendingLine(p rpc.SystemUpdatesResult) string {
	line := fmt.Sprintf("%d updates pending (%d security)", len(p.Packages), p.Security)
	if p.RebootRequired {
		line += "; the server needs a reboot to finish installing them"
	}
	return line
}

func (s *Server) handleRefreshUpdates(w http.ResponseWriter, r *http.Request, c *call) {
	task, err := s.runJob(jobReq{kind: "apt.update", locks: []string{aptLock}, dedupKey: "apt.update"},
		func(ctx context.Context, _ state.Site, step func(int, string)) error {
			step(20, "Updating the package lists")
			res, err := agentrpc.Call(ctx, s.Agent, agentrpc.SystemUpdates, rpc.SystemUpdatesParams{Refresh: true})
			if err != nil {
				return err
			}
			s.sysUpdates.put(res, time.Now().UTC())
			step(100, pendingLine(res))
			return nil
		})
	accepted(w, task, err)
}

// applyUpdatesReq is the body of POST /api/system/updates/apply.
type applyUpdatesReq struct {
	SecurityOnly bool `json:"security_only"`
}

func (s *Server) handleApplyUpdates(w http.ResponseWriter, r *http.Request, c *call) {
	var req applyUpdatesReq
	if !decodeOr400(w, r, &req) {
		return
	}
	what := "all"
	if req.SecurityOnly {
		what = "security"
	}
	task, err := s.runJob(jobReq{kind: "apt.upgrade", locks: []string{aptLock}, dedupKey: "apt.upgrade"},
		func(ctx context.Context, _ state.Site, step func(int, string)) error {
			if err := checkpoint(ctx, step, "the upgrade"); err != nil {
				return err
			}
			if req.SecurityOnly {
				step(10, "Installing the security updates")
			} else {
				step(10, "Upgrading the system packages")
			}
			res, err := agentrpc.Call(context.WithoutCancel(ctx), s.Agent, agentrpc.ApplyUpdates, rpc.ApplyUpdatesParams{SecurityOnly: req.SecurityOnly})
			s.sysUpdates.drop()
			if err != nil {
				return err
			}
			s.sysUpdates.put(res.Pending, time.Now().UTC())
			step(100, fmt.Sprintf("%d packages upgraded; %s", res.Upgraded, pendingLine(res.Pending)))
			return nil
		})
	if err == nil {
		s.audit(c.Actor(), "system.updates", "server", what)
	}
	accepted(w, task, err)
}

// drop forgets the list: the next read asks the agent.
func (c *updatesCache) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = time.Time{}
}
