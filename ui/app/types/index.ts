export type SiteType = 'wordpress' | 'woocommerce' | 'static' | 'php' | 'laravel' | 'proxy'
export type Profile = 'balanced' | 'commerce' | 'maximum'
export type Role = 'admin' | 'operator' | 'readonly'

export interface Me {
  id: number
  email: string
  first_name: string
  last_name: string
  /** Chosen interface language, '' until the user picks one. */
  locale: '' | 'it' | 'en' | 'fr' | 'es' | 'de'
  disabled: boolean
  role: Role
  site_ids?: number[]
  totp_enabled: boolean
  created_at: string
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
    object_cache: boolean
    sftp_enabled: boolean
    sftp_keys_only?: boolean
    shell_enabled?: boolean
    hsts?: boolean
    wildcard_cert?: boolean
    proxy_upstream?: string
    public_root?: string
    database: { enabled: boolean, name: string, user: string }
    backups: { enabled: boolean, schedule: string, retention_days: number, hour?: number | null }
    resources?: { memory_high_mb: number, php_workers: number }
    php: { memory_limit_mb: number, upload_max_mb: number, max_execution_seconds: number }
    /** Paths behind a username and password (the hash never leaves the panel). */
    protected_paths?: { path: string, username: string }[]
    /** The panel's periodic check from the public address (absent: off). */
    uptime?: { enabled: boolean, path?: string }
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
