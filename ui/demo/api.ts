// The demo's API (#39): what the panel's pages ask of /api/*, answered in the
// browser from sample data. Nothing here touches the network. A change the
// panel would make is refused (403, with the demo's words), except the few
// that only concern this browser: signing in and out and the language.
//
// Erasable TypeScript only (no enums, no parameter properties): tests/demo.test.mjs
// runs this file under node as it is.

import { messagesFor } from './messages.ts'

export interface DemoRequest {
  method: string
  /** The path, with no query. */
  path: string
  query: URLSearchParams
  body: unknown
  /** The language the interface asks in (Accept-Language), for the demo's words. */
  lang: string
}

export interface DemoResponse {
  status: number
  body: unknown
}

type Handler = (req: DemoRequest, m: RegExpMatchArray) => DemoResponse | unknown
type Query = Record<string, unknown>

const ok = (body: unknown = {}): DemoResponse => ({ status: 200, body })
const ago = (now: number, ms: number) => new Date(now - ms).toISOString()
const MIN = 60_000
const HOUR = 3_600_000
const DAY = 86_400_000

interface DemoSite {
  id: number
  domain: string
  aliases: string[] | null
  type: string
  profile: string
  engine: string
  php_version: string
  system_user: string
  root_path: string
  status: string
  staging_of?: number
  config: Query
  created_at: string
}

function sitesOf(now: number): DemoSite[] {
  const base = (id: number, domain: string, type: string, profile: string, extra: Query = {}, top: Partial<DemoSite> = {}): DemoSite => ({
    id,
    domain,
    aliases: top.aliases ?? null,
    type,
    profile,
    engine: 'nginx',
    php_version: type === 'static' ? '' : '8.4',
    system_user: `cg-site-${id}`,
    root_path: `/srv/sites/${domain}`,
    status: 'active',
    created_at: ago(now, (40 - id * 5) * DAY),
    ...top,
    config: {
      cache_enabled: type !== 'static',
      cache_ttl_sec: 0,
      object_cache: type === 'woocommerce',
      sftp_enabled: id !== 3,
      hsts: true,
      database: { enabled: type === 'wordpress' || type === 'woocommerce' || type === 'laravel', name: `site_${id}`, user: `site_${id}` },
      backups: { enabled: true, schedule: 'daily', retention_days: 14, hour: 3 },
      resources: { memory_high_mb: 384, php_workers: 4 },
      php: { memory_limit_mb: 256, upload_max_mb: 64, max_execution_seconds: 60 },
      uptime: { enabled: true, path: '/' },
      ...extra
    }
  })
  return [
    base(1, 'shop.example.com', 'woocommerce', 'commerce', { resources: { memory_high_mb: 768, php_workers: 8 }, php: { memory_limit_mb: 512, upload_max_mb: 128, max_execution_seconds: 120 } }, { aliases: ['www.shop.example.com'] }),
    base(2, 'blog.example.com', 'wordpress', 'balanced', { wp_two_factor: 'optional' }),
    base(3, 'docs.example.com', 'static', 'balanced'),
    base(4, 'app.example.com', 'laravel', 'maximum', { public_root: 'public' }),
    base(5, 'staging.shop.example.com', 'woocommerce', 'commerce', { cache_enabled: false }, { staging_of: 1 })
  ]
}

function logText(source: string, domain: string, now: number): string {
  const t = (s: number) => new Date(now - s * 1000).toISOString().replace('T', ' ').slice(0, 19)
  const paths = ['/', '/shop/', '/product/ceramic-mug/', '/cart/', '/wp-json/wc/store/v1/products', '/blog/hello-world/', '/robots.txt', '/favicon.ico']
  if (source === 'error') {
    return [
      `${t(5400)} [warn] 1201#1201: *48113 an upstream response is buffered to a temporary file while reading upstream, client: 203.0.113.17, server: ${domain}`,
      `${t(3100)} [error] 1201#1201: *48802 open() "/srv/sites/${domain}/current/apple-touch-icon.png" failed (2: No such file or directory), client: 198.51.100.4`,
      `${t(900)} [warn] 1201#1201: *49220 upstream server temporarily disabled while reading response header from upstream`
    ].join('\n')
  }
  if (source === 'php' || source === 'cleanup') return `${t(7200)} NOTICE: fpm is running, pid 912\n${t(7200)} NOTICE: ready to handle connections`
  return Array.from({ length: 24 }, (_, i) =>
    `203.0.113.${10 + (i % 9)} - - [${t((24 - i) * 70)}] "GET ${paths[i % paths.length]} HTTP/2.0" ${i % 11 === 0 ? 404 : 200} ${1200 + i * 313} "-" "Mozilla/5.0" HIT`).join('\n')
}

export function createDemo(opts: { now?: number } = {}) {
  const now = opts.now ?? Date.now()
  const sites = sitesOf(now)
  const me: Query = {
    id: 1, email: 'demo@cloudground.it', first_name: 'Demo', last_name: 'Admin', locale: '', whats_new_seen: 'demo', disabled: false, role: 'admin',
    totp_enabled: true, created_at: ago(now, 60 * DAY), password_signin: true, passkeys: 1, recovery_codes_left: 10
  }
  let signedIn = true
  const siteOf = (id: string) => sites.find(s => String(s.id) === id)

  const certOf = (s: DemoSite) => s.status !== 'active' || s.staging_of
    ? { exists: false, hsts: false }
    : { exists: true, hsts: true, not_after: ago(now, -(s.id * 11 + 31) * DAY), days_left: s.id * 11 + 31, issuer: "Let's Encrypt (R11)" }

  const backupsOf = (s: DemoSite) => s.staging_of || s.type === 'proxy'
    ? []
    : Array.from({ length: 6 }, (_, i) => ({
      id: s.id * 100 + i, site_id: s.id, snapshot_id: `${(s.id * 7919 + i * 104729).toString(16).padStart(8, '0')}c0ffee`, repository: 'sftp:backup@storage.example.net:/cloudground',
      size_bytes: (180 + s.id * 40 - i * 3) * 1048576, kind: 'scheduled', verify_status: i === 0 && s.id === 4 ? 'pending' : 'passed',
      verified_at: ago(now, i * DAY + 2 * HOUR), restore_estimate_ms: 42_000 + s.id * 3000, created_at: ago(now, i * DAY + 5 * HOUR),
      databases: s.config.database && (s.config.database as Query).enabled ? [`site_${s.id}`] : []
    }))

  const usageOf = (s: DemoSite, range: string) => {
    const n = range === '30d' ? 30 * 24 : 96
    const step = range === '30d' ? HOUR : 15 * MIN
    const points = Array.from({ length: n }, (_, i) => {
      const wave = (Math.sin(i / 7 + s.id) + 1) / 2
      const mem = (160 + s.id * 30 + wave * 90) * 1048576
      return { at: ago(now, (n - 1 - i) * step), cpu_pct: Math.round((6 + wave * 28) * 10) / 10, cpu_max_pct: Math.round((14 + wave * 40) * 10) / 10, mem_bytes: mem, mem_pct: Math.round(mem / (768 * 1048576) * 1000) / 10, io_read_bps: 40000 + wave * 90000, io_write_bps: 20000 + wave * 60000, disk_bytes: (2.1 + s.id * 0.4) * 1073741824 }
    })
    return {
      range, cpus: 2, limits: { memory_max_mb: 1024, memory_high_mb: Number((s.config.resources as Query).memory_high_mb), cpu_quota_pct: 0, tasks_max: 512 }, points,
      latest: { sampled_at: ago(now, MIN), cpu_cores: 0.21, mem_bytes: 232 * 1048576, mem_max_bytes: 1024 * 1048576, io_read_bps: 61000, io_write_bps: 32000, tasks: 24, oom_kills: 0, disk_bytes: (2.1 + s.id * 0.4) * 1073741824, disk_inodes: 48211 }
    }
  }

  const routes: [string, RegExp, Handler][] = [
    ['GET', /^\/api\/bootstrap$/, () => ({ setup_complete: true })],
    ['GET', /^\/api\/session$/, () => ({ user: signedIn ? me : null })],
    ['POST', /^\/api\/login$/, () => { signedIn = true; return { user: me } }],
    ['POST', /^\/api\/logout$/, () => { signedIn = false; return {} }],
    ['PUT', /^\/api\/account\/profile$/, (r) => {
      const b = (r.body ?? {}) as Query
      if (typeof b.locale === 'string') me.locale = b.locale
      return me
    }],
    ['POST', /^\/api\/account\/whats-new$/, () => ({ seen: 'demo' })],
    ['GET', /^\/api\/changelog$/, () => ({ current: 'demo', seen: 'demo', unseen: [], entries: [{ version: 'demo', body: 'This is the demo of CloudGround: the real interface with sample data.' }] })],
    ['GET', /^\/api\/tasks\/names$/, () => ({})],
    ['GET', /^\/api\/tasks$/, () => [
      { id: 3, kind: 'backup.run', site_id: 1, status: 'succeeded', progress: 100, message: 'Backup verified', created_at: ago(now, 5 * HOUR) },
      { id: 2, kind: 'cert.issue', site_id: 2, status: 'succeeded', progress: 100, message: 'Certificate issued', created_at: ago(now, 2 * DAY) },
      { id: 1, kind: 'site.create', site_id: 4, status: 'succeeded', progress: 100, message: 'Site ready', created_at: ago(now, 9 * DAY) }
    ]],
    ['GET', /^\/api\/system\/status$/, () => ({
      cpu_count: 4, load1: 0.62, mem_total_mb: 7953, mem_available_mb: 4630, disk_total_mb: 160000, disk_free_mb: 103400, agent_version: 'demo', uptime_seconds: 19 * DAY,
      cpu_headroom_pct: 84, mem_headroom_pct: 58, disk_headroom_pct: 65
    })],
    ['GET', /^\/api\/system\/metrics$/, () => Array.from({ length: 288 }, (_, i) => {
      const wave = (Math.sin(i / 20) + 1) / 2
      return { cpu_headroom_pct: 70 + wave * 20, mem_headroom_pct: 52 + wave * 8, disk_headroom_pct: 65, load1: 0.3 + wave * 0.8, sampled_at: ago(now, (287 - i) * 5 * MIN) }
    })],
    ['GET', /^\/api\/system\/alerts$/, () => [
      { id: 'demo-cert', level: 'warning', kind: 'cert.expiring', site_id: 3, site: 'docs.example.com', message: 'The certificate of docs.example.com expires in 31 days', text: 'The certificate of docs.example.com expires in 31 days', at: ago(now, 3 * HOUR) }
    ]],
    ['GET', /^\/api\/system\/drift$/, () => []],
    ['GET', /^\/api\/system\/capabilities$/, () => ({
      php_versions: ['8.5', '8.4', '8.3'], default_php: '8.4', services: [
        { name: 'nginx', unit: 'nginx.service', listed: true, restartable: true }, { name: 'mariadb', unit: 'mariadb.service', listed: true, restartable: true }
      ], sites_root: '/srv/sites', http3: true, time_zone: 'Europe/Rome'
    })],
    ['GET', /^\/api\/system\/update$/, () => ({ current: 'demo', update_available: false, check_enabled: false, channel: 'stable' })],
    ['GET', /^\/api\/system\/updates$/, () => ({
      packages: [
        { name: 'openssl', from: '3.0.13-0ubuntu3.4', to: '3.0.13-0ubuntu3.5', origin: 'noble-security', security: true },
        { name: 'tzdata', from: '2024a-0ubuntu1', to: '2024b-0ubuntu0.24.04', origin: 'noble-updates' }
      ],
      security: 1, reboot_required: false, lists_updated_at: ago(now, 4 * HOUR), checked_at: ago(now, 4 * HOUR)
    })],
    ['GET', /^\/api\/settings$/, () => ({
      acme_email: 'admin@example.com', backup_repository: 'sftp:backup@storage.example.net:/cloudground', backup_password: '********', panel_domain: 'panel.example.com',
      alert_emails: 'admin@example.com', mail_host: 'smtp.example.com', mail_port: '587', mail_security: 'starttls', mail_username: 'cloudground', mail_password: '********', mail_from: 'cloudground@example.com'
    })],
    ['GET', /^\/api\/blocklist$/, () => ({
      settings: { enabled: true, max_failures: 5, window_minutes: 10, ban_minutes: 60 },
      blocked: [
        { id: 2, address: '203.0.113.77', source: 'wordpress', note: 'shop.example.com: 6 failed sign-ins', failures: 6, created_at: ago(now, 2 * HOUR), expires_at: ago(now, -HOUR) },
        { id: 1, address: '198.51.100.23', source: 'ssh', note: '5 failed logins', failures: 5, created_at: ago(now, 1 * DAY), expires_at: ago(now, -23 * HOUR) }
      ],
      allowed: [{ id: 3, address: '192.0.2.10', source: 'manual', note: 'Office', created_at: ago(now, 20 * DAY) }],
      enforced: true, applied_at: ago(now, 2 * HOUR), watching: { ssh: true, wordpress: true }, your_address: '192.0.2.44'
    })],
    ['GET', /^\/api\/services$/, () => [
      { name: 'nginx', unit: 'nginx.service', active: true, enabled: true, state: 'running', memory_bytes: 41 * 1048576, since: ago(now, 19 * DAY), version: '1.28.0' },
      { name: 'mariadb', unit: 'mariadb.service', active: true, enabled: true, state: 'running', memory_bytes: 312 * 1048576, since: ago(now, 19 * DAY), version: '11.4' },
      { name: 'site-php', unit: 'cloudground-php-*.service', active: true, enabled: true, state: 'running', memory_bytes: 905 * 1048576, since: ago(now, 6 * DAY) },
      { name: 'cloudground-agent', unit: 'cloudground-agent.service', active: true, enabled: true, state: 'running', memory_bytes: 28 * 1048576, since: ago(now, 19 * DAY), version: 'demo' }
    ]],
    ['GET', /^\/api\/firewall$/, () => ({ enabled: true, rules: ['22/tcp ALLOW IN Anywhere', '80/tcp ALLOW IN Anywhere', '443/tcp ALLOW IN Anywhere', '443/udp ALLOW IN Anywhere'] })],
    ['GET', /^\/api\/audit$/, () => [
      { id: 4, actor: 'demo@cloudground.it', action: 'site.update', subject: 'shop.example.com', detail: 'cache_ttl_sec', created_at: ago(now, 3 * HOUR) },
      { id: 3, actor: 'demo@cloudground.it', action: 'backup.restore', subject: 'blog.example.com', detail: 'database', created_at: ago(now, 1 * DAY) },
      { id: 2, actor: 'demo@cloudground.it', action: 'cert.issue', subject: 'blog.example.com', detail: '', created_at: ago(now, 2 * DAY) },
      { id: 1, actor: 'demo@cloudground.it', action: 'site.create', subject: 'app.example.com', detail: 'laravel', created_at: ago(now, 9 * DAY) }
    ]],
    ['GET', /^\/api\/users$/, () => [
      { id: 1, email: 'demo@cloudground.it', first_name: 'Demo', last_name: 'Admin', disabled: false, role: 'admin', totp_enabled: true, created_at: ago(now, 60 * DAY), passkeys: 1 },
      { id: 2, email: 'operator@example.com', first_name: 'Olivia', last_name: 'Operator', disabled: false, role: 'operator', site_ids: [1, 2], totp_enabled: false, created_at: ago(now, 30 * DAY), passkeys: 0 },
      { id: 3, email: 'viewer@example.com', first_name: 'Victor', last_name: 'Viewer', disabled: false, role: 'readonly', totp_enabled: false, created_at: ago(now, 12 * DAY), passkeys: 0 }
    ]],
    ['GET', /^\/api\/tokens$/, () => []],
    ['GET', /^\/api\/account\/passkeys$/, () => [{ id: 1, name: 'Laptop', rp_id: 'demo.cloudground.it', created_at: ago(now, 20 * DAY), last_used_at: ago(now, 1 * DAY), backed_up: true }]],
    ['GET', /^\/api\/account\/sessions$/, () => [{ id: 'demo', current: true, created_at: ago(now, 2 * HOUR), expires_at: ago(now, -22 * HOUR), last_seen: ago(now, MIN), browser: 'This browser' }]],
    ['GET', /^\/api\/logs$/, (r) => ({ path: r.query.get('source') ?? 'nginx-access', content: logText(r.query.get('source')?.includes('error') ? 'error' : 'access', 'example.com', now) })],
    ['GET', /^\/api\/sites$/, () => sites],
    ['GET', /^\/api\/sites\/(\d+)$/, (_r, m) => siteOf(m[1]!) ?? { status: 404, body: { error: 'site not found' } }],
    ['GET', /^\/api\/sites\/(\d+)\/certificate$/, (_r, m) => { const s = siteOf(m[1]!); return s ? certOf(s) : notFound() }],
    ['GET', /^\/api\/sites\/(\d+)\/backups$/, (_r, m) => { const s = siteOf(m[1]!); return s ? backupsOf(s) : notFound() }],
    ['GET', /^\/api\/sites\/(\d+)\/cache-stats$/, () => ({ backend: 'nginx', hits: 183_204, misses: 4_912, entries: 1_308, mem_used: 0, mem_total: 0, hit_rate_pct: 97.4 })],
    ['GET', /^\/api\/sites\/(\d+)\/usage$/, (r, m) => { const s = siteOf(m[1]!); return s ? usageOf(s, r.query.get('range') === '30d' ? '30d' : '24h') : notFound() }],
    ['GET', /^\/api\/sites\/(\d+)\/logs$/, (r, m) => ({ path: `/var/log/cloudground/${siteOf(m[1]!)?.domain ?? 'site'}/${r.query.get('source') ?? 'access'}.log`, content: logText(r.query.get('source') ?? 'access', siteOf(m[1]!)?.domain ?? 'example.com', now) })],
    ['GET', /^\/api\/sites\/(\d+)\/deployments$/, (_r, m) => {
      const s = siteOf(m[1]!)
      if (!s || s.type === 'wordpress' || s.type === 'woocommerce' || s.type === 'static') return []
      return [
        { id: 12, site_id: s.id, release_id: '20261024-141500', status: 'promoted', checksum: 'sha256:9f2c1a', created_at: ago(now, 3 * DAY), promoted_at: ago(now, 3 * DAY) },
        { id: 11, site_id: s.id, release_id: '20261019-093000', status: 'superseded', checksum: 'sha256:51ab77', created_at: ago(now, 8 * DAY), promoted_at: ago(now, 8 * DAY) }
      ]
    }],
    ['GET', /^\/api\/sites\/(\d+)\/cron$/, (_r, m) => [
      { id: 1, site_id: Number(m[1]), schedule: '*/5 * * * *', command: 'php artisan schedule:run', description: 'Laravel scheduler', enabled: true, last_run: ago(now, 3 * MIN), last_status: 'ok', created_at: ago(now, 9 * DAY) }
    ]],
    ['GET', /^\/api\/sites\/(\d+)\/databases$/, (_r, m) => {
      const s = siteOf(m[1]!)
      const on = !!s && !!(s.config.database as Query).enabled
      return {
        databases: on ? [{ name: `site_${s!.id}`, primary: true, size_bytes: (38 + s!.id * 9) * 1048576, tables: 42, created_at: s!.created_at }] : [],
        users: on ? [{ name: `site_${s!.id}`, primary: true, grants: [{ database: `site_${s!.id}`, level: 'all' }], created_at: s!.created_at }] : []
      }
    }],
    ['GET', /^\/api\/sites\/(\d+)\/wp\/plugins$/, () => [
      { name: 'woocommerce', status: 'active', version: '9.4.2', update: '' },
      { name: 'wordpress-seo', status: 'active', version: '24.1', update: '24.2' },
      { name: 'akismet', status: 'inactive', version: '5.3.3', update: '' }
    ]],
    ['GET', /^\/api\/sites\/(\d+)\/wp\/cleanup$/, () => ({ enabled: true, keep_revisions: 5, last_run: ago(now, 3 * DAY), next_run: ago(now, -4 * DAY), backups_ready: true })],
    ['GET', /^\/api\/php\/([\d.]+)\/extensions$/, (_r, m) => ({
      version: m[1], modules: ['Core', 'date', 'json', 'mysqlnd', 'openssl', 'pcre', 'zlib'],
      extensions: ['bcmath', 'curl', 'gd', 'imagick', 'intl', 'mbstring', 'mysqli', 'opcache', 'redis', 'xml', 'zip'].map((name, i) => ({ name, installed: i !== 3 && i !== 8 }))
    })],
    ['GET', /^\/api\/sites\/(\d+)\/staging\/credentials$/, () => ({ url: 'https://staging.shop.example.com', user: 'staging', password: 'demo-only-not-a-secret' })],
    ['GET', /^\/api\/sites\/(\d+)\/staging\/diff$/, () => ({ added: ['wp-content/uploads/2026/10/banner.jpg'], removed: [], modified: ['wp-content/themes/shop/style.css', 'wp-content/themes/shop/functions.php'], scanned: 4821 })],
    ['GET', /^\/api\/sites\/(\d+)\/staging\/tables$/, () => ({ tables: [{ name: 'wp_options', protected: true }, { name: 'wp_posts', protected: false }, { name: 'wp_postmeta', protected: false }, { name: 'wp_users', protected: true }] })],
    ['GET', /^\/api\/sites\/(\d+)\/ssh-keys$/, () => ({
      keys: [{ type: 'ssh-ed25519', fingerprint: 'SHA256:3b1xK0mJf9VQq0o8dYV3mS2yq1cX0Yw0uJQ4mBz5k2A', label: 'deploy@ci', access: 'sftp' }], port: 22, shell_user: ''
    })],
    ['GET', /^\/api\/sites\/(\d+)\/mail$/, () => ({ off: false, from: '', dkim_selector: '', relay: null, effective: 'server', sender: 'cloudground@example.com' })],
    ['GET', /^\/api\/sites\/(\d+)\/mail\/checks$/, (_r, m) => ({ domain: siteOf(m[1]!)?.domain ?? '', spf: { found: true, record: 'v=spf1 include:_spf.example.com ~all' }, dmarc: { found: false }, dkim: { selector: 's1', found: false } })],
    ['GET', /^\/api\/sites\/(\d+)\/maintenance$/, () => ({ enabled: false, title: '', message: '', retry_after_sec: 3600, allow: [] })],
    ['GET', /^\/api\/sites\/(\d+)\/uptime$/, (_r, m) => ({
      enabled: true, path: '/', url: `https://${siteOf(m[1]!)?.domain ?? 'example.com'}/`, interval_seconds: 60,
      last: { checked_at: ago(now, 40_000), up: true, status: 200, latency_ms: 118 }
    })],
    ['GET', /^\/api\/sites\/(\d+)\/vulnerabilities$/, () => ({
      enabled: true, checked: true, checked_at: ago(now, 6 * HOUR), attempted_at: ago(now, 6 * HOUR), core: '6.7.1', components: 23,
      findings: [{ id: 'demo-1', component: 'plugin', slug: 'contact-form-demo', version: '5.8.0', title: 'Cross-site scripting in a form field', severity: 'medium', fixed_in: '5.8.2' }],
      interval_seconds: 86400, source: { name: 'Sample data', url: 'https://cloudground.it/', license: 'demo' }
    })],
    ['GET', /^\/api\/sites\/(\d+)\/malware$/, () => ({
      enabled: true, scanned: true, next_scan: ago(now, -18 * HOUR),
      scan: { scanned_at: ago(now, 6 * HOUR), attempted_at: ago(now, 6 * HOUR), files: 18423, signatures: 2210, duration_ms: 41000, engine: 'demo' },
      findings: [], interval_seconds: 86400
    })],
    ['GET', /^\/api\/sites\/(\d+)\/files$/, (r) => ({
      entries: r.query.get('path')
        ? [{ name: 'index.php', is_dir: false, size: 1824, mode: '0644', mod_time: ago(now, 3 * DAY) }]
        : [
          { name: 'wp-content', is_dir: true, size: 4096, mode: '0755', mod_time: ago(now, 1 * DAY) },
          { name: 'wp-config.php', is_dir: false, size: 3012, mode: '0640', mod_time: ago(now, 30 * DAY) },
          { name: 'index.php', is_dir: false, size: 405, mode: '0644', mod_time: ago(now, 30 * DAY) }
        ]
    })],
    ['GET', /^\/api\/sites\/(\d+)\/files\/read$/, () => ({ content: '<?php\n// Sample file from the demo.\necho "Hello from CloudGround";\n', size: 62, binary: false, too_large: false, encoding: 'utf-8' })]
  ]

  function notFound(): DemoResponse {
    return { status: 404, body: { error: 'not found' } }
  }

  // The changes that concern this browser only: they touch no sample data
  // that survives a reload and never leave it.
  const local = (r: DemoRequest) => r.method !== 'GET' && routes.some(([method, re]) => method === r.method && re.test(r.path))

  function handle(req: DemoRequest): DemoResponse {
    const words = messagesFor(req.lang)
    const method = req.method.toUpperCase()
    if (method !== 'GET' && method !== 'HEAD') {
      if (!local({ ...req, method })) return { status: 403, body: { error: words.refused, demo: true } }
    }
    for (const [m, re, fn] of routes) {
      if (m !== (method === 'HEAD' ? 'GET' : method)) continue
      const match = req.path.match(re)
      if (!match) continue
      const out = fn({ ...req, method }, match)
      if (out && typeof out === 'object' && 'status' in out && 'body' in out) return out as DemoResponse
      return ok(out)
    }
    return { status: 404, body: { error: words.missing, demo: true } }
  }

  return { handle, sites }
}
