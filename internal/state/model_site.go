package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// SiteType names the recipe a site is provisioned with.
type SiteType string

// The site types.
const (
	SiteWordPress   SiteType = "wordpress"
	SiteWooCommerce SiteType = "woocommerce"
	SiteStatic      SiteType = "static"
	SitePHP         SiteType = "php"
	SiteLaravel     SiteType = "laravel"
	SiteProxy       SiteType = "proxy"
)

// Profile is a Velocity Engine policy: how a site is cached and tuned.
type Profile string

// The profiles.
const (
	ProfileBalanced Profile = "balanced"
	ProfileCommerce Profile = "commerce"
	ProfileMaximum  Profile = "maximum"
)

// Engine is the web-serving stack a site is rendered for.
type Engine string

// EngineNginx, nginx in front of PHP-FPM, is the only engine and the default.
const EngineNginx Engine = "nginx"

// SiteStatus is where a site is in its life.
type SiteStatus string

// The site statuses.
const (
	SiteProvisioning SiteStatus = "provisioning"
	SiteActive       SiteStatus = "active"
	SiteError        SiteStatus = "error"
	SiteDeleting     SiteStatus = "deleting"
)

// SiteConfig is a site's declarative description, kept as JSON in the
// sites row (its config column).
type SiteConfig struct {
	CacheEnabled bool `json:"cache_enabled"`
	// CacheTTLSec is the page cache lifetime; 0 takes the profile's.
	CacheTTLSec int `json:"cache_ttl_sec"`
	// ObjectCache gives the site its own Redis namespace.
	ObjectCache bool           `json:"object_cache"`
	Database    DatabaseConfig `json:"database"`
	Resources   Resources      `json:"resources"`
	Backups     BackupPolicy   `json:"backups"`
	PHP         PHPSettings    `json:"php"`
	SFTPEnabled bool           `json:"sftp_enabled"`
	// SFTPKeysOnly: the SFTP login takes keys only, its password locked
	// (ops/sftp SPEC §3).
	SFTPKeysOnly bool `json:"sftp_keys_only,omitempty"`
	// ShellEnabled lets the site's SSH keys of kind "shell" open a shell
	// in the site (R-4.5, ops/sftp SPEC §5). Only an administrator sets it.
	ShellEnabled bool `json:"shell_enabled"`
	// ProxyUpstream is where a proxy site forwards to.
	ProxyUpstream string `json:"proxy_upstream,omitempty"`
	// PublicRoot is the document root inside a release ("public" for
	// Laravel); empty is the release itself.
	PublicRoot string `json:"public_root,omitempty"`
	// HSTS asks for Strict-Transport-Security, sent only once the site
	// has a real certificate: on the self-signed fallback it would lock
	// browsers out.
	HSTS bool `json:"hsts,omitempty"`
	// StagingAuthHash is, on a staging site, the bcrypt hash of the
	// password nginx asks for: a staging copy is never public.
	StagingAuthHash string `json:"staging_auth_hash,omitempty"`
	// WildcardCert extends the certificate to *.<domain>, issued over
	// Cloudflare DNS.
	WildcardCert bool `json:"wildcard_cert,omitempty"`
	// Routing is the administrator's own block of nginx locations
	// (internal/engine SPEC §7); empty is the site type's default.
	Routing string `json:"routing,omitempty"`
	// BypassCookies are cookie names that skip the page cache beyond the
	// site type's own set (internal/velocity SPEC §1.4): an application's
	// session cookie the type does not know.
	BypassCookies []string `json:"bypass_cookies,omitempty"`
	// ProtectedPaths are paths nginx asks a username and password for
	// (#26), each with its own credentials.
	ProtectedPaths []ProtectedPath `json:"protected_paths,omitempty"`
	// Uptime is the panel's periodic check of the site from its public
	// address (#32).
	Uptime UptimeCheck `json:"uptime,omitzero"`
	// Blocking is who the site refuses (#25): addresses, user agents,
	// countries (internal/engine SPEC §4.7).
	Blocking SiteBlocking `json:"blocking,omitzero"`
	// WPTwoFactor is two-factor sign-in on a WordPress site (#74,
	// connector/SPEC.md §2.11): TwoFactorOptional or TwoFactorRequired;
	// "" is off.
	WPTwoFactor string `json:"wp_two_factor,omitempty"`
}

// SiteBlocking is the visitors a site answers 403: addresses or networks,
// user agents containing one of UserAgents (case-insensitive), and those
// whose address is in one of Countries (ISO 3166-1 alpha-2), or, with
// AllowCountriesOnly, in none of them.
type SiteBlocking struct {
	Addresses          []string `json:"addresses,omitempty"`
	UserAgents         []string `json:"user_agents,omitempty"`
	Countries          []string `json:"countries,omitempty"`
	AllowCountriesOnly bool     `json:"allow_countries_only,omitempty"`
}

// Any reports whether b refuses anyone.
func (b SiteBlocking) Any() bool {
	return len(b.Addresses)+len(b.UserAgents)+len(b.Countries) > 0
}

// The modes of two-factor sign-in on a WordPress site. Optional lets each
// account set it up from its profile; required also makes it compulsory
// for administrators and shop managers.
const (
	TwoFactorOptional = "optional"
	TwoFactorRequired = "required"
)

// ProtectedPath is one path behind HTTP basic auth: the path and what is
// below it (site.ValidProtectedPath), the one username it takes, and the
// bcrypt hash of its password, which the API never shows (Public).
type ProtectedPath struct {
	Path         string `json:"path"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash,omitempty"`
}

// UptimeCheck turns the uptime check of a site on, and names the path it
// requests ("" is "/").
type UptimeCheck struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path,omitempty"`
}

// DatabaseConfig is the database a site uses. It may be external (managed
// or on another host), so a site can outgrow the server without being
// created again.
type DatabaseConfig struct {
	Enabled  bool   `json:"enabled"`
	External bool   `json:"external"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Name     string `json:"name"`
	User     string `json:"user"`
	TLS      bool   `json:"tls"`
}

// Resources size and bound a site. MemoryHighMB feeds the estimate of
// PHP-FPM's pm.max_children (a heuristic, not a limit), and PHPWorkers,
// when set, is that worker count. MemoryMaxMB, CPUQuotaPct and TasksMax are
// enforced by the kernel on the site's slice (cg-site-<id>.slice), which
// holds every process of the site: its PHP master, cron, wp-cli and shell
// (ops/sites SPEC §3). 0 is the measured default of each.
type Resources struct {
	MemoryHighMB int `json:"memory_high_mb"`
	// PHPWorkers of 0 sizes the pool automatically.
	PHPWorkers int `json:"php_workers"`
	// MemoryMaxMB is the slice's MemoryMax; 0 is half the server's memory,
	// and never less than DefaultMemoryFloorMB.
	MemoryMaxMB int `json:"memory_max_mb,omitempty"`
	// CPUQuotaPct is the slice's CPUQuota, 100 per CPU; 0 is no quota.
	CPUQuotaPct int `json:"cpu_quota_pct,omitempty"`
	// TasksMax is the slice's TasksMax; 0 is DefaultTasksMax.
	TasksMax int `json:"tasks_max,omitempty"`
	// Alerts are the usage thresholds a site may stay above before an
	// alert (api SPEC §11.5).
	Alerts UsageAlerts `json:"alerts,omitzero"`
}

// The measured defaults of a site's limits (bench/site-resources.md): the
// WooCommerce bench site peaked at 561 MB and 8 tasks with its whole suite,
// cron and a cache warm.
const (
	DefaultMemoryFloorMB = 1024
	DefaultTasksMax      = 512
)

// UsageAlerts are a site's usage thresholds; 0 is the default of each.
type UsageAlerts struct {
	// Off turns the site's usage alerts off.
	Off bool `json:"off,omitempty"`
	// CPUPct is the share of the site's CPU allowance (its quota, or every
	// CPU of the server) it may stay above; default DefaultAlertPct.
	CPUPct int `json:"cpu_pct,omitempty"`
	// MemoryPct is the share of its MemoryMax; default DefaultAlertPct.
	MemoryPct int `json:"memory_pct,omitempty"`
	// DiskMB is the disk use of its directories; 0 is no disk alert.
	DiskMB int `json:"disk_mb,omitempty"`
}

// DefaultAlertPct is the CPU and memory threshold of a site that sets none.
const DefaultAlertPct = 90

// CPU and memory thresholds with their defaults applied.
func (a UsageAlerts) CPU() int    { return cmp.Or(a.CPUPct, DefaultAlertPct) }
func (a UsageAlerts) Memory() int { return cmp.Or(a.MemoryPct, DefaultAlertPct) }

// BackupPolicy is a site's restic schedule. A "repository" key that old
// releases wrote and nothing read is ignored when a config is decoded.
type BackupPolicy struct {
	Enabled bool `json:"enabled"`
	// Schedule is hourly, daily or weekly.
	Schedule      string `json:"schedule"`
	RetentionDays int    `json:"retention_days"`
	// Hour, 0–23 in the server's time zone, is when a daily or weekly
	// backup runs; nil runs it a day or a week after the last one,
	// whatever the time. An hourly schedule ignores it.
	Hour *int `json:"hour,omitempty"`
}

// PHPSettings are the php.ini values a site may override; 0 leaves the
// pool renderer's own choice.
type PHPSettings struct {
	MemoryLimitMB       int `json:"memory_limit_mb"`
	UploadMaxMB         int `json:"upload_max_mb"`
	MaxExecutionSeconds int `json:"max_execution_seconds"`
}

// RequestTimeoutSeconds bounds one PHP request in wall-clock time, for both
// FPM (request_terminate_timeout) and nginx (fastcgi_read_timeout): ten
// seconds past MaxExecutionSeconds, so neither cuts a raised limit short,
// and never under 120.
func (p PHPSettings) RequestTimeoutSeconds() int {
	return max(p.MaxExecutionSeconds+10, 120)
}

// Site is one managed website; a staging copy names its production site in
// StagingOf.
type Site struct {
	ID         int64      `json:"id"`
	Domain     string     `json:"domain"`
	Aliases    []string   `json:"aliases"`
	Type       SiteType   `json:"type"`
	Profile    Profile    `json:"profile"`
	Engine     Engine     `json:"engine"`
	PHPVersion string     `json:"php_version"`
	SystemUser string     `json:"system_user"`
	RootPath   string     `json:"root_path"`
	Status     SiteStatus `json:"status"`
	StagingOf  int64      `json:"staging_of,omitempty"`
	Config     SiteConfig `json:"config"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	// The generations (SPEC.md invariant 5), all three the store's:
	// DesiredGeneration counts changes to the desired configuration
	// (MutateSite), AppliedGeneration is the newest the agent is known to
	// have rendered and LastApplyError why the last attempt failed
	// (MarkApplied).
	DesiredGeneration int64  `json:"desired_generation"`
	AppliedGeneration int64  `json:"applied_generation"`
	LastApplyError    string `json:"last_apply_error"`
}

// InternalKey is what the panel and the agent send in the
// X-CloudGround-Internal header to reach a password-protected staging site
// without its password (Performance Guard, cache flushes, the warmer).
// It derives from the staging password's hash, which only the panel, the
// agent and the root-only vhost hold; a site without that protection has
// none ("").
func (s Site) InternalKey() string {
	if s.Config.StagingAuthHash == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cloudground-staging-internal\x00" + s.Config.StagingAuthHash))
	// Half the digest: a longer key overflows nginx's default
	// map_hash_bucket_size of 64 and nginx -t refuses the vhost (measured).
	return hex.EncodeToString(sum[:16])
}

// Public is the site as panel users see it through the API: without the
// staging password hash, from which InternalKey derives, nor the protected
// paths' password hashes.
func (s Site) Public() Site {
	s.Config.StagingAuthHash = ""
	if n := len(s.Config.ProtectedPaths); n > 0 {
		paths := make([]ProtectedPath, n)
		for i, p := range s.Config.ProtectedPaths {
			paths[i] = ProtectedPath{Path: p.Path, Username: p.Username}
		}
		s.Config.ProtectedPaths = paths
	}
	return s
}
