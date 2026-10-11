import type { RouteLocationRaw } from 'vue-router'

/** One tab of a TabBar: a link, with its icon and whether it is the page shown. */
export interface TabBarItem {
  label: string
  icon: string
  to: RouteLocationRaw
  active: boolean
  /** Replace the history entry instead of adding one (tabs kept in the query). */
  replace?: boolean
}
