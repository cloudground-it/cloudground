// Package agent is CloudGround's root daemon. It serves the method table of
// internal/agentrpc on a Unix socket and does the system work those methods
// name. Every call carries the site as the API stores it, and the agent
// checks it again before acting; the one state it keeps is its index of the
// sites it provisioned (siteindex.go), which every site of a request must
// match.
//
// The work itself lives in the ops/* packages, one per domain, over the
// host adapters (internal/agent/host) and confined site trees
// (internal/agent/sitefs). This package builds them and wires them to the
// server, with the per-site locks they share.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/backup"
	"github.com/cloudground-it/cloudground/internal/agent/ops/blocklist"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cache"
	"github.com/cloudground-it/cloudground/internal/agent/ops/certs"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cron"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/ops/dbstudio"
	"github.com/cloudground-it/cloudground/internal/agent/ops/files"
	"github.com/cloudground-it/cloudground/internal/agent/ops/panel"
	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sftp"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sites"
	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/agent/ops/system"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// Paths is the machine layout the agent works in. DefaultPaths is the one
// the installer makes; tests move every entry into a temporary directory.
type Paths struct {
	SitesRoot     string // the sites' trees, one per domain
	CacheRoot     string // the page caches
	LogRoot       string // each site's nginx and PHP logs
	ACMEWebroot   string // HTTP-01 challenges, served by nginx
	FallbackCert  string // the self-signed certificate a vhost has before its own
	FallbackKey   string
	CertLiveDir   string // certbot's live/ directory
	NginxSites    string // one vhost per site
	NginxConfDir  string // http-level files: page-cache zones, globals
	PHPBinDir     string // php-fpm<version>: a version is installed when its binary is here
	PHPSocketDir  string // each site's FPM socket directory
	WorkDir       string // root's scratch space (dumps, restore tests, staged updates)
	MySQLSocket   string // MariaDB's socket (root's admin connection, SQL consoles)
	BinDir        string // where self-update installs the binaries
	PHPMasterDir  string // each site's FPM master configuration
	SystemdDir    string // each site's FPM unit and its drop-ins
	NginxAuthDir  string // staging sites' password files
	CustomCertDir string // certificates the operator brought, per domain
	PanelCert     string // what nginx serves for the panel's domain
	PanelKey      string
	PanelAccess   string // the operator's access rules the panel vhost includes
	APISocket     string // the API, behind nginx
	CrontabDir    string // cron's per-user spool (sites had a crontab there before)
	CronDir       string // cron.d: one root-owned file per site
	CronSpecDir   string // each site's jobs for the cron runner, root's
	SiteIndex     string // the agent's record of its sites (id -> domain); "" keeps none
	DBRegistry    string // its record of each site's databases and accounts, beside SiteIndex; "" is databases.json in WorkDir
	StateDB       string // the API's state database, which a self-update snapshots first; "" snapshots none
	AuthLogSocket string // where WordPress vhosts log failed sign-ins for the block list; "" logs none
}

// DefaultPaths is the installer's layout.
func DefaultPaths() Paths {
	return Paths{
		SitesRoot:    "/srv/sites",
		CacheRoot:    "/var/cache/cloudground",
		LogRoot:      "/var/log/cloudground",
		ACMEWebroot:  "/var/www/cloudground-acme", // nginx must reach it; /var/lib/cloudground is closed to it
		FallbackCert: "/etc/cloudground/certs/fallback.pem",
		FallbackKey:  "/etc/cloudground/certs/fallback.key",
		CertLiveDir:  "/etc/letsencrypt/live",
		NginxSites:   "/etc/nginx/sites-enabled",
		NginxConfDir: "/etc/nginx/conf.d",
		PHPBinDir:    "/usr/sbin",
		PHPSocketDir: "/run/cloudground/php",
		// Not under /var/lib/cloudground, which the API's user owns: it could
		// swap a directory there for a link and steer root's writes.
		WorkDir:       "/var/lib/cloudground-agent/work",
		MySQLSocket:   "/run/mysqld/mysqld.sock",
		BinDir:        "/usr/local/bin",
		PHPMasterDir:  "/etc/cloudground-php",
		SystemdDir:    "/etc/systemd/system",
		NginxAuthDir:  "/etc/nginx/cloudground-auth",
		CustomCertDir: "/etc/cloudground/certs/custom",
		PanelCert:     "/etc/cloudground/certs/panel.pem",
		PanelKey:      "/etc/cloudground/certs/panel.key",
		PanelAccess:   "/etc/cloudground/panel-access.conf",
		APISocket:     "/run/cloudground/api.sock",
		CrontabDir:    cron.DefaultCrontabDir,
		CronDir:       cron.DefaultCronDir,
		CronSpecDir:   cron.DefaultSpecDir,
		SiteIndex:     "/var/lib/cloudground-agent/sites.json",
		DBRegistry:    "/var/lib/cloudground-agent/databases.json",
		StateDB:       "/var/lib/cloudground/state.db",
		AuthLogSocket: blocklist.DefaultSocket,
	}
}

// Agent holds the adapters, the paths and the locks every domain shares.
// A nil adapter means the real one, built on first use.
type Agent struct {
	Paths    Paths
	Systemd  host.Systemd
	Accounts host.Accounts
	Exec     host.Exec
	Firewall host.Firewall
	ACME     host.ACME
	Nginx    host.Nginx
	WPCLI    host.WPCLI
	MariaDB  host.MariaDB
	Restic   host.Restic
	Renderer engine.Renderer
	Log      *slog.Logger
	// LookupOwner resolves a site user to the owner of its files; nil is
	// the user database. Tests, whose site users do not exist, set it.
	LookupOwner func(systemUser string) (sitefs.Owner, error)
	// AllowPrivateRepo lets a backup repository's s3:/rest: endpoint be a
	// private or local address (CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT).
	AllowPrivateRepo bool
	// LookupGroup resolves a gid to its group's name; nil is the group
	// database (the site index's rebuild).
	LookupGroup func(gid int) (string, error)
	// UpdateClient fetches self-update releases; nil refuses redirects off
	// https.
	UpdateClient *http.Client

	// siteLocks orders whole-site operations per domain: a render racing
	// a deletion would bring the deleted site's vhost back.
	siteLocks sync.Map // domain -> *sync.Mutex
	// nginxMu orders every change to nginx's files with the reload after
	// it: nginx reads its includes after the signal, so a file removed in
	// between made a reload fail and left the change unapplied.
	nginxMu sync.Mutex
	// certMu: certbot allows one process at a time on its state.
	certMu sync.Mutex

	// Each domain's handlers, built once (they keep locks and caches) by
	// the <domain>Handlers methods of the ops_*.go files.
	sitesOnce, certsOnce, panelOnce, systemOnce, filesOnce, sftpOnce sync.Once
	wpOnce, cronOnce, dbOnce, releaseOnce, backupOnce, stagingOnce   sync.Once
	dbStudioOnce, blocklistOnce                                      sync.Once

	// index is the record of the sites this agent provisioned.
	index     siteIndex
	indexOnce sync.Once

	sitesOps   *sites.Ops
	certsOps   *certs.Ops
	panelOps   *panel.Ops
	systemOps  *system.Ops
	filesOps   *files.Ops
	sftpOps    *sftp.Ops
	wpOps      *wp.Ops
	cronOps    *cron.Ops
	dbOps      *db.Ops
	releaseOps *release.Ops
	backupOps  *backup.Ops
	stagingOps *staging.Ops
	dbStudio   *dbstudio.Ops
	blocklist  *blocklist.Ops
}

// New is the agent of a real machine.
func New(logger *slog.Logger) *Agent {
	if logger == nil {
		logger = slog.Default()
	}
	p := DefaultPaths()
	ex := host.OSExec{Timeout: time.Minute}
	sd := host.NewSystemd()
	return &Agent{
		Paths:    p,
		Systemd:  sd,
		Accounts: host.NewAccounts(ex),
		Exec:     ex,
		Firewall: host.NewFirewall(ex),
		// wp-cli runs up to an update's deadline; the request's context ends it.
		WPCLI:    host.NewWPCLI(host.OSExec{Timeout: 45 * time.Minute}),
		ACME:     host.NewCertbot(host.OSExec{Timeout: 20 * time.Minute}),
		MariaDB:  host.NewMariaDB(p.MySQLSocket, p.WorkDir, ex),
		Nginx:    host.NewNginx(ex, sd),
		Renderer: nginx.Renderer{SitesDir: p.NginxSites, ConfDir: p.NginxConfDir},
		Log:      logger,
	}
}

// NewServer is the agent's RPC server: agentrpc's table, every method
// wired, sites confined to Paths.SitesRoot and locked with this agent's
// locks. cfg brings the token, the allowed peers and the logger.
func (a *Agent) NewServer(cfg agentrpc.Config) *agentrpc.Server {
	cfg.SitesRoot = a.Paths.SitesRoot
	cfg.Locker = siteLocker{a}
	if a.Paths.SiteIndex != "" {
		a.indexOnce.Do(func() { a.index.a = a })
		cfg.Registry = &a.index
	}
	if cfg.Log == nil {
		cfg.Log = a.log()
	}
	s := agentrpc.NewServer(cfg)
	agentrpc.Handle(s, agentrpc.Ping, func(context.Context, agentrpc.None) (map[string]string, error) {
		return map[string]string{"version": version.Version}, nil
	})
	agentrpc.Handle(s, agentrpc.PurgeCache, cache.Purger{Root: a.Paths.CacheRoot}.Purge)
	agentrpc.Handle(s, agentrpc.Capabilities, a.capabilities)
	for _, d := range []interface{ Register(*agentrpc.Server) }{
		a.systemHandlers(), a.sitesHandlers(), a.certHandlers(), a.dbHandlers(),
		a.releaseHandlers(), a.stagingHandlers(), a.backupHandlers(), a.cronHandlers(),
		a.filesHandlers(), a.sftpHandlers(), a.wpHandlers(), a.panelHandlers(), a.migrationHandlers(),
		a.dbStudioHandlers(), a.blocklistHandlers(),
	} {
		d.Register(s)
	}
	return s
}

// LoadSiteIndex reads the site index, building it from the site trees the
// first time (an agent upgraded from one without it).
func (a *Agent) LoadSiteIndex() error {
	if a.Paths.SiteIndex == "" {
		return nil
	}
	a.index.mu.Lock()
	defer a.index.mu.Unlock()
	a.indexOnce.Do(func() { a.index.a = a })
	return a.index.load()
}

// siteLocker lends the server the agent's per-site locks.
type siteLocker struct{ a *Agent }

func (l siteLocker) LockSite(domain string) func()             { return l.a.lockSite(domain) }
func (l siteLocker) LockLiveSite(s state.Site) (func(), error) { return l.a.lockLiveSite(s) }

// lockSite takes domain's lock and returns its release.
func (a *Agent) lockSite(domain string) func() {
	m, _ := a.siteLocks.LoadOrStore(domain, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// lockLiveSite takes the site's lock and then requires its tree to exist:
// a handler that waited while the site was deleted must not write part of
// it back. The lock is not reentrant, so a handler that holds it calls
// only the unlocked inner functions of other domains. Order: site lock,
// then certMu, then nginxMu.
func (a *Agent) lockLiveSite(s state.Site) (func(), error) {
	unlock := a.lockSite(s.Domain)
	if _, err := os.Lstat(s.RootPath); err != nil {
		unlock()
		return nil, agentrpc.Conflict("site %s no longer exists", s.Domain)
	}
	return unlock, nil
}

// EnsureWorkDir creates the work directory (0700) and refuses to run when
// it, or a directory above it, is a link or writable by anyone but root:
// what the agent stages there is later read, run or restored as root.
func (a *Agent) EnsureWorkDir() error {
	dir := filepath.Clean(a.Paths.WorkDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for ; ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; the agent work directory must be a real path", dir)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			if st.Uid != 0 {
				return fmt.Errorf("%s is owned by uid %d; it must be owned by root", dir, st.Uid)
			}
			if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
				return fmt.Errorf("%s is writable by group or others", dir)
			}
		}
		if dir == "/" || dir == "." {
			return nil
		}
	}
}

// log is the agent's logger, the default one when none was set (tests).
func (a *Agent) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// exec is the agent's command runner; nil means real processes.
func (a *Agent) exec() host.Exec {
	if a.Exec != nil {
		return a.Exec
	}
	return host.OSExec{Timeout: time.Minute}
}

// nginxCtl is the agent's nginx adapter; nil means the real one.
func (a *Agent) nginxCtl() host.Nginx {
	if a.Nginx != nil {
		return a.Nginx
	}
	return host.NewNginx(host.OSExec{Timeout: time.Minute}, a.Systemd)
}

// wpcli is the agent's wp-cli runner; nil means the real one.
func (a *Agent) wpcli() host.WPCLI {
	if a.WPCLI != nil {
		return a.WPCLI
	}
	return host.NewWPCLI(a.exec())
}

// siteOwner resolves a site user to the owner of its files. An unknown
// user is an error: a lookup that failed quietly would leave root-owned
// files in a site's tree.
func (a *Agent) siteOwner(systemUser string) (sitefs.Owner, error) {
	if a.LookupOwner != nil {
		return a.LookupOwner(systemUser)
	}
	o, err := sitefs.LookupOwner(systemUser)
	if err != nil {
		return o, fmt.Errorf("site user %s: %w", systemUser, err)
	}
	return o, nil
}

// openSite opens a site's tree; what it creates belongs to the site user.
func (a *Agent) openSite(s state.Site) (*sitefs.Root, error) {
	o, err := a.siteOwner(s.SystemUser)
	if err != nil {
		return nil, err
	}
	return sitefs.Open(s.RootPath, o)
}

// records are the agent's own files a self-update snapshots: its site
// index and its database registry.
func (a *Agent) records() []string {
	var out []string
	if a.Paths.SiteIndex != "" {
		out = append(out, a.Paths.SiteIndex)
	}
	reg := a.Paths.DBRegistry
	if reg == "" && a.Paths.WorkDir != "" {
		reg = filepath.Join(a.Paths.WorkDir, "databases.json")
	}
	if reg != "" {
		out = append(out, reg)
	}
	return out
}
