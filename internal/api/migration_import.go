package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Importing a site from another host (SPEC.md §10): an archive of its tree
// and, optionally, a dump of its database, both already in the site's own
// directory, become a new promoted release.

// migrationOrder is POST /api/sites/{id}/migration.
type migrationOrder struct {
	Archive   string `json:"archive"`    // a .tar.gz in the site's tree
	SQL       string `json:"sql"`        // a .sql in the site's tree, optional
	Database  string `json:"database"`   // the site's database SQL goes into; "" is the primary
	OldDomain string `json:"old_domain"` // the domain URLs are rewritten from
	Confirm   string `json:"confirm"`    // the site's domain, typed
	// MaxRatio raises the archive's expansion limit (administrators only);
	// 0 keeps the agent's.
	MaxRatio int64 `json:"max_ratio"`
}

// check is why o cannot start on site for an account of role, or nil.
func (o migrationOrder) check(site state.Site, role string) error {
	bad := func(format string, args ...any) error { return decline(http.StatusBadRequest, format, args...) }
	switch {
	case o.Confirm != site.Domain:
		return bad("type the destination domain to confirm migration")
	case strings.TrimSpace(o.Archive) == "":
		return bad("site archive path is required")
	case o.SQL != "" && o.Database == "" && !localDB(site):
		return decline(http.StatusPreconditionFailed, "SQL import requires a local managed database")
	case o.SQL != "" && isWordPress(site.Type) && o.Database != "" && o.Database != site.Config.Database.Name:
		return bad("a WordPress site's dump goes into its primary database")
	case o.MaxRatio != 0 && role != roleAdmin:
		return decline(http.StatusForbidden, "only an administrator can change the expansion limit")
	case o.MaxRatio < 0 || o.MaxRatio > rpc.MaxMigrationRatio:
		return bad("the expansion limit must be between 1 and %d", rpc.MaxMigrationRatio)
	}
	return nil
}

// startMigration queues the import with a release id minted now, and
// returns the task and that id.
func (s *Server) startMigration(site state.Site, role string, o migrationOrder) (jobs.Task, string, error) {
	if err := o.check(site, role); err != nil {
		return jobs.Task{}, "", err
	}
	if o.SQL != "" && o.Database != "" {
		if _, err := s.resolveDB(site, o.Database); err != nil {
			return jobs.Task{}, "", err
		}
	}
	repo, password, err := s.backupRepository()
	if err != nil {
		return jobs.Task{}, "", err
	}
	safety := migrationSafety{repo: repo, password: password}
	locks := siteLocks(site.ID)
	if safety.on() {
		// One restic process on the repository, as for a restore.
		locks = append(locks, jobs.RepoLock(repo))
	}
	p := rpc.MigrationParams{
		Archive: o.Archive, SQL: o.SQL, Database: o.Database, MaxRatio: o.MaxRatio,
		OldDomain: strings.ToLower(strings.TrimSpace(o.OldDomain)),
		ReleaseID: siteval.NewReleaseID(time.Now()),
	}
	task, err := s.runJob(jobReq{kind: "migration.import", siteID: site.ID, locks: locks}, func(ctx context.Context, site state.Site, step func(int, string)) error {
		run := p
		run.Site = site
		return s.runMigration(ctx, run, safety, step)
	})
	return task, p.ReleaseID, err
}

// migrationSafety is the backup repository a migration's safety snapshot
// goes to, read when the import is queued; both empty when backups are
// not configured.
type migrationSafety struct{ repo, password string }

func (m migrationSafety) on() bool { return m.repo != "" && m.password != "" }

// runMigration is the migration job: a restore-tested safety snapshot of
// the site as it is (an import replaces its uploads and tables, and a
// stale or wrong archive still succeeds), the agent's import, the release
// it made recorded as promoted, the page cache emptied and warmed again.
// Without a backup repository the task says, before anything moves, that
// the import cannot be undone. The agent's refusal is returned as it
// came, so the task keeps its translation key and each reader sees it in
// their language.
func (s *Server) runMigration(ctx context.Context, p rpc.MigrationParams, safety migrationSafety, step func(int, string)) error {
	kept := ""
	if safety.on() {
		b, err := s.safetySnapshot(ctx, p.Site, safety.repo, safety.password, "pre-migration", step)
		if err != nil {
			return err
		}
		kept = b.SnapshotID
		if err := checkpoint(ctx, step, "the import (the safety snapshot "+kept+" is kept)"); err != nil {
			return err
		}
	} else {
		step(5, "No backup repository is configured: no safety snapshot is taken, and this import cannot be undone")
	}
	step(45, "Validating and extracting migration archive")
	res, err := agentrpc.Call(ctx, s.Agent, agentrpc.ImportMigration, p)
	if err != nil {
		if kept != "" {
			// The agent rolls a failed import back itself; the snapshot is
			// there for whatever its rollback could not put back.
			step(-1, "The import failed; the safety snapshot "+kept+" is kept and restores the site as it was before")
		}
		return err
	}
	step(85, "Recording immutable migration release")
	if _, err := s.Store.CreateDeployment(state.Deployment{
		SiteID: p.Site.ID, ReleaseID: p.ReleaseID, Status: state.DeployPromoted,
		Path: p.Site.RootPath + "/releases/" + p.ReleaseID,
	}); err != nil {
		return err
	}
	_ = agentrpc.Into(ctx, s.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: p.Site}, nil) // best effort
	s.warmInBackground(p.Site)
	done := "Migration imported and activated (" + strconv.Itoa(res.Files) + " files)"
	if res.Skipped != 0 {
		done += "; " + strconv.Itoa(res.Skipped) + " unsafe or special entries skipped"
	}
	if kept != "" {
		done += "; safety snapshot " + kept + " retained"
	}
	step(100, done)
	return nil
}

func (s *Server) handleImportMigration(w http.ResponseWriter, r *http.Request, c *call) {
	var o migrationOrder
	if !decodeOr400(w, r, &o) {
		return
	}
	task, release, err := s.startMigration(c.Site, c.User.Role, o)
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(c.Actor(), "migration.import", c.Site.Domain, o.Archive)
	respond(w, http.StatusAccepted, map[string]any{"task": task, "release_id": release})
}
