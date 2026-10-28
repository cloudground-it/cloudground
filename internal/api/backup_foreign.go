package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
	site, primary, err := foreignNewSite(snap, req)
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
		return s.importSite(ctx, p, rec, snap, primary, progress)
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

// foreignNewSite is the site a backup of another server describes, as
// POST /api/sites would take it, and the name of the backup's primary
// database ("" for a backup from before they were named, which the agent
// restores into the site's primary itself).
func foreignNewSite(snap rpc.ForeignSnapshotResult, req foreignRestore) (newSite, string, error) {
	m := snap.Manifest
	site := newSite{Domain: snap.Domain, Type: req.Type, PHPVersion: req.PHPVersion}
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
	}
	if site.Type == "" {
		return newSite{}, "", refuse(probInvalid, "this backup has no description of the site (it was made before 1.0): give its type, and its PHP version for a PHP site")
	}
	if state.SiteType(site.Type) == state.SiteProxy {
		return newSite{}, "", refuse(probInvalid, "a reverse-proxy site has no backup to restore")
	}
	primary := backupPrimary(snap)
	if len(snap.Databases) > 0 && !startsWithDB(state.SiteType(site.Type)) {
		yes := true
		site.CreateDatabase = &yes
	}
	if isWordPress(state.SiteType(site.Type)) {
		// Placeholders: the restored database brings the site's own accounts.
		site.Title = snap.Domain
		site.AdminEmail = "admin@" + snap.Domain
		site.AdminUser = "cloudground-restore"
		site.AdminPassword = randomToken(24)
	}
	return site, primary, nil
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
// backup's other databases made, the restore of the backup whole, then a
// backup of the site on this server, restore-tested.
func (s *Server) importSite(ctx context.Context, p preparedSite, rec state.Site, snap rpc.ForeignSnapshotResult, primary string, progress func(int, string)) error {
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
	// The backup's databases: its primary goes into the new site's primary;
	// any other is made under its own name when that name is free here.
	rename := map[string]string{}
	if primary != "" && site.Config.Database.Enabled && !site.Config.Database.External {
		rename[primary] = site.Config.Database.Name
	}
	var skipped []string
	for _, name := range snap.Databases {
		if name == "" || name == primary {
			continue
		}
		if err := s.addDatabaseNow(ctx, site, name); err != nil {
			skipped = append(skipped, name)
			progress(35, fmt.Sprintf("warning: the database %s cannot be made here (%v); the restore leaves it out", name, err))
			continue
		}
		progress(35, "Database "+name+" created")
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
		Foreign: &rpc.ForeignRestore{Hostname: snap.Hostname, Databases: rename, DBPassword: p.sec.dbPassword, ConnectorToken: p.sec.connectorToken},
	}, &done); err != nil {
		return kept(err)
	}
	s.purgeQuietly(ctx, site)
	if names := strings.Trim(done["skipped"]+","+strings.Join(skipped, ","), ","); names != "" {
		progress(80, "warning: not restored, no such database here: "+names)
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
