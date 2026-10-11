import type { InjectionKey, Ref } from 'vue'
import type { Site } from '~/types'

export interface CurrentSite {
  site: Ref<Site>
  /** Production's staging copy, when there is one. */
  staging: Ref<Site | null>
  refresh: () => Promise<void>
}

export const currentSiteKey: InjectionKey<CurrentSite> = Symbol('current-site')

/** The site whose detail page is open (provided by pages/sites/[id].vue). */
export function useCurrentSite(): CurrentSite {
  const ctx = inject(currentSiteKey)
  if (!ctx) throw new Error('useCurrentSite outside a site page')
  return ctx
}
