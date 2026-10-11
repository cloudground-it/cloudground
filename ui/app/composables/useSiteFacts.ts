import type { Backup, CertificateStatus, Site } from '~/types'

export interface SiteFacts {
  cert?: CertificateStatus | null
  /** The newest backup; null when the site has none. */
  backup?: Backup | null
}

/**
 * The certificate and the newest backup of each listed site, for the
 * dashboard and the sites table. The API has no summary endpoint for these,
 * so they are read per site — once per site, a few at a time — and kept
 * across page visits in shared state.
 */
export function useSiteFacts(sites: Ref<Site[] | null | undefined>, opts: { cert?: boolean, backup?: boolean } = { cert: true, backup: true }) {
  const api = useApi()
  const facts = useState<Record<number, SiteFacts>>('site-facts', () => ({}))
  const loading = new Set<string>()

  async function load(site: Site) {
    const key = `${site.id}`
    if (loading.has(key)) return
    loading.add(key)
    const [cert, backups] = await Promise.all([
      opts.cert ? api.get<CertificateStatus>(`/api/sites/${site.id}/certificate`).catch(() => null) : Promise.resolve(undefined),
      opts.backup ? api.get<Backup[]>(`/api/sites/${site.id}/backups`).catch(() => undefined) : Promise.resolve(undefined)
    ]).finally(() => loading.delete(key))
    facts.value = {
      ...facts.value,
      [site.id]: {
        ...facts.value[site.id],
        ...(cert !== undefined ? { cert } : {}),
        ...(backups !== undefined ? { backup: backups?.[0] ?? null } : {})
      }
    }
  }

  async function refresh() {
    const list = (sites.value ?? []).filter(s => s.status !== 'provisioning' && s.status !== 'deleting')
    for (let i = 0; i < list.length; i += 4) {
      await Promise.all(list.slice(i, i + 4).map(load))
    }
  }

  watch(() => (sites.value ?? []).map(s => `${s.id}:${s.status}`).join(','), () => void refresh(), { immediate: true })
  // One site's certificate or backups changed: reload that site's facts.
  useEvents().on((ev) => {
    const wanted = (opts.cert && ev.type === 'cert.changed') || (opts.backup && ev.type.startsWith('backup.'))
    const site = wanted && ev.site_id ? sites.value?.find(s => s.id === ev.site_id) : undefined
    if (site) void load(site)
  })
  return { facts, refresh }
}
