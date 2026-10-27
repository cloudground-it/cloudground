// Package sites creates, deletes and re-renders the agent's sites: the
// user, the tree, the database, the PHP master, the vhost and the slice
// they all run in. SPEC.md in this directory is the contract.
package sites

import (
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Paths is where a site's pieces live (SPEC §2).
type Paths struct {
	SitesRoot     string
	CacheRoot     string
	LogRoot       string
	ACMEWebroot   string
	FallbackCert  string
	FallbackKey   string
	CertLiveDir   string
	CustomCertDir string
	NginxSites    string
	NginxConfDir  string
	NginxAuthDir  string
	PHPBinDir     string
	PHPSocketDir  string
	PHPMasterDir  string
	SystemdDir    string
	// Quarantine holds the files the malware scan moved out of the sites,
	// one directory per site id (ops/malware); "" keeps none.
	Quarantine string
	// AuthLogSocket is where WordPress vhosts log failed sign-ins for the
	// block list (ops/blocklist SPEC §1.2); "" logs none.
	AuthLogSocket string
	// AppEnvDir holds the managed apps' environment files (SPEC §11.2);
	// "" is runtimes.EnvDir.
	AppEnvDir string
}

// Databases is the part of ops/db a site's lifecycle uses: the primary
// database and its account made with the site, every database and account
// recorded for it dropped with it (ops/db SPEC §7).
type Databases interface {
	ProvisionSite(ctx context.Context, s state.Site, password string) error
	DropSite(ctx context.Context, siteID int64) error
	Exists(ctx context.Context, name string) (bool, error)
}

// SFTP turns a site user's SFTP access on or off (ops/sftp).
type SFTP interface {
	SetAccess(ctx context.Context, user string, allow bool) error
	// RemoveShell ends the site's shells and removes its shell login
	// (ops/sftp SPEC §5.5), before the site user goes.
	RemoveShell(ctx context.Context, s state.Site) error
}

// Certs removes what ops/certs keeps for a site.
type Certs interface {
	RemoveSite(ctx context.Context, domain string) error
}

// Cron installs and removes a site's crontab (ops/cron).
type Cron interface {
	Apply(ctx context.Context, p rpc.CronParams) (rpc.CronResult, error)
	RemoveSite(ctx context.Context, systemUser string) error
}

// WordPress installs WordPress, WooCommerce and the object cache (ops/wp).
type WordPress interface {
	InstallWordPress(ctx context.Context, r *sitefs.Root, s state.Site, release string, in wp.Install) error
	InstallObjectCache(ctx context.Context, s state.Site) error
	InstallWooCommerce(ctx context.Context, s state.Site) error
}

// Ops holds what the handlers use.
type Ops struct {
	Paths    Paths
	Renderer engine.Renderer
	Accounts host.Accounts
	Systemd  host.Systemd
	Nginx    host.Nginx
	Exec     host.Exec
	// Open opens a site's tree; what it creates belongs to the site user.
	Open      func(state.Site) (*sitefs.Root, error)
	Owner     func(systemUser string) (sitefs.Owner, error)
	DB        Databases
	SFTP      SFTP
	Certs     Certs
	Cron      Cron
	WordPress WordPress
	// Relink rewrites a tree's absolute layout links (before R-2.3) as
	// relative ones; nil rewrites nothing.
	Relink func(r *sitefs.Root, siteRoot string) error
	// NginxMu covers every change to nginx's files and the reload after
	// it; the agent shares it with the panel's own vhost.
	NginxMu *sync.Mutex
	// MemTotalMB is the machine's memory; nil reads /proc/meminfo.
	MemTotalMB func() int64
	// KillUser kills every process of a user and waits for them to exit;
	// nil is the real one (/proc).
	KillUser func(ctx context.Context, systemUser string) error
	Log      *slog.Logger
	// What a routing block must not reach: the system services' TCP ports,
	// this machine's addresses, a resolver. nil reads /proc/net, the
	// interfaces and DNS (reach.go).
	SystemPorts func() []int
	LocalAddrs  func() []netip.Addr
	Resolve     func(host string) ([]netip.Addr, error)
	// CountryPrefixes resolves country codes to their addresses for a
	// site's blocking rules (ops/blocklist SPEC §5); nil refuses them.
	CountryPrefixes func(ctx context.Context, codes []string) ([]netip.Prefix, error)
	// PrepareApp installs a managed app's dependencies in a release
	// (ops/runtimes SPEC §3); nil installs none.
	PrepareApp func(ctx context.Context, s state.Site, dir string) (string, error)
	// AppSocket is where the health probe dials an app; nil is its socket.
	AppSocket func(s state.Site) string
	// RemoveFunctions removes a site user's functions (ops/functions SPEC
	// §3); nil removes none.
	RemoveFunctions func(ctx context.Context, systemUser string) error

	buildOnce      sync.Once
	http3, http2On bool
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.CreateSite, o.Create)
	agentrpc.Handle(s, agentrpc.DeleteSite, o.Delete)
	agentrpc.Handle(s, agentrpc.ApplySiteConfig, o.Apply)
	agentrpc.Handle(s, agentrpc.SiteRouting, o.Routing)
	agentrpc.Handle(s, agentrpc.SetSiteRouting, o.SetRouting)
	agentrpc.Handle(s, agentrpc.CheckDocRoot, o.CheckDocRoot)
	agentrpc.Handle(s, agentrpc.PHPExtensions, o.PHPExtensions)
	agentrpc.Handle(s, agentrpc.PHPExtInstall, o.InstallPHPExtension)
	agentrpc.Handle(s, agentrpc.SetAppEnv, o.SetAppEnv)
	agentrpc.Handle(s, agentrpc.AppStatus, o.AppStatus)
	agentrpc.Handle(s, agentrpc.AppControl, o.AppControl)
	agentrpc.Handle(s, agentrpc.ReloadWebServer, func(ctx context.Context, _ agentrpc.None) (agentrpc.None, error) {
		return agentrpc.None{}, o.ReloadNginx(ctx)
	})
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// Names of a site's files (SPEC §2).

func (o *Ops) vhostPath(s state.Site) string {
	return filepath.Join(o.Paths.NginxSites, s.Domain+".conf")
}

func (o *Ops) cacheZonePath(s state.Site) string {
	return filepath.Join(o.Paths.NginxConfDir, "cloudground-cache-"+itoa(s.ID)+".conf")
}

// blockingPath is the site's blocking rules (internal/engine SPEC §4.7).
func (o *Ops) blockingPath(s state.Site) string {
	return filepath.Join(o.Paths.NginxConfDir, "cloudground-block-"+itoa(s.ID)+".conf")
}

func (o *Ops) poolPath(s state.Site) string {
	return filepath.Join(o.Paths.PHPMasterDir, s.SystemUser+".conf")
}

func (o *Ops) unitPath(s state.Site) string {
	return filepath.Join(o.Paths.SystemdDir, phpfpm.UnitName(s.SystemUser))
}

// dropinDir holds the unit's drop-ins; this package writes one file there.
func (o *Ops) dropinDir(s state.Site) string { return o.unitPath(s) + ".d" }

func (o *Ops) dropinPath(s state.Site) string {
	return filepath.Join(o.dropinDir(s), "cloudground-slice.conf")
}

func isPHP(s state.Site) bool {
	return s.Type != state.SiteStatic && s.Type != state.SiteProxy && !s.Type.IsApp()
}

func isWordPress(s state.Site) bool {
	return s.Type == state.SiteWordPress || s.Type == state.SiteWooCommerce
}

func localDB(s state.Site) bool { return s.Config.Database.Enabled && !s.Config.Database.External }
