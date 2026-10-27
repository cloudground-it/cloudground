export type SiteType = 'wordpress' | 'woocommerce' | 'static' | 'php' | 'laravel' | 'proxy' | 'node' | 'python'
export type Profile = 'balanced' | 'commerce' | 'maximum'
export type Role = 'admin' | 'operator' | 'readonly'

export interface Me {
  id: number
  email: string
  first_name: string
  last_name: string
  /** Chosen interface language, '' until the user picks one. */
  locale: '' | 'it' | 'en' | 'fr' | 'es' | 'de'
  /** The release whose changelog entry the account last saw (issue 106), '' for none. */
  whats_new_seen: string
  disabled: boolean
  role: Role
  site_ids?: number[]
  totp_enabled: boolean
  created_at: string
  /** Whether the account's password signs in (off only with a passkey, issue 156). */
  password_signin: boolean
  /** How many passkeys the account has. */
  passkeys: number
  /** How many of its recovery codes are unused. */
  recovery_codes_left: number
}

/** One of the account's passkeys. */
export interface Passkey {
  id: number
  name: string
  rp_id: string
  created_at: string
  last_used_at?: string
  backed_up: boolean
}

/**
 * Confirming it is you (api SPEC §2.4): the password while it signs in,
 * and one current second factor when the account has two-factor.
 */
export interface Proof {
  password?: string
  code?: string
  recovery_code?: string
  passkey?: Record<string, unknown>
}

/** A site's refused visitors (#25): addresses, user agents, countries. */
export interface SiteBlocking {
  addresses?: string[]
  user_agents?: string[]
  countries?: string[]
  allow_countries_only?: boolean
}

export interface Site {
  id: number
  domain: string
  aliases: string[] | null
  type: SiteType
  profile: Profile
  engine: string
  php_version: string
  system_user: string
  /** The site's tree on the server, where the agent keeps sites. */
  root_path: string
  status: 'provisioning' | 'active' | 'error' | 'deleting'
  staging_of?: number
  config: {
    cache_enabled: boolean
    cache_ttl_sec: number
    /** The end of development mode (#80); the page cache is bypassed until then. */
    dev_mode_until?: string
    object_cache: boolean
    sftp_enabled: boolean
    sftp_keys_only?: boolean
    shell_enabled?: boolean
    hsts?: boolean
    wildcard_cert?: boolean
    proxy_upstream?: string
    /** A Node.js or Python site's app (#28). */
    app?: SiteApp
    public_root?: string
    database: { enabled: boolean, name: string, user: string }
    backups: { enabled: boolean, schedule: string, retention_days: number, hour?: number | null }
    resources?: {
      memory_high_mb: number, php_workers: number, memory_max_mb?: number, cpu_quota_pct?: number, tasks_max?: number
      alerts?: { off?: boolean, cpu_pct?: number, memory_pct?: number, disk_mb?: number }
    }
    php: { memory_limit_mb: number, upload_max_mb: number, max_execution_seconds: number }
    /** Paths behind a username and password (the hash never leaves the panel). */
    protected_paths?: { path: string, username: string }[]
    /** Visitors the site answers 403 (#25). */
    blocking?: SiteBlocking
    /** The panel's periodic check from the public address (absent: off). */
    uptime?: { enabled: boolean, path?: string }
    wp_two_factor?: 'optional' | 'required'
    wp_hardening?: WPHardening
    /** Scheduled WordPress updates (#20); absent: never. */
    wp_updates?: { enabled?: boolean, schedule?: 'daily' | 'weekly', hour?: number | null, what?: 'core' | 'plugins' | 'themes' | 'all' }
  }
  created_at: string
}

export interface Task {
  id: number
  kind: string
  /** kind in words in the reader's language; absent on live events. */
  name?: string
  site_id: number
  status: 'pending' | 'running' | 'succeeded' | 'failed'
  progress: number
  message: string
  log?: string
  error?: string
  created_at: string
}

export interface Deployment {
  id: number
  site_id: number
  release_id: string
  status: 'pending' | 'guarding' | 'building' | 'created' | 'promoting' | 'verifying' | 'promoted' | 'superseded' | 'rolled_back' | 'blocked' | 'failed' | 'canceled'
  checksum: string
  guard_json?: string
  rollback_to?: string
  regressed?: boolean
  // what a person should know: drift[:release], unverified, regressed
  note?: string
  created_at: string
  promoted_at?: string
}

export interface Backup {
  id: number
  site_id: number
  snapshot_id: string
  repository: string
  size_bytes: number
  kind: string
  verify_status: 'pending' | 'passed' | 'failed'
  verified_at?: string
  restore_estimate_ms?: number
  created_at: string
  // the databases the snapshot holds; null for a backup from before they were recorded
  databases?: string[] | null
}

export interface SystemStatus {
  cpu_count: number
  load1: number
  mem_total_mb: number
  mem_available_mb: number
  disk_total_mb: number
  disk_free_mb: number
  agent_version: string
  uptime_seconds: number
  cpu_headroom_pct: number
  mem_headroom_pct: number
  disk_headroom_pct: number
}

export interface MetricSample {
  cpu_headroom_pct: number
  mem_headroom_pct: number
  disk_headroom_pct: number
  load1: number
  sampled_at: string
}

export interface DriftEvent {
  id: number
  path: string
  status: string
  detected_at: string
}

export interface AuditEvent {
  id: number
  actor: string
  action: string
  subject: string
  detail: string
  created_at: string
}

export interface GuardSample {
  path: string
  p50_ms: number
  p95_ms: number
  avg_queries: number
  peak_mem_bytes: number
  errors: number
  requests: number
}

export interface GuardComparison {
  path: string
  /** Median staging/production time ratio, with its 95% interval. */
  median_ratio: number
  ci_low: number
  ci_high: number
  median_delta_ms: number
  pairs: number
  server_timed: boolean
}

export interface GuardReport {
  verdict: 'pass' | 'warn' | 'block' | 'inconclusive' | 'unverified'
  reasons?: string[]
  baseline?: GuardSample[]
  candidate?: GuardSample[]
  paths?: GuardComparison[]
  measured_at: string
}

export interface CronJob {
  id: number
  site_id: number
  schedule: string
  command: string
  description: string
  enabled: boolean
  last_run?: string
  last_status: string
  created_at: string
}

export interface FileEntry {
  name: string
  is_dir: boolean
  size: number
  mode: string
  mod_time: string
}

export interface WPPlugin {
  name: string
  status: string
  version: string
  update: string
}

export interface ServiceInfo {
  name: string
  unit: string
  active: boolean
  enabled: boolean
  state: 'running' | 'stopped' | 'failed' | 'disabled'
  detail?: string
  memory_bytes?: number
  since?: string
  version?: string
}

/** What the server offers, as its agent finds it (GET /api/system/capabilities). */
export interface Capabilities {
  /** Installed PHP versions, newest first. */
  php_versions: string[]
  /** The version a new site gets when none is chosen. */
  default_php: string
  /** The services page's services. */
  services: { name: string, unit: string, listed: boolean, restartable: boolean }[]
  sites_root: string
  http3: boolean
  /** The panel's time zone, the one a backup's hour is in. */
  time_zone?: string
  /** The Node.js and Python versions installed (#28). */
  runtimes?: Runtimes
}

/** An installed runtime version: as sites name it, and as it reports itself. */
export interface RuntimeVersion { version: string, full: string }
export interface Runtimes {
  node: RuntimeVersion[]
  python: RuntimeVersion[]
  /** The distro's own Python 3.N, installed or not, newest first. */
  python_distro?: string[]
}

/** A managed app's settings (GET /api/sites/{id}/app). */
export interface SiteApp {
  version: string
  start: string
  env?: Record<string, string>
  secrets?: string[]
  port?: number
  health_path?: string
}

export interface PanelUser {
  id: number
  email: string
  first_name: string
  last_name: string
  disabled: boolean
  role: Role
  site_ids?: number[]
  totp_enabled: boolean
  created_at: string
  passkeys: number
}

export interface UpdateStatus {
  current: string
  latest?: string
  update_available: boolean
  notes_url?: string
  checked_at?: string
  check_enabled: boolean
  /** The release channel the check follows: stable or beta. */
  channel: string
  error?: string
}

export interface Alert {
  id: string
  level: 'warning' | 'error'
  kind: string
  site_id?: number
  site?: string
  /** English, for logs and older panels. */
  message: string
  /** The message in the interface's language (sent as Accept-Language). */
  text?: string
  at?: string
  /** A task alert's job kind (named by the interface) and its error, in the interface's language when the panel has it. */
  task_kind?: string
  /** task_kind in words, in the reader's language. */
  task_name?: string
  detail?: string
}

export interface CertificateStatus {
  exists: boolean
  hsts: boolean
  not_after?: string
  days_left?: number
  issuer?: string
  last_renewal_error?: string
}

export interface StagingDiff {
  added: string[] | null
  removed: string[] | null
  modified: string[] | null
  scanned: number
  truncated?: boolean
}

export interface QueryResult {
  columns: string[] | null
  rows: string[][] | null
  message?: string
}

/** One known vulnerability of something a WordPress site has installed (#31). */
export interface VulnFinding {
  id: string
  component: 'core' | 'plugin' | 'theme'
  slug?: string
  version: string
  title: string
  severity?: 'critical' | 'high' | 'medium' | 'low'
  cve?: string
  fixed_in?: string
  unfixed?: boolean
  link?: string
}

/** GET /api/sites/{id}/vulnerabilities. */
export interface VulnView {
  enabled: boolean
  checked: boolean
  checked_at: string
  attempted_at: string
  error?: string
  core?: string
  components: number
  findings: VulnFinding[]
  interval_seconds: number
  source: { name: string, url: string, license: string }
}

/** One file a malware scan found (#76). */
export interface MalwareFinding {
  id: number
  path: string
  signature: string
  status: 'open' | 'quarantined' | 'gone'
  first_seen: string
  last_seen: string
  quarantined_at?: string
  quarantine_id?: string
}

/** GET /api/sites/{id}/malware. */
export interface MalwareView {
  enabled: boolean
  scanned: boolean
  next_scan?: string
  scan: {
    scanned_at: string
    attempted_at: string
    error?: string
    files: number
    signatures: number
    duration_ms: number
    engine?: string
    database?: string
  }
  findings: MalwareFinding[]
  interval_seconds: number
}

/** A WordPress site's hardening (#75); an absent field is off. */
export interface WPHardening {
  xmlrpc_off?: boolean
  xmlrpc_allow?: string[]
  file_edit_off?: boolean
  hide_version?: boolean
  block_readme?: boolean
  login_path?: string
}
