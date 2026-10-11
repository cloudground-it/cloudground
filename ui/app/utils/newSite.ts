/**
 * The new-site dialog's helpers: the database names it proposes, the
 * passwords it generates and the summary it shows once the site is
 * created (internal/api SPEC §3.2). The names follow internal/site's rules
 * (database.go): 3 to 48 lowercase letters, digits and _, starting with a
 * letter, never reserved; the server checks them again.
 */

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
