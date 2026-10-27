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
	// SiteNode and SitePython run the operator's app as a unit of their
	// own behind nginx (#28, internal/runtimes SPEC).
	SiteNode   SiteType = "node"
	SitePython SiteType = "python"
)

// IsApp reports whether t runs a managed app (#28).
func (t SiteType) IsApp() bool { return t == SiteNode || t == SitePython }

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
	// DevModeUntil, when set, is the end of the site's development mode (#80):
	// until then the page cache is bypassed (internal/velocity SPEC §1.5).
	// The panel ends it when the time has come (internal/api SPEC §3.15).
	DevModeUntil time.Time `json:"dev_mode_until,omitzero"`
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
	// Maintenance is the site's maintenance page (#79, internal/engine SPEC
	// §4.9): its text is kept while the mode is off.
	Maintenance SiteMaintenance `json:"maintenance,omitzero"`
	// Redirects is the site's canonical host and redirect rules (#83,
	// internal/engine SPEC §4.10).
	Redirects SiteRedirects `json:"redirects,omitzero"`
	// Cloudflare is the site behind Cloudflare's proxy (#22, internal/api
	// SPEC §3.20): visitor addresses and the CDN purge.
	Cloudflare SiteCloudflare `json:"cloudflare,omitzero"`
	// App is a managed app's settings (#28, internal/runtimes SPEC §2).
	App SiteApp `json:"app,omitzero"`
	// Functions are the site's functions (#143, internal/functions SPEC §1).
	Functions []SiteFunction `json:"functions,omitempty"`
	// WPCleanup is a WordPress site's scheduled database cleanup (#78,
	// internal/api SPEC §3.17).
	WPCleanup WPCleanup `json:"wp_cleanup,omitzero"`
	// WPHardening is a WordPress site's hardening (#75), each item on
	// its own (internal/engine SPEC §4.8, connector/SPEC.md §2.12).
	WPHardening WPHardening `json:"wp_hardening,omitzero"`
	// WPUpdates is when a WordPress site's updates run by themselves
	// (#20, internal/api SPEC §6.2.3); the zero value is never.
	WPUpdates WPUpdatePolicy `json:"wp_updates,omitzero"`
	// Mail is how the site's mail leaves (#77, internal/api SPEC §14).
	Mail SiteMail `json:"mail,omitzero"`
}

// MailRelay is an SMTP relay mail is handed to (internal/api SPEC §14.1).
// Its password is a secret.
type MailRelay struct {
	Host string `json:"host,omitempty"`
	// Port 0 takes the security's (587, 465, 25).
	Port int `json:"port,omitempty"`
	// Security is starttls (also ""), tls or none.
	Security string `json:"security,omitempty"`
	Username string `json:"username,omitempty"`
	// From is the envelope sender, and the From of a message with none.
	From string `json:"from,omitempty"`
}

// Configured reports whether mail can be handed to the relay.
func (r MailRelay) Configured() bool { return r.Host != "" && r.From != "" }

// SiteMail is a site's mail. The zero value sends through the server's
// relay, from the relay's address.
type SiteMail struct {
	Off bool `json:"off,omitempty"`
	// From is the site's sender, the relay's when empty.
	From string `json:"from,omitempty"`
	// DKIMSelector names the DKIM record the checklist looks for.
	DKIMSelector string `json:"dkim_selector,omitempty"`
	// Relay is the site's own relay; nil takes the server's.
	Relay *MailRelay `json:"relay,omitempty"`
}

// WPUpdatePolicy is a WordPress site's scheduled updates. Each runs as the
// panel's own update does: a tested snapshot first, a health check after,
// and a rollback when that fails.
type WPUpdatePolicy struct {
	Enabled bool `json:"enabled,omitempty"`
	// Schedule is daily or weekly; empty reads as weekly.
	Schedule string `json:"schedule,omitempty"`
	// Hour, 0–23 in the server's time zone; nil runs it a day or a week
	// after the last one, whatever the time.
	Hour *int `json:"hour,omitempty"`
	// What is core, plugins, themes or all; empty reads as all.
	What string `json:"what,omitempty"`
}

// Redirect rule kinds and statuses (#83).
const (
	RedirectExact  = "exact"
	RedirectPrefix = "prefix"
)

// RedirectRule sends a visitor from a path of the site to another place.
// Exact matches the path only; prefix matches the path and everything under
// it, and the rest is carried over (/blog/a to /news/a). The query string is
// kept unless To has one of its own (an exact rule). Status is 301 or 302.
type RedirectRule struct {
	Match  string `json:"match"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status"`
}

// SiteRedirects is the site's canonical host and redirect rules. Canonical
// is the site's domain or one of its aliases that every other name sends
// visitors to ("" keeps each name as it is).
type SiteRedirects struct {
	Canonical string         `json:"canonical,omitempty"`
	Rules     []RedirectRule `json:"rules,omitempty"`
}

// SiteMaintenance is a site's maintenance mode: while Enabled, visitors get
// a 503 page with Retry-After, except the addresses of Allow, this machine's
// own, and whoever holds the cookie that Key opens. Title and Message
// are the page's text; empty ones take the default.
type SiteMaintenance struct {
	Enabled bool   `json:"enabled,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	// RetryAfterSec is the Retry-After the page sends; 0 is an hour.
	RetryAfterSec int `json:"retry_after_sec,omitempty"`
	// Allow are the addresses or networks that see the site.
	Allow []string `json:"allow,omitempty"`
	// Key opens the site to a browser (internal/engine SPEC §4.9): 32 hex
	// characters, made when the mode is first turned on. Never in the API's
	// view of the site (Public).
	Key string `json:"key,omitempty"`
}

// WPCleanup is a WordPress site's scheduled database cleanup: once a week,
// at the site's own slot, its expired transients are deleted and its
// revisions trimmed to KeepRevisions per post. Off by default.
type WPCleanup struct {
	Enabled bool `json:"enabled,omitempty"`
	// KeepRevisions is how many revisions of each post stay; 0 is the
	// default (internal/site, DefaultKeepRevisions).
	KeepRevisions int `json:"keep_revisions,omitempty"`
}

// WPHardening is the WordPress hardening a site has turned on. The zero
// value is none.
type WPHardening struct {
	// XMLRPCOff answers xmlrpc.php with 403, except for the addresses and
	// networks of XMLRPCAllow and Jetpack's own signed requests.
	XMLRPCOff   bool     `json:"xmlrpc_off,omitempty"`
	XMLRPCAllow []string `json:"xmlrpc_allow,omitempty"`
	// FileEditOff takes the theme and plugin file editors away.
	FileEditOff bool `json:"file_edit_off,omitempty"`
	// HideVersion keeps WordPress's version out of pages, feeds and asset
	// URLs.
	HideVersion bool `json:"hide_version,omitempty"`
	// BlockReadme answers 404 for readme.html, license.txt and the readme,
	// changelog and licence files of plugins and themes.
	BlockReadme bool `json:"block_readme,omitempty"`
	// LoginPath, when set, is the sign-in page instead of wp-login.php
	// (a path segment such as "my-door"); wp-login.php then answers 404.
	LoginPath string `json:"login_path,omitempty"`
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
	s.Config.Maintenance.Key = ""
	if n := len(s.Config.ProtectedPaths); n > 0 {
		paths := make([]ProtectedPath, n)
		for i, p := range s.Config.ProtectedPaths {
			paths[i] = ProtectedPath{Path: p.Path, Username: p.Username}
		}
		s.Config.ProtectedPaths = paths
	}
	return s
}

// SiteCloudflare is a site behind Cloudflare (#22): RealIP takes the
// visitor's address from CF-Connecting-IP, from Cloudflare's ranges only
// (internal/engine SPEC §4.11); ZoneID, when set, has purges empty
// Cloudflare's cache of the site too.
type SiteCloudflare struct {
	RealIP bool   `json:"real_ip,omitempty"`
	ZoneID string `json:"zone_id,omitempty"`
}

// SiteApp is a node or python site's app (#28, internal/runtimes SPEC §2).
// The secret variables' values are sealed secrets (SiteAppEnvSecret); only
// their names are here.
type SiteApp struct {
	Version    string            `json:"version,omitempty"`
	Start      string            `json:"start,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Secrets    []string          `json:"secrets,omitempty"`
	Port       int               `json:"port,omitempty"`
	HealthPath string            `json:"health_path,omitempty"`
}

// SiteFunction is one of a site's functions (#143, internal/functions SPEC
// §1); its code lives in the site's tree, its secrets' values are sealed.
type SiteFunction struct {
	Name       string            `json:"name"`
	Runtime    string            `json:"runtime"`
	Version    string            `json:"version"`
	MemoryMB   int               `json:"memory_mb"`
	TimeoutSec int               `json:"timeout_sec"`
	Schedule   string            `json:"schedule,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Secrets    []string          `json:"secrets,omitempty"`
	Release    string            `json:"release,omitempty"`
}
