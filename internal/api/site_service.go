package api

// siteService: creating, listing and deleting sites (SPEC.md §3.1–2.3).
// Settings changes are in site_settings.go, their job in site_apply.go.

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// siteService is the sites domain. It holds nothing of its own: the
// server's store, agent and job queue are its state.
type siteService struct{ srv *Server }

// newSite is the body of POST /api/sites.
type newSite struct {
	Domain        string   `json:"domain"`
	Aliases       []string `json:"aliases"`
	Type          string   `json:"type"`
	Profile       string   `json:"profile"`
	PHPVersion    string   `json:"php_version"`
	Title         string   `json:"title"`
	AdminEmail    string   `json:"admin_email"`
	AdminUser     string   `json:"admin_user"`
	AdminPassword string   `json:"admin_password"`
	ProxyUpstream string   `json:"proxy_upstream"`
	// A managed app (§3.19): its runtime version and start command; empty
	// takes the defaults.
	RuntimeVersion string `json:"runtime_version"`
	StartCommand   string `json:"start_command"`
	CacheEnabled   *bool  `json:"cache_enabled"`
	ObjectCache    bool   `json:"object_cache"`
	// The primary database (site_create_db.go): asked for on a static or
	// PHP site, its names and password chosen or the panel's.
	CreateDatabase   *bool  `json:"create_database"`
	DatabaseName     string `json:"database_name"`
	DatabaseUser     string `json:"database_user"`
	DatabasePassword string `json:"database_password"`
}

// siteKinds are the types a site can be created as.
var siteKinds = []state.SiteType{
	state.SiteWordPress, state.SiteWooCommerce, state.SiteStatic,
	state.SitePHP, state.SiteLaravel, state.SiteProxy, state.SiteNode, state.SitePython,
}

// knownProfile reports whether p is one of the three tuning profiles.
func knownProfile(p state.Profile) bool {
	return p == state.ProfileBalanced || p == state.ProfileCommerce || p == state.ProfileMaximum
}

// runsPHP reports whether sites of type t have a PHP pool.
func runsPHP(t state.SiteType) bool {
	return t != state.SiteStatic && t != state.SiteProxy && !t.IsApp()
}

// backedUp reports whether sites of type t have anything to back up: every
// type but a reverse proxy, whose tree holds no content of its own and
// which has no database (its restore test could only find an empty tree).
func backedUp(t state.SiteType) bool { return t != state.SiteProxy }

// startsWithDB reports whether sites of type t are made with a database
// and an account (their primary, which the application is configured
// with). A static, plain PHP or proxy site has none until one is asked
// for (Fase 9b).
func startsWithDB(t state.SiteType) bool {
	return t == state.SiteWordPress || t == state.SiteWooCommerce || t == state.SiteLaravel
}

// recordPrimaryDB records the site's primary database and account, the
// account granted all of it, its password the site's db_password secret
// (stored before by siteSecrets.store). A name another site has is
// errPrimaryTaken.
func recordPrimaryDB(st *state.Store, rec state.Site) error {
	if !localDB(rec) {
		return nil
	}
	d := rec.Config.Database
	if _, err := st.AddSiteDatabase(rec.ID, d.Name); err != nil {
		if errors.Is(err, state.ErrDBNameTaken) {
			return errPrimaryTaken{fieldDBName, d.Name}
		}
		return err
	}
	_, err := st.AddDBUser(rec.ID, d.User, state.SiteSecret(rec.ID, state.SiteSecretDBPassword), "",
		[]state.DBGrant{{Database: d.Name, Level: site.GrantAll}})
	if errors.Is(err, state.ErrDBNameTaken) {
		return errPrimaryTaken{fieldDBUser, d.User}
	}
	return err
}

// cleanHost is a host name as the panel stores it.
func cleanHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// List is the sites the user may see: an operator's own (and their staging
// copies), everyone else's all of them.
func (svc siteService) List(user state.User) ([]state.Site, error) {
	var (
		list []state.Site
		err  error
	)
	if user.Role == "operator" {
		list, err = svc.srv.Store.ListSitesForUser(user.ID)
	} else {
		list, err = svc.srv.Store.ListSites()
	}
	if err != nil {
		return nil, err
	}
	return list, nil
}

// wpAdmin is the first WordPress account a create job installs.
type wpAdmin struct{ email, user, password, title string }

// siteSecrets are the credentials made for a new site.
type siteSecrets struct{ dbPassword, connectorToken string }

// newSiteSecrets makes them; dbPassword, when not "", is the password
// the site's creator chose for its database user.
func newSiteSecrets(dbPassword string) siteSecrets {
	if dbPassword == "" {
		dbPassword = randomToken(24)
	}
	return siteSecrets{dbPassword: dbPassword, connectorToken: randomToken(24)}
}

// store writes the secrets of site id.
func (sec siteSecrets) store(st *state.Store, id int64) error {
	return errors.Join(
		st.SetSecret(state.SiteSecret(id, state.SiteSecretDBPassword), sec.dbPassword),
		st.SetSecret(state.SiteSecret(id, state.SiteSecretConnectorToken), sec.connectorToken),
	)
}

// checkNewSite validates a create request and fills in its defaults; the
// record it answers has no ID, user or root yet, and no database (the
// database it asks for is the second answer).
func checkNewSite(req *newSite) (state.Site, newSiteDB, error) {
	req.Domain = cleanHost(req.Domain)
	if err := site.ValidateDomain(req.Domain); err != nil {
		return state.Site{}, newSiteDB{}, refuse(probInvalid, err.Error())
	}
	if len(req.Aliases) > maxAliases {
		return state.Site{}, newSiteDB{}, refuse(probInvalid, fmt.Sprintf("at most %d aliases", maxAliases))
	}
	for i := range req.Aliases {
		req.Aliases[i] = cleanHost(req.Aliases[i])
		a := req.Aliases[i]
		if err := site.ValidateDomain(a); err != nil {
			return state.Site{}, newSiteDB{}, refuse(probInvalid, err.Error())
		}
		if a == req.Domain || slices.Contains(req.Aliases[:i], a) {
			return state.Site{}, newSiteDB{}, refuse(probInvalid, "alias "+a+" is the site's own domain or listed twice")
		}
	}
	kind := state.SiteType(req.Type)
	known := false
	for _, k := range siteKinds {
		known = known || k == kind
	}
	if !known {
		return state.Site{}, newSiteDB{}, refuse(probInvalid, "invalid site type")
	}
	profile := state.Profile(req.Profile)
	if profile == "" {
		profile = state.ProfileBalanced
		if kind == state.SiteWooCommerce {
			profile = state.ProfileCommerce
		}
	}
	if !knownProfile(profile) {
		return state.Site{}, newSiteDB{}, refuse(probInvalid, "invalid profile")
	}
	if isWordPress(kind) {
		if _, err := mail.ParseAddress(req.AdminEmail); err != nil {
			return state.Site{}, newSiteDB{}, refuse(probInvalid, "valid admin email required for WordPress")
		}
		if req.AdminUser == "" || len(req.AdminPassword) < 12 {
			return state.Site{}, newSiteDB{}, refuse(probInvalid, "admin user and a 12+ character password required")
		}
	}
	db, err := checkNewSiteDB(req, kind)
	if err != nil {
		return state.Site{}, newSiteDB{}, err
	}
	if kind == state.SiteProxy {
		up, err := site.NormalizeProxyUpstream(req.ProxyUpstream)
		if err != nil {
			return state.Site{}, newSiteDB{}, refuse(probInvalid, err.Error())
		}
		req.ProxyUpstream = up
	}
	cache := runsPHP(kind)
	if req.CacheEnabled != nil {
		cache = cache && *req.CacheEnabled
	}
	rec := state.Site{
		Domain:  req.Domain,
		Aliases: req.Aliases,
		Type:    kind,
		Profile: profile,
		Engine:  state.EngineNginx,
		Status:  state.SiteProvisioning,
		Config: state.SiteConfig{
			CacheEnabled:  cache,
			ObjectCache:   req.ObjectCache,
			ProxyUpstream: req.ProxyUpstream,
		},
	}
	if kind == state.SiteLaravel {
		rec.Config.PublicRoot = "public"
	}
	if kind.IsApp() {
		rt := runtimes.Of(kind)
		app := state.SiteApp{Version: strings.TrimSpace(req.RuntimeVersion), Start: strings.TrimSpace(req.StartCommand)}
		if app.Version == "" {
			app.Version = "22"
			if rt == runtimes.Python {
				app.Version = "3.12"
			}
		}
		if app.Start == "" {
			app.Start = runtimes.DefaultStart(rt)
		}
		if err := runtimes.CheckApp(kind, app); err != nil {
			return state.Site{}, newSiteDB{}, refuse(probInvalid, err.Error())
		}
		rec.Config.App = app
	}
	return rec, db, nil
}

// Create records a new site and queues its provisioning (§3.2).
func (svc siteService) Create(ctx context.Context, actor string, req newSite) (state.Site, jobs.Task, error) {
	out, err := svc.create(ctx, actor, req, "")
	return out.site, out.task, err
}

// siteCreated is what a create answers: the record, its job and the
// summary for its creator, which holds the database password.
type siteCreated struct {
	site    state.Site
	task    jobs.Task
	summary siteSummary
}

// create is Create; reqHost is the Host the request named (for the
// summary's panel link).
func (svc siteService) create(ctx context.Context, actor string, req newSite, reqHost string) (siteCreated, error) {
	srv := svc.srv
	rec, db, err := checkNewSite(&req)
	if err != nil {
		return siteCreated{}, err
	}
	caps, err := srv.capabilities(ctx)
	if err != nil {
		return siteCreated{}, agentFailure{err}
	}
	if caps.Paths.SitesRoot == "" {
		return siteCreated{}, agentFailure{errNoSitesRoot}
	}
	if runsPHP(rec.Type) {
		rec.PHPVersion = req.PHPVersion
		if rec.PHPVersion == "" {
			rec.PHPVersion = srv.defaultPHP(caps)
		} else if msg := phpVersionError(caps, rec.PHPVersion); msg != "" {
			return siteCreated{}, refuse(probInvalid, msg)
		}
	}
	rec.SystemUser = "pending" // named after the ID, once there is one
	rec.RootPath = filepath.Join(caps.Paths.SitesRoot, rec.Domain)
	if db.want {
		if err := svc.claimFree(db); err != nil {
			return siteCreated{}, err
		}
	}

	created, err := svc.insertNamed(rec)
	if err != nil {
		return siteCreated{}, err
	}
	created, err = srv.Store.MutateSite(created.ID, func(st *state.Site) error {
		st.SystemUser = siteUser(st.ID)
		if db.want {
			st.Config.Database = db.primaryConfig(st.ID)
		}
		// A proxy site keeps nothing of its own here (its content is its
		// upstream's): it is never backed up (SPEC.md §7.1).
		st.Config.Backups = state.BackupPolicy{Enabled: backedUp(st.Type), Schedule: "daily", RetentionDays: 30}
		return nil
	})
	if err != nil {
		srv.logIfErr(srv.Store.DeleteSite(created.ID), "DeleteSite")
		return siteCreated{}, err
	}
	// A site whose credentials could not be kept could never reach its
	// database: nothing is provisioned for it.
	sec := newSiteSecrets(db.password)
	if err := sec.store(srv.Store, created.ID); err != nil {
		srv.logIfErr(srv.Store.DeleteSite(created.ID), "DeleteSite")
		return siteCreated{}, err
	}
	if err := recordPrimaryDB(srv.Store, created); err != nil {
		srv.logIfErr(srv.Store.DeleteSite(created.ID), "DeleteSite")
		if errors.Is(err, state.ErrDBNameTaken) {
			return siteCreated{}, takenProblem(err)
		}
		return siteCreated{}, err
	}
	srv.audit(actor, "site.create", created.Domain, string(created.Type))

	var admin *wpAdmin
	if isWordPress(created.Type) {
		admin = &wpAdmin{email: req.AdminEmail, user: req.AdminUser, password: req.AdminPassword, title: req.Title}
	}
	task, err := srv.startJob(jobReq{kind: "site.create", siteID: created.ID, locks: siteLocks(created.ID)},
		func(ctx context.Context, rec state.Site, progress func(int, string)) error {
			return svc.provision(ctx, rec, sec, admin, progress)
		})
	if err != nil {
		srv.rollbackCreatedSite(created)
		return siteCreated{}, queueFailure{err}
	}
	ps, _ := srv.Store.PanelSettings()
	return siteCreated{site: created, task: task,
		summary: summaryOf(created, req.AdminUser, sec.dbPassword, caps.Paths.MySQLSocket, panelHost(ps, reqHost))}, nil
}

// insertNamed inserts a new site record once no other site uses its domain
// or any of its aliases, under aliasesMu (site_aliases.go).
func (svc siteService) insertNamed(rec state.Site) (state.Site, error) {
	aliasesMu.Lock()
	defer aliasesMu.Unlock()
	taken, err := nameOwners(svc.srv.Store, append([]string{rec.Domain}, rec.Aliases...), 0)
	if err != nil {
		return state.Site{}, err
	}
	if owner, ok := taken[rec.Domain]; ok {
		if owner == rec.Domain {
			return state.Site{}, refuse(probConflict, "a site with this domain already exists")
		}
		return state.Site{}, refuse(probConflict, rec.Domain+" is already an alias of "+owner)
	}
	for _, a := range rec.Aliases {
		if owner, ok := taken[a]; ok {
			return state.Site{}, refuse(probConflict, "alias "+a+" is already used by "+owner)
		}
	}
	created, err := svc.srv.Store.CreateSite(rec)
	if errors.Is(err, state.ErrDomainTaken) {
		return state.Site{}, refuse(probConflict, "a site with this domain already exists")
	}
	return created, err
}

// siteUser and siteDatabase are the names a site's ID gives it.
func siteUser(id int64) string     { return "cg-site-" + strconv.FormatInt(id, 10) }
func siteDatabase(id int64) string { return "site_" + strconv.FormatInt(id, 10) }

// provision is the site.create job (§3.2).
func (svc siteService) provision(ctx context.Context, rec state.Site, sec siteSecrets, admin *wpAdmin, progress func(int, string)) error {
	srv := svc.srv
	progress(10, "Provisioning "+rec.Domain)
	if rec.Type.IsApp() {
		if err := srv.ensureRuntime(ctx, rec, progress); err != nil {
			srv.abandonCreatedSite(rec, err)
			return err
		}
	}
	params := rpc.CreateSiteParams{Site: rec, DBPassword: sec.dbPassword, ConnectorToken: sec.connectorToken}
	if admin != nil {
		params.AdminEmail, params.AdminUser, params.AdminPassword, params.SiteTitle = admin.email, admin.user, admin.password, admin.title
	}
	var built rpc.CreateSiteResult
	if err := agentrpc.Into(ctx, srv.Agent, agentrpc.CreateSite, params, &built); err != nil {
		srv.abandonCreatedSite(rec, err)
		return err
	}
	srv.logIfErr(srv.Store.MarkApplied(rec.ID, rec.DesiredGeneration, nil), "MarkApplied")
	progress(60, "Configuration rendered, recording state")
	for _, f := range built.Files {
		if err := srv.Store.RecordManagedFile(f.Path, f.SHA256); err != nil {
			srv.teardownCreatedSite(rec)
			return fmt.Errorf("record managed configuration: %w", err)
		}
	}
	srv.firstCertificate(ctx, rec, progress)
	rec.Status = state.SiteActive
	if err := srv.setSiteStatus(rec.ID, state.SiteActive); err != nil {
		srv.teardownCreatedSite(rec)
		return err
	}
	// The site serves provisioning's release: the Releases list shows it.
	srv.recordInitialRelease(rec.ID, state.DeployPromoted)
	if isWordPress(rec.Type) {
		// WordPress's scheduled events run from the site's crontab.
		if err := srv.applyCron(ctx, rec); err != nil {
			progress(95, "warning: could not install the site's crontab: "+err.Error())
		}
	}
	progress(100, rec.Domain+" is live")
	return nil
}

// recordFiles records files the agent wrote; a failure is only logged.
func (s *Server) recordFiles(files []rpc.ManagedFile) {
	for _, f := range files {
		s.logIfErr(s.Store.RecordManagedFile(f.Path, f.SHA256), "RecordManagedFile")
	}
}

// setSiteStatus changes a site's status alone.
func (s *Server) setSiteStatus(id int64, status state.SiteStatus) error {
	_, err := s.Store.MutateSite(id, func(st *state.Site) error {
		st.Status = status
		return nil
	})
	return err
}

// markSiteError puts a site in error, logging a failed write.
func (s *Server) markSiteError(id int64) {
	s.logIfErr(s.setSiteStatus(id, state.SiteError), "setSiteStatus")
}

// Delete marks the site deleting and queues its removal, with its staging
// copy if it is a production site (§3.3).
func (svc siteService) Delete(actor string, target state.Site) (jobs.Task, error) {
	srv := svc.srv
	before := target.Status
	srv.logIfErr(srv.setSiteStatus(target.ID, state.SiteDeleting), "setSiteStatus")

	var stagingID int64
	if target.StagingOf == 0 {
		if stg, err := srv.stagingSvc().Of(target); err == nil {
			stagingID = stg.ID
		}
	}
	task, err := srv.startJob(jobReq{
		kind: "site.delete", siteID: target.ID, locks: siteLocks(target.ID, stagingID),
		dedupKey: "site.delete:" + strconv.FormatInt(target.ID, 10),
		payload:  deletePayload{PreviousStatus: before, StagingID: stagingID},
	}, func(ctx context.Context, rec state.Site, progress func(int, string)) error {
		if stagingID != 0 {
			stg, err := srv.Store.GetSite(stagingID)
			switch {
			case errors.Is(err, state.ErrNotFound):
				// Removed meanwhile: nothing to do for it.
			case err != nil:
				return err
			default:
				progress(15, "Removing staging environment "+stg.Domain)
				if err := svc.remove(ctx, stg); err != nil {
					return err
				}
			}
		}
		if err := checkpoint(ctx, progress, "removing "+rec.Domain); err != nil {
			// Production is untouched; it gets its status back.
			srv.logIfErr(srv.setSiteStatus(rec.ID, before), "setSiteStatus")
			return err
		}
		progress(50, "Removing "+rec.Domain)
		if err := svc.remove(ctx, rec); err != nil {
			return err
		}
		progress(100, "Removed")
		return nil
	})
	if err != nil {
		// Nothing will delete it: it gets its status back.
		if before != state.SiteDeleting {
			srv.logIfErr(srv.setSiteStatus(target.ID, before), "setSiteStatus")
		}
		return jobs.Task{}, queueFailure{err}
	}
	srv.audit(actor, "site.delete", target.Domain, "")
	return task, nil
}

// remove takes one site off the server and out of the records. An agent
// failure leaves the site in error.
func (svc siteService) remove(ctx context.Context, target state.Site) error {
	srv := svc.srv
	if err := agentrpc.Into(ctx, srv.Agent, agentrpc.DeleteSite, rpc.SiteRef{Site: target}, nil); err != nil {
		srv.markSiteError(target.ID)
		return fmt.Errorf("remove %s: %w", target.Domain, err)
	}
	srv.forgetSiteFiles(target)
	// Secrets, certificate state and alert dismissals go with the row.
	return srv.Store.DeleteSite(target.ID)
}

// Purge empties the site's page cache: every page, or the URLs given.
func (svc siteService) Purge(ctx context.Context, actor string, target state.Site, urls []string) (rpc.PurgeResult, error) {
	var res rpc.PurgeResult
	if err := agentrpc.Into(ctx, svc.srv.Agent, agentrpc.PurgeCache, rpc.PurgeParams{Site: target, URLs: urls}, &res); err != nil {
		return rpc.PurgeResult{}, agentFailure{err}
	}
	svc.srv.audit(actor, "cache.purge", target.Domain, fmt.Sprintf("%d urls, %d removed", len(urls), res.Removed))
	return res, nil
}
