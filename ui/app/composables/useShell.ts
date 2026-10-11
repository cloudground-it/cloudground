import type { Site } from '~/types'

/**
 * State the panel's frame shares with its pages: the site list behind the
 * sidebar count and the search palette, the "new site" dialog the top bar
 * opens from any page, and the panel's own version.
 */
export function useShell() {
  const sites = useState<Site[] | null>('shell:sites', () => null)
  const creating = useState<boolean>('shell:creating', () => false)
  // Bumped when a site is created, so a page listing sites reloads at once.
  const sitesVersion = useState<number>('shell:sites-version', () => 0)
  const version = useState<string>('shell:version', () => '')
  return { sites, creating, sitesVersion, version }
}
