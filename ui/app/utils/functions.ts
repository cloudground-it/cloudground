/**
 * Standalone functions (issue 143, v0.9.0): the API's shapes (internal/api SPEC
 * §17) and the few rules the panel checks before the server does
 * (internal/functions SPEC §1–§2).
 */

export type FnRuntime = 'node' | 'python'
export type FnAuth = 'public' | 'token'

/** One invocation: a request, a scheduled run or a test run. */
export interface FnInvocation { time: string, method: string, path: string, status: number, ms: number, source: string }

export interface FnView {
  id: number
  name: string
  runtime: FnRuntime
  version: string
  memory_mb: number
  timeout_sec: number
  schedule: string
  env: Record<string, string>
  /** The names only. */
  secrets: string[]
  auth: FnAuth
  cors_origins: string[]
  domains: string[]
  release: string
  created_at: string
  updated_at: string
  urls: string[]
  has_token: boolean
  /** The list's: the newest invocation. */
  last?: FnInvocation
  /** One function's: its entry file, the live release's code and its releases (newest first). */
  entry?: string
  files?: Record<string, string>
  releases?: string[]
  /** Only in the answer that made a token. */
  token?: string
}

export interface FnList { functions: FnView[], domain: string }
export interface FnLogs { invocations: FnInvocation[], output: string[] }
export interface FnInvokeResult { status: number, ms: number, body: string, error?: string }

/** The DNS readiness check (internal/api SPEC §3.14). */
export interface DnsCheck {
  domain: string
  status: 'here' | 'partial' | 'elsewhere' | 'proxied' | 'unresolved' | 'unknown'
  addresses: { family: number, address?: string, private?: boolean, own: boolean }[]
  server: string[]
  cdn?: string
}
export interface CertState { exists: boolean, not_after?: string, issuer?: string }
export interface FnDomainStatus { domain: string, dns?: DnsCheck | null, certificate: CertState, acme_email: boolean }
export interface FnOwnDomainsStatus { domains: { domain: string, dns: DnsCheck }[], certificate: CertState }

/** A secret's value as the API shows it: sent back, it keeps the secret. */
export const SECRET_MASK = '••••••••'
export const FN_NAME_RE = /^[a-z][a-z0-9-]{0,30}$/
const PART_RE = /^[A-Za-z0-9._-]{1,100}$/

export const fnEntry = (runtime: FnRuntime) => runtime === 'node' ? 'index.js' : 'main.py'
export const fnRuntimeName = (runtime: FnRuntime) => runtime === 'node' ? 'Node.js' : 'Python'
export const fnRuntimeIcon = (runtime: FnRuntime) => runtime === 'node' ? 'i-brand-node' : 'i-brand-python'

/** Whether a file path is one a release may hold (internal/functions SPEC §2). */
export function fnPathOK(path: string): boolean {
  if (!path || path.length > 255 || path === '.cg-files') return false
  const parts = path.split('/')
  if (parts[0] === 'node_modules' || parts[0] === '.venv') return false
  return parts.every(p => PART_RE.test(p) && p !== '.' && p !== '..')
}

/** The entry file first, then the rest by path. */
export function fnFileOrder(paths: string[], entry: string): string[] {
  return [...paths].sort((a, b) => a === entry ? -1 : b === entry ? 1 : a.localeCompare(b))
}

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'
const ULID_RE = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/
const LEGACY_RE = /^(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})(?:\.(\d{3})-[0-9a-f]{6})?$/

/**
 * When a release was made, from its ID: a ULID's first ten characters are
 * the time in milliseconds; the names from before ULIDs are the UTC time
 * (internal/site, release.go). Undefined for anything else.
 */
export function releaseTime(id: string): string | undefined {
  if (ULID_RE.test(id)) {
    let ms = 0
    for (const c of id.slice(0, 10)) ms = ms * 32 + CROCKFORD.indexOf(c)
    return new Date(ms).toISOString()
  }
  const m = id.match(LEGACY_RE)
  if (m) return new Date(Date.UTC(+m[1]!, +m[2]! - 1, +m[3]!, +m[4]!, +m[5]!, +m[6]!, +(m[7] ?? 0))).toISOString()
  return undefined
}

/** A badge colour for an HTTP status. */
export function httpStatusColor(status: number): 'success' | 'warning' | 'error' | 'neutral' {
  if (!status) return 'error'
  if (status >= 500) return 'error'
  if (status >= 400) return 'warning'
  return 'success'
}
