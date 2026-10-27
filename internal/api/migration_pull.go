package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Pulling a migration from another host (SPEC.md §10.1, #82): the agent
// fetches the old site over SSH/SFTP into the site's tmp/, then the import
// of §10 runs with its safety snapshot. The old host's credentials are
// single-use: they live in this request's job and its one agent call.

// pullOrder is what the two pull routes take.
type pullOrder struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	HostKey    string `json:"host_key"`
	Path       string `json:"path"`
	DB         string `json:"db"`
	SQLPath    string `json:"sql_path"`
	OldDomain  string `json:"old_domain"`
	Confirm    string `json:"confirm"`
	MaxRatio   int64  `json:"max_ratio"`
}

func (o pullOrder) params(site state.Site) rpc.PullParams {
	return rpc.PullParams{
		Site: site, Host: strings.TrimSpace(o.Host), Port: o.Port, User: strings.TrimSpace(o.User),
		Password: o.Password, PrivateKey: o.PrivateKey, HostKey: strings.TrimSpace(o.HostKey),
		Path: strings.TrimSpace(o.Path), DB: o.DB, SQLPath: strings.TrimSpace(o.SQLPath),
	}
}

// where is the old site as the logs name it, with no secret.
func (o pullOrder) where() string {
	return fmt.Sprintf("%s@%s:%s", strings.TrimSpace(o.User), strings.TrimSpace(o.Host), strings.TrimSpace(o.Path))
}

func (s *Server) handlePullEstimate(w http.ResponseWriter, r *http.Request, c *call) {
	var o pullOrder
	if !decodeOr400(w, r, &o) {
		return
	}
	p := o.params(c.Site)
	if err := p.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var res rpc.PullEstimateResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.PullEstimate, p, &res); err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handlePullMigration(w http.ResponseWriter, r *http.Request, c *call) {
	var o pullOrder
	if !decodeOr400(w, r, &o) {
		return
	}
	site := c.Site
	p := o.params(site)
	if err := p.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The import's own rules (§10), with the files to come.
	order := migrationOrder{Archive: "pulled.tar.gz", OldDomain: o.OldDomain, Confirm: o.Confirm, MaxRatio: o.MaxRatio}
	if o.DB != "" {
		order.SQL = "pulled.sql"
	}
	if err := order.check(site, c.User.Role); err != nil {
		fail(w, err)
		return
	}
	repo, password, err := s.backupRepository()
	if err != nil {
		internalError(w, "backupRepository", err, "could not read the backup settings")
		return
	}
	safety := migrationSafety{repo: repo, password: password}
	locks := siteLocks(site.ID)
	if safety.on() {
		locks = append(locks, jobs.RepoLock(repo))
	}
	p.ReleaseID = siteval.NewReleaseID(time.Now())
	release, oldDomain, maxRatio, where := p.ReleaseID, strings.ToLower(strings.TrimSpace(o.OldDomain)), o.MaxRatio, o.where()
	task, err := s.runJob(jobReq{kind: "migration.pull", siteID: site.ID, locks: locks,
		// The record holds where, never how to sign in there.
		payload: map[string]any{"site_id": site.ID, "domain": site.Domain, "from": where, "release_id": release},
	}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		pull := p
		pull.Site = site
		// The credentials are spent by this one call (SPEC.md §10.1).
		p.Password, p.PrivateKey = "", ""
		step(5, "Connecting to "+where)
		var res rpc.PullResult
		err := agentrpc.Into(ctx, s.Agent, agentrpc.PullMigration, pull, &res)
		pull.Password, pull.PrivateKey = "", ""
		defer func() {
			_ = agentrpc.Into(context.WithoutCancel(ctx), s.Agent, agentrpc.PullCleanup, rpc.PullCleanupParams{Site: site, ReleaseID: release}, nil)
		}()
		if err != nil {
			return err
		}
		step(30, fmt.Sprintf("Copied %d files (%d bytes) from %s; the host's key was %s", res.Files, res.Bytes, where, res.HostKey))
		return s.runMigration(ctx, rpc.MigrationParams{
			Site: site, Archive: res.Archive, SQL: res.SQL, OldDomain: oldDomain, ReleaseID: release, MaxRatio: maxRatio,
		}, safety, step)
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(c.Actor(), "migration.pull", site.Domain, where)
	respond(w, http.StatusAccepted, map[string]any{"task": task, "release_id": release})
}
