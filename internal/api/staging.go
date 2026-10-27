package api

// stagingService: a production site's staging copy (SPEC.md §3.7), its
// clone, credentials, diff, tables and database push. Safe Push
// (deployments.go) finds the copy here too.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

type stagingService struct{ srv *Server }

// stagingHost is the domain of prod's staging copy.
func stagingHost(prod state.Site) string { return "staging." + prod.Domain }

// Of is prod's staging copy, or state.ErrNotFound.
func (st stagingService) Of(prod state.Site) (state.Site, error) {
	return st.srv.Store.StagingSiteFor(prod.ID)
}

// Create records prod's staging copy and queues the clone.
func (st stagingService) Create(ctx context.Context, actor string, prod state.Site) (state.Site, jobs.Task, error) {
	srv := st.srv
	if prod.StagingOf != 0 {
		return state.Site{}, jobs.Task{}, refuse(probInvalid, "cannot stage a staging site")
	}
	if prod.Type.IsApp() {
		return state.Site{}, jobs.Task{}, refuse(probInvalid, "staging is not available for Node.js and Python sites")
	}
	if prod.Status == state.SiteDeleting {
		return state.Site{}, jobs.Task{}, refuse(probConflict, "this site is being deleted")
	}
	if _, err := st.Of(prod); err == nil {
		return state.Site{}, jobs.Task{}, refuse(probConflict, "staging already exists for this site")
	}
	// Where it will live is settled before anything is written.
	root, err := srv.siteRoot(ctx, stagingHost(prod))
	if err != nil {
		return state.Site{}, jobs.Task{}, agentFailure{err}
	}
	copyRec, sec, err := st.record(prod, root)
	if err != nil {
		return state.Site{}, jobs.Task{}, err
	}
	srv.audit(actor, "staging.create", prod.Domain, copyRec.Domain)
	task, err := srv.startJob(jobReq{
		kind: "staging.create", siteID: prod.ID, locks: siteLocks(prod.ID, copyRec.ID),
		payload: stagingPayload{StagingID: copyRec.ID},
	}, func(ctx context.Context, prod state.Site, progress func(int, string)) error {
		return st.clone(prod, copyRec, sec, progress)
	})
	if err != nil {
		srv.rollbackCreatedSite(copyRec)
		return state.Site{}, jobs.Task{}, queueFailure{err}
	}
	return copyRec, task, nil
}

// record writes a new staging site for prod, rooted at root, with its own
// identity, credentials and the password nginx asks for: staging is a copy
// of the live site, unfinished work included, and is not public.
func (st stagingService) record(prod state.Site, root string) (state.Site, siteSecrets, error) {
	store := st.srv.Store
	rec := prod
	rec.ID = 0
	rec.Domain = stagingHost(prod)
	rec.Aliases = nil
	rec.Status = state.SiteProvisioning
	rec.StagingOf = prod.ID
	rec.Config.HSTS = false
	// The staging name may be a domain or an alias of another site: 409.
	created, err := st.srv.siteSvc().insertNamed(rec)
	if err != nil {
		return state.Site{}, siteSecrets{}, err
	}
	// Whatever fails from here takes the record (and any secret) with it.
	fail := func(err error) (state.Site, siteSecrets, error) {
		st.srv.rollbackCreatedSite(created)
		return state.Site{}, siteSecrets{}, err
	}
	password := randomToken(15)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fail(err)
	}
	created, err = store.MutateSite(created.ID, func(s *state.Site) error {
		s.SystemUser = siteUser(s.ID)
		s.RootPath = root
		// The copy's routing names the copy's tree (internal/engine SPEC §7).
		s.Config.Routing = nginx.RebaseRouting(s.Config.Routing, prod.RootPath, root)
		if s.Config.Database.Enabled {
			s.Config.Database.Name = siteDatabase(s.ID)
			s.Config.Database.User = siteDatabase(s.ID)
		}
		s.Config.StagingAuthHash = string(hash)
		// A copy never inherits the original's development mode (#80).
		s.Config.DevModeUntil = time.Time{}
		// Nor the original's maintenance page, nor its key (the text may stay).
		s.Config.Maintenance.Enabled, s.Config.Maintenance.Key = false, ""
		// Nor its redirects (#83): the canonical host is the original's name
		// and a rule's target may be the original itself.
		s.Config.Redirects = state.SiteRedirects{}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	sec := newSiteSecrets("")
	if err := errors.Join(
		sec.store(store, created.ID),
		store.SetSecret(state.SiteSecret(created.ID, state.SiteSecretStagingPassword), password),
	); err != nil {
		return fail(err)
	}
	// The copy has a primary database of its own (only the primary is
	// staged, SPEC.md §9).
	if err := recordPrimaryDB(store, created); err != nil {
		return fail(err)
	}
	return created, sec, nil
}

// clone has the agent copy prod into the recorded staging site.
func (st stagingService) clone(prod, rec state.Site, sec siteSecrets, progress func(int, string)) error {
	srv := st.srv
	progress(20, "Cloning "+prod.Domain+" to "+rec.Domain)
	var built rpc.CreateSiteResult
	err := agentrpc.Into(context.Background(), srv.Agent, agentrpc.CreateStaging, rpc.StagingParams{
		Production: prod, Staging: rec, DBPassword: sec.dbPassword, ConnectorToken: sec.connectorToken,
	}, &built)
	if err != nil {
		srv.abandonCreatedSite(rec, err)
		return err
	}
	srv.logIfErr(srv.Store.MarkApplied(rec.ID, rec.DesiredGeneration, nil), "MarkApplied")
	srv.recordFiles(built.Files)
	// The copy has a name of its own, staging.<domain>: like a new site it
	// asks for its certificate (it was left on the default one).
	srv.firstCertificate(context.Background(), rec, progress)
	rec.Status = state.SiteActive
	if err := srv.setSiteStatus(rec.ID, state.SiteActive); err != nil {
		return err
	}
	if err := srv.applyCron(context.Background(), rec); err != nil {
		progress(95, "warning: could not install the staging site's crontab: "+err.Error())
	}
	progress(100, rec.Domain+" ready")
	return nil
}

// Recreate throws prod's staging copy away and clones production again.
func (st stagingService) Recreate(ctx context.Context, actor string, prod state.Site) (jobs.Task, error) {
	srv := st.srv
	old, err := st.Of(prod)
	if err != nil {
		return jobs.Task{}, refuse(probNotFound, "this site has no staging copy to recreate")
	}
	// What can fail before the old copy is gone is settled first.
	root, err := srv.siteRoot(ctx, stagingHost(prod))
	if err != nil {
		return jobs.Task{}, agentFailure{err}
	}
	srv.audit(actor, "staging.recreate", prod.Domain, old.Domain)
	task, err := srv.startJob(jobReq{kind: "staging.recreate", siteID: prod.ID, locks: siteLocks(prod.ID, old.ID)},
		func(ctx context.Context, prod state.Site, progress func(int, string)) error {
			gone, err := srv.Store.GetSite(old.ID)
			if errors.Is(err, state.ErrNotFound) {
				return errors.New("the staging copy was deleted before this job could run")
			}
			if err != nil {
				return err
			}
			progress(10, "Removing "+gone.Domain)
			if err := agentrpc.Into(ctx, srv.Agent, agentrpc.DeleteSite, rpc.SiteRef{Site: gone}, nil); err != nil {
				return fmt.Errorf("remove %s: %w", gone.Domain, err)
			}
			srv.forgetSiteFiles(gone)
			if err := srv.Store.DeleteSite(gone.ID); err != nil {
				return err
			}
			if err := checkpoint(ctx, progress, "cloning production again (the old staging copy is removed)"); err != nil {
				return err
			}
			rec, sec, err := st.record(prod, root)
			if err != nil {
				return err
			}
			return st.clone(prod, rec, sec, progress)
		})
	if err != nil {
		return jobs.Task{}, queueFailure{err}
	}
	return task, nil
}

// stagingCredentials is how to reach a staging copy through its password.
type stagingCredentials struct {
	URL      string `json:"url"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// Credentials reads the password protecting prod's staging copy.
func (st stagingService) Credentials(actor string, prod state.Site) (stagingCredentials, error) {
	stg, err := st.Of(prod)
	if err != nil {
		return stagingCredentials{}, refuse(probNotFound, "this site has no staging copy")
	}
	pw, err := st.srv.Store.Secret(state.SiteSecret(stg.ID, state.SiteSecretStagingPassword))
	if err != nil {
		return stagingCredentials{}, fmt.Errorf("secret store: %w", err)
	}
	st.srv.audit(actor, "staging.credentials", stg.Domain, "")
	return stagingCredentials{URL: "https://" + stg.Domain + "/", User: "staging", Password: pw}, nil
}

// Diff is what differs between prod and its staging copy, for review
// before a push.
func (st stagingService) Diff(ctx context.Context, prod state.Site) (rpc.StagingDiffResult, error) {
	stg, err := st.Of(prod)
	if err != nil {
		return rpc.StagingDiffResult{}, refuse(probNotFound, "this site has no staging copy")
	}
	// Mid-clone the copy's tree is partial: a diff then lists most of
	// production as removed.
	if st.cloning(prod, stg) {
		return rpc.StagingDiffResult{}, refuse(probConflict, msgStagingCloning)
	}
	var res rpc.StagingDiffResult
	if err := agentrpc.Into(ctx, st.srv.Agent, agentrpc.StagingDiff, rpc.StagingParams{Production: prod, Staging: stg}, &res); err != nil {
		return rpc.StagingDiffResult{}, agentFailure{err}
	}
	return res, nil
}

// msgStagingCloning answers a diff asked while the copy is being made.
const msgStagingCloning = "the staging copy is still being cloned"

// cloning reports whether stg is not complete yet: still provisioning, or
// a clone or reclone of prod queued or running (a reclone removes the old
// copy's tree while its record still reads active).
func (st stagingService) cloning(prod, stg state.Site) bool {
	if stg.Status == state.SiteProvisioning {
		return true
	}
	active, err := st.srv.Store.ActiveJobs()
	if err != nil {
		st.srv.logIfErr(err, "ActiveJobs")
		return false
	}
	for _, j := range active {
		if j.SiteID == prod.ID && (j.Kind == "staging.create" || j.Kind == "staging.recreate") {
			return true
		}
	}
	return false
}

// errNoStaging is a staging operation on a site that has no copy.
var errNoStaging = refuse(probPrecondition, "create a staging site first")

// Tables lists the staging copy's tables with the agent's push policy
// applied: which tables are protected is the agent's to say.
func (st stagingService) Tables(ctx context.Context, prod state.Site) (state.Site, []rpc.StagingTable, error) {
	stg, err := st.Of(prod)
	if err != nil {
		return state.Site{}, nil, errNoStaging
	}
	var res rpc.StagingTablesResult
	if err := agentrpc.Into(ctx, st.srv.Agent, agentrpc.StagingTables, rpc.StagingParams{Production: prod, Staging: stg}, &res); err != nil {
		return state.Site{}, nil, agentFailure{err}
	}
	if res.Tables == nil {
		res.Tables = []rpc.StagingTable{}
	}
	return stg, res.Tables, nil
}

// stagingPush is the body of a database push: the tables to copy, and
// production's domain typed out to confirm.
type stagingPush struct {
	Tables  []string `json:"tables"`
	Confirm string   `json:"confirm"`
}

// maxPushTables bounds one database push.
const maxPushTables = 50

// PushDatabase queues the copy of the selected tables of staging's
// database over production's, behind a verified safety snapshot. What the
// agent's policy refuses is refused here first, before any snapshot.
func (st stagingService) PushDatabase(ctx context.Context, actor string, prod state.Site, req stagingPush) (jobs.Task, error) {
	srv := st.srv
	stg, err := st.Of(prod)
	if err != nil {
		return jobs.Task{}, errNoStaging
	}
	if req.Confirm != prod.Domain {
		return jobs.Task{}, refuse(probInvalid, "type the production domain to confirm database promotion")
	}
	if n := len(req.Tables); n == 0 || n > maxPushTables {
		return jobs.Task{}, refuse(probInvalid, "select between 1 and 50 tables")
	}
	if err := st.checkPushable(ctx, prod, req.Tables); err != nil {
		return jobs.Task{}, err
	}
	repo, password, err := srv.backupRepository()
	if err != nil {
		return jobs.Task{}, err
	}
	if repo == "" || password == "" {
		return jobs.Task{}, refuse(probPrecondition, "configure backup_repository and backup_password in settings first")
	}
	srv.audit(actor, "staging.database-push", prod.Domain, strings.Join(req.Tables, ","))
	// Both sites' locks and the repository's: one restic process on it,
	// and no scheduled backup of production while its tables change.
	locks := append(siteLocks(prod.ID, stg.ID), jobs.RepoLock(repo))
	task, err := srv.startJob(jobReq{kind: "staging.database-push", siteID: prod.ID, locks: locks},
		func(ctx context.Context, prod state.Site, progress func(int, string)) error {
			return st.pushTables(ctx, prod, stg.ID, req.Tables, repo, password, progress)
		})
	if err != nil {
		return jobs.Task{}, queueFailure{err}
	}
	return task, nil
}

// checkPushable asks the agent for staging's tables and its policy, and
// refuses the first requested table staging lacks or the policy protects.
// The agent checks again under its own locks.
func (st stagingService) checkPushable(ctx context.Context, prod state.Site, tables []string) error {
	_, listed, err := st.Tables(ctx, prod)
	if err != nil {
		return err
	}
	byName := make(map[string]rpc.StagingTable, len(listed))
	for _, t := range listed {
		byName[t.Name] = t
	}
	for _, name := range tables {
		t, ok := byName[name]
		if !ok {
			return refuse(probInvalid, "staging has no table "+name)
		}
		if t.Protected {
			return refuse(probInvalid, "selection contains protected live data table "+name+" ("+t.Reason+")")
		}
	}
	return nil
}

// pushTables is the staging.database-push job.
func (st stagingService) pushTables(ctx context.Context, prod state.Site, stagingID int64, tables []string, repo, password string, progress func(int, string)) error {
	srv := st.srv
	stg, err := srv.Store.GetSite(stagingID)
	if err != nil {
		return fmt.Errorf("the staging copy: %w", err)
	}
	safety, err := srv.safetySnapshot(ctx, prod, repo, password, "pre-database-push", progress)
	if err != nil {
		return err
	}
	kept := safety.SnapshotID
	if err := checkpoint(ctx, progress, "promoting the tables (the safety snapshot "+kept+" is kept)"); err != nil {
		return err
	}
	progress(45, fmt.Sprintf("Promoting %d selected tables", len(tables)))
	var synced map[string]any
	if err := agentrpc.Into(ctx, srv.Agent, agentrpc.SyncStagingDB, rpc.SyncStagingDBParams{Production: prod, Staging: stg, Tables: tables}, &synced); err != nil {
		return err
	}
	progress(90, "Purging production cache")
	_ = agentrpc.Into(ctx, srv.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: prod}, nil)
	progress(100, "Selected database tables promoted; safety snapshot "+kept+" retained")
	return nil
}
