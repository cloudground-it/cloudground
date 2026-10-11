import type { Site, SiteType } from '~/types'

export const SITE_TYPES: SiteType[] = ['wordpress', 'woocommerce', 'static', 'php', 'laravel', 'proxy']

export const siteTypeIcon: Record<SiteType, string> = {
  wordpress: 'i-cg-browser',
  woocommerce: 'i-cg-package',
  static: 'i-cg-file-code',
  php: 'i-cg-code',
  laravel: 'i-cg-layers',
  proxy: 'i-cg-network'
}

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
