/**
 * The new-site wizard's helpers (pages/sites/new.vue): its steps and their
 * checks, the request it sends, the draft it keeps across a reload, the
 * database names it proposes, the passwords it generates and the summary it
 * shows once the site is created (internal/api SPEC §3.2). The names follow internal/site's rules
 * (database.go): 3 to 48 lowercase letters, digits and _, starting with a
 * letter, never reserved; the server checks them again.
 */

import type { SiteType } from '~/types'

/** The summary POST /api/sites answers, the only answer with the database password. */
export interface SiteSummary {
  site_url: string
  panel_path: string
  panel_url?: string
  wordpress?: { admin_url: string, admin_user: string }
  database?: { host: string, port: number, socket?: string, name: string, user: string, password: string }
  sftp: { enabled: boolean, user: string, host?: string, port?: number }
}

export const DB_NAME_RE = /^[a-z][a-z0-9_]{2,47}$/
const MAX = 48
const RESERVED = new Set(['mysql', 'sys', 'performance_schema', 'information_schema', 'root', 'debian_sys_maint', 'mariadb_sys',
  'mysql_sys', 'mariadb', 'mysqld', 'admin', 'test', 'phpmyadmin', 'cloudground', 'replication', 'repl', 'monitor', 'backup'])
const RESERVED_PREFIXES = ['mysql_', 'sys_', 'test_', 'mariadb_']

/** Whether name is one the server reserves (its own, the agent's, the panel's site_<n>). */
export function reservedDBName(name: string): boolean {
  return RESERVED.has(name) || RESERVED_PREFIXES.some(p => name.startsWith(p)) ||
    /^cg._/.test(name) || /^site_[0-9]+$/.test(name)
}

/**
 * The database name proposed for a domain: its labels without the top-level
 * one, joined by _ (blog.example.com → blog_example), cut to the rules.
 * "" while the domain gives nothing to work with.
 */
export function proposeDBName(domain: string): string {
  let d = domain.trim().toLowerCase().replace(/^www\./, '').replace(/\.+$/, '')
  if (!d) return ''
  const labels = d.split('.').filter(Boolean)
  if (labels.length > 1) labels.pop()
  d = labels.join('_').replace(/[^a-z0-9_]+/g, '_').replace(/_+/g, '_').replace(/^_+|_+$/g, '')
  if (!d) return ''
  if (!/^[a-z]/.test(d)) d = 'db_' + d
  if (d.length < 3) d += '_db'
  if (reservedDBName(d)) d = 'site_' + d
  return d.slice(0, MAX).replace(/_+$/, '')
}

/**
 * The i18n key of what is wrong with a chosen database password, or
 * undefined: the server's one rule (internal/api SPEC §6.5), for a new
 * site and the Database tab alike. 12 to 256 characters, no control
 * characters, none of ' " \ (wp-cli cannot write them into wp-config.php),
 * no space at either end.
 */
export function dbPasswordIssue(pw: string): string | undefined {
  if (pw.length < 12 || pw.length > 256) return 'newSite.dbPasswordRule'
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f-\u009f]/.test(pw)) return 'newSite.dbPasswordRule'
  if (/['"\\]/.test(pw)) return 'newSite.dbPasswordChars'
  if (pw.trim() !== pw) return 'newSite.dbPasswordSpaces'
  return undefined
}

/** A random password of 24 characters (18 bytes, URL-safe base64). */
export function generatePassword(): string {
  const bytes = new Uint8Array(18)
  crypto.getRandomValues(bytes)
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_')
}

/** The panel link of a summary, absolute: the API's, else this page's origin. */
export function panelLink(s: SiteSummary, origin: string): string {
  return s.panel_url || origin + s.panel_path
}

/**
 * The summary as plain text, for the .txt download and Copy: one heading
 * per part, label: value lines, and a first line saying it holds secrets.
 */
export function summaryText(s: SiteSummary, domain: string, origin: string, t: (key: string, args?: Record<string, unknown>) => string): string {
  const out: string[] = [t('newSite.txtTitle', { domain }), t('newSite.txtSecret'), '']
  const part = (title: string, rows: [string, string | number | undefined][]) => {
    out.push(title)
    for (const [label, value] of rows) {
      if (value !== undefined && value !== '') out.push(`  ${label}: ${value}`)
    }
    out.push('')
  }
  part(t('newSite.partSite'), [[t('newSite.siteUrl'), s.site_url], [t('newSite.panelUrl'), panelLink(s, origin)]])
  if (s.wordpress) part('WordPress', [[t('newSite.adminUrl'), s.wordpress.admin_url], [t('newSite.adminUser'), s.wordpress.admin_user]])
  if (s.database) {
    const d = s.database
    part(t('newSite.partDatabase'), [[t('newSite.dbHost'), d.host], [t('newSite.dbPort'), d.port], [t('newSite.dbSocket'), d.socket],
      [t('newSite.dbName'), d.name], [t('newSite.dbUser'), d.user], [t('newSite.dbPassword'), d.password]])
  }
  if (s.sftp.enabled) {
    part('SFTP', [[t('newSite.sftpHost'), s.sftp.host], [t('newSite.sftpPort'), s.sftp.port], [t('newSite.sftpUser'), s.sftp.user]])
  } else {
    out.push('SFTP', `  ${t('newSite.sftpOff', { user: s.sftp.user })}`, '')
  }
  return out.join('\n')
}

/** Offers text as a .txt file to save, made in the browser. */
export function downloadText(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// ---- the wizard ----

/** The wizard's steps, in order. Every type goes through all five. */
export const WIZARD_STEPS = ['type', 'domain', 'setup', 'database', 'review'] as const
export type WizardStep = typeof WIZARD_STEPS[number]

/** What the wizard asks for: the fields of the create request, as typed. */
export interface SiteDraft {
  type: SiteType
  profile: string
  domain: string
  php_version: string
  title: string
  admin_email: string
  admin_user: string
  admin_password: string
  proxy_upstream: string
  with_db: boolean
  db_name: string
  db_user: string
  db_password: string
}

export function freshDraft(): SiteDraft {
  return {
    type: 'wordpress',
    profile: 'auto',
    domain: '',
    php_version: 'auto',
    title: '',
    admin_email: '',
    admin_user: 'admin',
    admin_password: '',
    proxy_upstream: '',
    with_db: false,
    db_name: '',
    db_user: '',
    db_password: generatePassword()
  }
}

export const isWPType = (t: SiteType) => t === 'wordpress' || t === 'woocommerce'
export const hasPHPType = (t: SiteType) => t !== 'static' && t !== 'proxy'
/** A type whose configuration uses a database always gets one. */
export const dbAlways = (t: SiteType) => t === 'wordpress' || t === 'woocommerce' || t === 'laravel'
/** A type that may get one if asked; a proxy never does. */
export const dbOptional = (t: SiteType) => t === 'static' || t === 'php'
export const wantsDB = (d: Pick<SiteDraft, 'type' | 'with_db'>) => dbAlways(d.type) || (dbOptional(d.type) && d.with_db)
/** The profile "auto" stands for: the server picks it by type. */
export const autoProfile = (t: SiteType) => t === 'woocommerce' ? 'commerce' : 'balanced'

/** A problem with a field: an i18n key and its arguments. */
export interface FieldProblem { key: string, args?: Record<string, unknown> }

function nameProblem(n: string): FieldProblem | undefined {
  if (!n) return { key: 'newSite.nameRequired' }
  if (!DB_NAME_RE.test(n)) return { key: 'dbs.nameRule' }
  if (reservedDBName(n)) return { key: 'newSite.nameReserved' }
  return undefined
}

/** The step that asks for a field of the request (or of the server's answer). */
export const FIELD_STEP: Record<string, WizardStep> = {
  type: 'type',
  profile: 'type',
  domain: 'domain',
  php_version: 'setup',
  title: 'setup',
  admin_email: 'setup',
  admin_user: 'setup',
  admin_password: 'setup',
  proxy_upstream: 'setup',
  create_database: 'database',
  database_name: 'database',
  database_user: 'database',
  database_password: 'database'
}

/**
 * What is wrong with the draft, by the request's field names: the server's
 * rules, which it checks again. Only what the chosen type sends is checked.
 */
export function draftProblems(d: SiteDraft): Record<string, FieldProblem> {
  const e: Record<string, FieldProblem | undefined> = {}
  if (!d.domain.trim()) e.domain = { key: 'newSite.domainRequired' }
  if (isWPType(d.type)) {
    if (!/^[^\s@]+@[^\s@]+$/.test(d.admin_email.trim())) e.admin_email = { key: 'newSite.emailRequired' }
    if (!d.admin_user.trim()) e.admin_user = { key: 'newSite.adminUserRequired' }
    if (d.admin_password.length < 12) e.admin_password = { key: 'auth.min12' }
  }
  if (d.type === 'proxy' && !d.proxy_upstream.trim()) e.proxy_upstream = { key: 'newSite.upstreamRequired' }
  if (wantsDB(d)) {
    e.database_name = nameProblem(d.db_name)
    e.database_user = nameProblem(d.db_user)
    const issue = dbPasswordIssue(d.db_password)
    if (issue) e.database_password = { key: issue }
  }
  return Object.fromEntries(Object.entries(e).filter((x): x is [string, FieldProblem] => !!x[1]))
}

/** The problems a step's Next stops on (the review's are all of them). */
export function stepProblems(d: SiteDraft, step: WizardStep): Record<string, FieldProblem> {
  const all = draftProblems(d)
  if (step === 'review') return all
  return Object.fromEntries(Object.entries(all).filter(([f]) => FIELD_STEP[f] === step))
}

/** The first step with a problem among fields, or undefined. */
export function firstStepOf(fields: string[]): WizardStep | undefined {
  const steps = new Set(fields.map(f => FIELD_STEP[f]).filter(Boolean))
  return WIZARD_STEPS.find(s => steps.has(s))
}

/** The body of POST /api/sites: only what the chosen type uses. */
export function createBody(d: SiteDraft): Record<string, unknown> {
  const body: Record<string, unknown> = { domain: d.domain.trim().toLowerCase(), type: d.type }
  if (d.profile !== 'auto') body.profile = d.profile
  if (hasPHPType(d.type) && d.php_version !== 'auto') body.php_version = d.php_version
  if (isWPType(d.type)) Object.assign(body, { title: d.title, admin_email: d.admin_email.trim(), admin_user: d.admin_user.trim(), admin_password: d.admin_password })
  if (d.type === 'proxy') body.proxy_upstream = d.proxy_upstream.trim()
  if (dbOptional(d.type)) body.create_database = d.with_db
  if (wantsDB(d)) Object.assign(body, { database_name: d.db_name, database_user: d.db_user, database_password: d.db_password })
  return body
}

// ---- the draft kept across a reload ----

/** The session storage key of the draft. */
export const DRAFT_KEY = 'cg:new-site'
const SECRET_FIELDS = ['admin_password', 'db_password'] as const
const TYPES: SiteType[] = ['wordpress', 'woocommerce', 'static', 'php', 'laravel', 'proxy']

/** What a reload keeps: the step and the draft without its passwords. */
export interface StoredDraft { step: WizardStep, draft: Partial<SiteDraft>, edited: { name: boolean, user: boolean } }

/** The draft as stored: never a password, which a reload asks for again. */
export function storeDraft(step: WizardStep, d: SiteDraft, edited: { name: boolean, user: boolean }): string {
  const draft: Partial<SiteDraft> = { ...d }
  for (const f of SECRET_FIELDS) delete draft[f]
  return JSON.stringify({ step, draft, edited })
}

/**
 * A stored draft read back, field by field: what has the wrong shape (an
 * older panel's, a hand-edited one) is dropped, and a password is never
 * taken from it. null when there is nothing usable.
 */
export function readDraft(raw: string | null): StoredDraft | null {
  if (!raw) return null
  let v: unknown
  try {
    v = JSON.parse(raw)
  } catch {
    return null
  }
  if (!v || typeof v !== 'object') return null
  const o = v as Record<string, unknown>
  const src = (o.draft && typeof o.draft === 'object' ? o.draft : {}) as Record<string, unknown>
  const ref = freshDraft() as unknown as Record<string, unknown>
  const draft: Record<string, unknown> = {}
  for (const [k, def] of Object.entries(ref)) {
    if ((SECRET_FIELDS as readonly string[]).includes(k)) continue
    if (k in src && typeof src[k] === typeof def) draft[k] = src[k]
  }
  if (draft.type !== undefined && !TYPES.includes(draft.type as SiteType)) delete draft.type
  const step = WIZARD_STEPS.includes(o.step as WizardStep) ? o.step as WizardStep : 'type'
  const e = (o.edited && typeof o.edited === 'object' ? o.edited : {}) as Record<string, unknown>
  return { step, draft: draft as Partial<SiteDraft>, edited: { name: e.name === true, user: e.user === true } }
}
