/**
 * Quarry as a service of its own (/quarry, internal/api SPEC §6.4):
 * its session, apart from the panel's, and a client for /api/quarry/…
 * that sends the person back to Quarry's sign-in, never the panel's,
 * when the session has ended.
 */
export interface StudioSession {
  /** login: a database user's own name and password; panel: opened from the panel. */
  mode: 'login' | 'panel'
  /** The database user, or the panel account's e-mail. */
  user: string
  site: { id: number, domain: string }
  database: string
  /** Opened by a read-only panel account: tables and structure only. */
  read_only: boolean
  can_write: boolean
  idle_seconds: number
  expires_at: string
  databases?: string[]
}

/** Where the tool lives: its pages, and its API (internal/api SPEC §6.4). */
export const STUDIO_HOME = '/quarry'
export const STUDIO_API = '/api/quarry'

export function useStudio() {
  const session = useState<StudioSession | null>('studio:session', () => null)
  const loaded = useState<boolean>('studio:loaded', () => false)
  const ended = useState<string>('studio:ended', () => '')
  const api = useApi()

  async function load(): Promise<StudioSession | null> {
    try {
      const r = await api.get<{ session: StudioSession | null }>(`${STUDIO_API}/session`, { skipAuthRedirect: true })
      session.value = r.session
    } catch {
      session.value = null
    }
    loaded.value = true
    return session.value
  }

  /** Runs a request; a 401 means the session is over: back to the sign-in. */
  async function call<T>(fn: () => Promise<T>): Promise<T> {
    try {
      return await fn()
    } catch (e) {
      if (isApiError(e) && e.status === 401 && session.value) {
        ended.value = e.message
        session.value = null
        await navigateTo(STUDIO_HOME)
      }
      throw e
    }
  }
  const opts = { skipAuthRedirect: true }
  const client = {
    get: <T>(path: string, background = false) => call(() => api.get<T>(path, { ...opts, background })),
    post: <T>(path: string, body: unknown = {}) => call(() => api.post<T>(path, body, opts)),
    del: <T>(path: string) => call(() => api.del<T>(path, opts))
  }

  const canRead = computed(() => !!session.value && !session.value.read_only)
  const canWrite = computed(() => !!session.value?.can_write)
  const dbBase = (db: string) => `${STUDIO_API}/db/${encodeURIComponent(db)}`
  const tableBase = (db: string, table: string) => `${dbBase(db)}/tables/${encodeURIComponent(table)}`
  const dbLink = (db: string) => `${STUDIO_HOME}/db/${encodeURIComponent(db)}`
  const tableLink = (db: string, table: string, tab?: string) =>
    `${dbLink(db)}/${encodeURIComponent(table)}${tab ? `?tab=${tab}` : ''}`

  async function logout() {
    try {
      await api.post(`${STUDIO_API}/logout`, {}, opts)
    } catch {
      // Gone already: the sign-in page is where this ends either way.
    }
    session.value = null
    await navigateTo(STUDIO_HOME)
  }

  return { session, loaded, ended, load, api: client, canRead, canWrite, dbBase, tableBase, dbLink, tableLink, logout }
}

/** The path is Quarry's, which the panel's sign-in does not guard. */
export const isStudioRoute = (path: string) => path === STUDIO_HOME || path.startsWith(`${STUDIO_HOME}/`)
