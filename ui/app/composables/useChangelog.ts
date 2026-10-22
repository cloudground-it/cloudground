/** What `GET /api/changelog` answers (internal/api SPEC §2.9). */
export interface Changelog {
  current: string
  seen: string
  unseen: string[]
  entries: { version: string, body: string }[]
}

/** The changelog shipped in the panel, and which releases the account has not seen. */
export function useChangelog() {
  const api = useApi()
  const { user } = useAuth()
  const log = useState<Changelog | null>('changelog', () => null)

  async function load(): Promise<Changelog | null> {
    try {
      log.value = await api.get<Changelog>('/api/changelog')
    } catch {
      log.value = null
    }
    return log.value
  }

  /** Records the running release as seen by the signed-in account. */
  async function markSeen() {
    const res = await api.post<{ seen: string }>('/api/account/whats-new')
    if (user.value) user.value = { ...user.value, whats_new_seen: res.seen }
    if (log.value) log.value = { ...log.value, seen: res.seen, unseen: [] }
  }

  return { log, load, markSeen }
}
