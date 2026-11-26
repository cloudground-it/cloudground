import type { Site, SiteType } from '~/types'

export const SITE_TYPES: SiteType[] = ['wordpress', 'woocommerce', 'static', 'php', 'laravel', 'proxy']

// The software's own mark where there is one (app/assets/icons/brand,
// monochrome), else the CloudGround icon.
export const siteTypeIcon: Record<SiteType, string> = {
  wordpress: 'i-brand-wordpress',
  woocommerce: 'i-brand-woocommerce',
  static: 'i-cg-file-code',
  php: 'i-brand-php',
  laravel: 'i-brand-laravel',
  proxy: 'i-cg-network',
  node: 'i-brand-node',
  python: 'i-brand-python'
}

/** A Node.js or Python site: its app runs as a unit of its own (#28). */
export const isAppSite = (s: Pick<Site, 'type'>) => s.type === 'node' || s.type === 'python'

export const isWordPress = (s: Pick<Site, 'type'>) => s.type === 'wordpress' || s.type === 'woocommerce'

/** Production sites with their staging copy directly underneath. */
export function orderSites(sites: Site[]): { site: Site, staging: boolean }[] {
  const byProd = new Map<number, Site[]>()
  for (const s of sites) {
    if (s.staging_of) byProd.set(s.staging_of, [...(byProd.get(s.staging_of) ?? []), s])
  }
  const out: { site: Site, staging: boolean }[] = []
  for (const s of sites) {
    if (s.staging_of) continue
    out.push({ site: s, staging: false })
    for (const c of byProd.get(s.id) ?? []) out.push({ site: c, staging: true })
  }
  for (const s of sites) {
    if (s.staging_of && !sites.some(p => p.id === s.staging_of)) out.push({ site: s, staging: true })
  }
  return out
}

// The sites' icons (internal/api SPEC §3.23) as data: URLs, which the
// panel's CSP allows for images, kept ten minutes so that the list and the
// site's page show the same one without asking again. null: none.
const FAVICON_TTL = 10 * 60 * 1000
const favicons = new Map<number, { at: number, url: Promise<string | null> }>()

export function siteFavicon(id: number): Promise<string | null> {
  const now = Date.now()
  const kept = favicons.get(id)
  if (kept && now - kept.at < FAVICON_TTL) return kept.url
  const url = fetch(`/api/sites/${id}/favicon`, { credentials: 'same-origin' })
    .then(r => (r.status === 200 ? r.blob() : null))
    .then(b => (b && b.type.startsWith('image/') ? dataURL(b) : null))
    .catch(() => null)
  favicons.set(id, { at: now, url })
  return url
}

function dataURL(b: Blob): Promise<string | null> {
  return new Promise((resolve) => {
    const r = new FileReader()
    r.onload = () => resolve(typeof r.result === 'string' ? r.result : null)
    r.onerror = () => resolve(null)
    r.readAsDataURL(b)
  })
}
