package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Restoring a site from another server's backups (SPEC.md §5.10, #193): the
// sites the configured repository holds that this panel does not have, and
// the restore of one, which creates the site with the normal provisioning
// and then restores its backup whole. It is the one restore that takes a
// snapshot another server made (ops/backup SPEC §5).

// foreignSite is one entry of GET /api/backups/foreign.
type foreignSite struct {
	Domain    string `json:"domain"`
	Hostname  string `json:"hostname"`
	Snapshot  string `json:"snapshot"`
	Time      string `json:"time"`
	Snapshots int    `json:"snapshots"`
	Manifest  bool   `json:"manifest"`
}

func (s *Server) handleForeignBackups(w http.ResponseWriter, r *http.Request, c *call) {
	repo, password, ok := s.backupConfig(w)
	if !ok {
		return
	}
	var res rpc.RepositorySitesResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.RepositorySites, rpc.TestBackupParams{Repository: repo, Password: password, Env: s.backupEnv()}, &res); err != nil {
		respondAgentErr(w, 0, "the backup repository cannot be read", err)
		return
	}
	domains := make([]string, 0, len(res.Sites))
	for _, x := range res.Sites {
		domains = append(domains, x.Domain)
	}
	taken, err := nameOwners(s.Store, domains, 0)
	if err != nil {
		internalError(w, "nameOwners", err, "could not read the sites")
		return
	}
	out := []foreignSite{}
	for _, x := range res.Sites {
		if _, here := taken[x.Domain]; here {
			continue
		}
		out = append(out, foreignSite{Domain: x.Domain, Hostname: x.Hostname, Snapshot: x.Snapshot,
			Time: x.Time.UTC().Format("2006-01-02T15:04:05Z"), Snapshots: x.Snapshots, Manifest: x.Manifest})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	respond(w, http.StatusOK, map[string]any{"sites": out})
}

// foreignRestore is the body of POST /api/backups/foreign/restore. Type and
// PHPVersion are needed only for a backup without a description (before
// 1.0); given, they win over the description's.
type foreignRestore struct {
	Snapshot   string `json:"snapshot"`
	Confirm    string `json:"confirm"`
	Type       string `json:"type"`
	PHPVersion string `json:"php_version"`
}

func (s *Server) handleForeignRestore(w http.ResponseWriter, r *http.Request, c *call) {
	var req foreignRestore
	if !decodeOr400(w, r, &req) {
		return
	}
	if !rpc.ValidSnapshotID(req.Snapshot) {
		respondErr(w, http.StatusBadRequest, "invalid snapshot id")
		return
	}
	repo, password, ok := s.backupConfig(w)
	if !ok {
		return
	}
	var snap rpc.ForeignSnapshotResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ForeignSnapshot, rpc.ForeignSnapshotParams{
		Repository: repo, Password: password, Env: s.backupEnv(), SnapshotID: req.Snapshot}, &snap); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	if req.Confirm != snap.Domain {
		respondErr(w, http.StatusBadRequest, "type the site's domain to confirm the restore")
		return
	}
	site, plan, err := foreignNewSite(snap, req)
	if err != nil {
		replyFailure(w, err)
		return
	}
	svc := s.siteSvc()
	p, err := svc.prepare(r.Context(), c.Actor(), site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	s.audit(c.Actor(), "backup.import", snap.Domain, fmt.Sprintf("snapshot %s made on %s", shortID(snap.ID), snap.Hostname))
	task, err := s.startJob(jobReq{kind: "backup.import", siteID: p.site.ID,
		locks:   append(siteLocks(p.site.ID), jobs.RepoLock(repo)),
		payload: jobRecord{SiteID: p.site.ID, Domain: p.site.Domain},
	}, func(ctx context.Context, rec state.Site, progress func(int, string)) error {
		return s.importSite(ctx, p, rec, snap, plan, progress)
	})
	if err != nil {
		s.rollbackCreatedSite(p.site)
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"site": c.siteView(p.site), "task": task})
}

// siteNameRe is the shape of the panel's own database names, site_<id>:
// another server's are never taken over by a new site (§3.2 step 6).
var siteNameRe = regexp.MustCompile(`^site_[0-9]+$`)

// importPlan is what a restore from another server keeps of the backup:
// its primary database's name ("" for one from before they were named),
// the account the primary was configured with, and the other accounts.
type importPlan struct {
	primary  string
	account  *rpc.ManifestAccount
	accounts []rpc.ManifestAccount
	external map[string]bool
}

// foreignNewSite is the site a backup of another server describes, as
// POST /api/sites would take it, with the backup's ID, database names and
// primary password where this panel can keep them (§5.10), and the plan of
// the rest.
func foreignNewSite(snap rpc.ForeignSnapshotResult, req foreignRestore) (newSite, importPlan, error) {
	m := snap.Manifest
	site := newSite{Domain: snap.Domain, Type: req.Type, PHPVersion: req.PHPVersion}
	plan := importPlan{primary: backupPrimary(snap), external: map[string]bool{}}
	if m != nil {
		if site.Type == "" {
			site.Type = string(m.Type)
		}
		if site.PHPVersion == "" && runsPHP(m.Type) {
			site.PHPVersion = m.PHPVersion
		}
		site.Aliases = m.Aliases
		site.Profile = string(m.Profile)
		cache := m.Cache.Enabled
		site.CacheEnabled = &cache
		site.ObjectCache = m.Cache.ObjectCache
		if m.Runtime != nil {
			site.RuntimeVersion, site.StartCommand = m.Runtime.Version, m.Runtime.Start
		}
		site.keepID = m.SiteID
		for _, d := range m.Databases {
			if d.External {
				plan.external[d.Name] = true
			}
		}
		for i := range m.Accounts {
			if m.Accounts[i].Primary && plan.account == nil {
				plan.account = &m.Accounts[i]
			} else {
				plan.accounts = append(plan.accounts, m.Accounts[i])
			}
		}
	}
	if site.Type == "" {
		return newSite{}, importPlan{}, refuse(probInvalid, "this backup has no description of the site (it was made before 1.0): give its type, and its PHP version for a PHP site")
	}
	if state.SiteType(site.Type) == state.SiteProxy {
		return newSite{}, importPlan{}, refuse(probInvalid, "a reverse-proxy site has no backup to restore")
	}
	if len(snap.Databases) > 0 && !startsWithDB(state.SiteType(site.Type)) {
		yes := true
		site.CreateDatabase = &yes
	}
	if len(snap.Databases) > 0 || startsWithDB(state.SiteType(site.Type)) {
		// The backup's names, unless they are the panel's own site_<n>:
		// those come back by keeping the ID.
		if plan.primary != "" && !siteNameRe.MatchString(plan.primary) {
			site.DatabaseName = plan.primary
		}
		if a := plan.account; a != nil {
			if !siteNameRe.MatchString(a.Name) {
				site.DatabaseUser = a.Name
			}
			if dbPasswordProblem(a.Password) == "" {
				site.DatabasePassword = a.Password
			}
		}
	}
	if isWordPress(state.SiteType(site.Type)) {
		// Placeholders: the restored database brings the site's own accounts.
		site.Title = snap.Domain
		site.AdminEmail = "admin@" + snap.Domain
		site.AdminUser = "cloudground-restore"
		site.AdminPassword = randomToken(24)
	}
	return site, plan, nil
}

// backupPrimary is the primary database of a backup: the one its
// description says, else the only one, else the one named site_<n>.
func backupPrimary(snap rpc.ForeignSnapshotResult) string {
	if snap.Manifest != nil {
		for _, d := range snap.Manifest.Databases {
			if d.Primary {
				return d.Name
			}
		}
		return ""
	}
	if len(snap.Databases) == 1 {
		return snap.Databases[0]
	}
	for _, d := range snap.Databases {
		if siteNameRe.MatchString(d) {
			return d
		}
	}
	return ""
}

// importSite is the backup.import job (§5.10): the normal provisioning, the
// backup's other databases and accounts made, the restore of the backup
// whole, then a backup of the site on this server, restore-tested.
func (s *Server) importSite(ctx context.Context, p preparedSite, rec state.Site, snap rpc.ForeignSnapshotResult, plan importPlan, progress func(int, string)) error {
	if err := s.siteSvc().provision(ctx, rec, p.sec, p.admin, func(pct int, msg string) { progress(pct*3/10, msg) }); err != nil {
		return err
	}
	site, err := s.Store.GetSite(rec.ID)
	if err != nil {
		return err
	}
	kept := func(err error) error {
		return fmt.Errorf("%s was created but its backup could not be restored (delete it to try again): %w", site.Domain, err)
	}
	if snap.Manifest != nil && snap.Manifest.SiteID > 0 && site.ID != snap.Manifest.SiteID {
		progress(32, fmt.Sprintf("Site ID %d is taken on this panel: %s is site %d here", snap.Manifest.SiteID, site.Domain, site.ID))
	}
	// The backup's primary goes into the new site's primary, renamed only
	// when its name could not be kept.
	d := site.Config.Database
	rename := map[string]string{}
	if plan.primary != "" && d.Enabled && !d.External && plan.primary != d.Name {
		rename[plan.primary] = d.Name
	}
	var skipped []string
	for _, name := range snap.Databases {
		if name == "" || name == plan.primary {
			continue
		}
		if plan.external[name] {
			skipped = append(skipped, name)
			progress(35, "warning: the database "+name+" was on an external database server; it is not restored here")
			continue
		}
		if err := s.addDatabaseNow(ctx, site, name); err != nil {
			skipped = append(skipped, name)
			progress(35, fmt.Sprintf("warning: the database %s cannot be made here (%v); the restore leaves it out", name, err))
			continue
		}
		progress(35, "Database "+name+" created")
	}
	mapGrants := func(gs []state.DBGrant) []state.DBGrant {
		var out []state.DBGrant
		for _, g := range gs {
			if to, ok := rename[g.Database]; ok {
				g.Database = to
			}
			if g.Database != "" && !slices.Contains(skipped, g.Database) {
				out = append(out, g)
			}
		}
		return out
	}
	dbs := s.databases()
	// The primary account's grants beyond its own database (made by the
	// provisioning with all of the primary).
	if a := plan.account; a != nil && a.Name == d.User && len(a.Grants) > 1 {
		if _, err := dbs.setGrants(ctx, "system", site, d.User, mapGrants(a.Grants)); err != nil {
			progress(37, fmt.Sprintf("warning: the grants of %s cannot be set as they were (%v)", d.User, err))
		}
	}
	for _, a := range plan.accounts {
		if _, err := dbs.createUser(ctx, "system", site, newDBUser{Name: a.Name, Password: a.Password, Grants: mapGrants(a.Grants)}); err != nil {
			progress(38, fmt.Sprintf("warning: the database user %s cannot be made here (%v)", a.Name, err))
			continue
		}
		progress(38, "Database user "+a.Name+" created, with its password")
	}
	// What the application's configuration names is kept unless it could
	// not be: then a WordPress site's is rewritten, and any other is told.
	credentials := snap.Manifest == nil || plan.account == nil || plan.account.Name != d.User ||
		plan.account.Password != p.sec.dbPassword || len(rename) > 0
	if credentials && d.Enabled && !isWordPress(site.Type) {
		progress(39, fmt.Sprintf("note: the application's database settings must name database %s, user %s and the password the site's Databases tab sets: the backup's could not be kept", d.Name, d.User))
	}
	repo, password, err := s.backupRepository()
	if err != nil {
		return kept(err)
	}
	if repo == "" || password == "" {
		return kept(errBackupsOff)
	}
	progress(45, fmt.Sprintf("Restoring %s from snapshot %s, made on %s", site.Domain, shortID(snap.ID), snap.Hostname))
	var done map[string]string
	if err := agentrpc.Into(context.WithoutCancel(ctx), s.Agent, agentrpc.RestoreSnapshot, rpc.RestoreParams{
		Env: s.backupEnv(), Site: site, Repository: repo, Password: password, SnapshotID: snap.ID, Mode: "full",
		Foreign: &rpc.ForeignRestore{Hostname: snap.Hostname, Databases: rename, DBPassword: p.sec.dbPassword,
			ConnectorToken: p.sec.connectorToken, Credentials: credentials},
	}, &done); err != nil {
		return kept(err)
	}
	s.purgeQuietly(ctx, site)
	if names := strings.Trim(done["skipped"]+","+strings.Join(skipped, ","), ","); names != "" {
		progress(80, "warning: not restored here: "+names)
	}
	s.audit("system", "backup.imported", site.Domain, fmt.Sprintf("restored from snapshot %s made on %s", shortID(snap.ID), snap.Hostname))

	// A backup of its own, on this server: the restored site is protected
	// from now on, and retention here only ever touches this one's.
	progress(85, "Backing up "+site.Domain+" on this server")
	b, err := s.snapshot(ctx, site, repo, password, "full")
	if err != nil {
		return fmt.Errorf("%s was restored, but its first backup here failed: %w", site.Domain, err)
	}
	return s.verifyBackup(context.WithoutCancel(ctx), site, b, password, func(pct int, msg string) { progress(max(pct, 90), msg) }, 90)
}

// addDatabaseNow makes a database of site on this server's own MariaDB, as
// POST /api/sites/{id}/databases does, in the caller's job (§6.5).
func (s *Server) addDatabaseNow(ctx context.Context, site state.Site, name string) error {
	if err := checkName(site, name); err != nil {
		return err
	}
	if _, err := s.Store.AddSiteDatabaseOn(site.ID, name, 0); err != nil {
		if errors.Is(err, state.ErrDBNameTaken) {
			return errors.New("a database of that name exists on this server")
		}
		return err
	}
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.CreateDatabase, rpc.SiteDatabaseParams{Site: site, Name: name}, nil); err != nil {
		s.logIfErr(s.Store.RemoveSiteDatabase(site.ID, name), "RemoveSiteDatabase")
		return err
	}
	s.Store.AnnounceDatabases(site.ID, name)
	s.audit("system", "database.create", site.Domain, name)
	return nil
}
