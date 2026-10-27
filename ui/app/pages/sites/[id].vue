<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Site } from '~/types'
import type { PanelEvent } from '~/composables/useEvents'

const { t } = useI18n()
const api = useApi()
const route = useRoute()
const { isAdmin, canManage } = useAuth()
const confirm = useConfirm()
const { busy, run } = useAction()

const id = computed(() => Number(route.params.id))
const site = ref<Site | null>(null)
const staging = ref<Site | null>(null)
const loadError = ref<string | null>(null)

async function refresh() {
  try {
    const [s, all] = await Promise.all([api.get<Site>(`/api/sites/${id.value}`), api.get<Site[]>('/api/sites')])
    site.value = s
    staging.value = all.find(x => x.staging_of === s.id) ?? null
    loadError.value = null
  } catch (e) {
    // Leave only when the site is really gone; a transient error while
    // polling must not throw someone out of a form.
    if (isApiError(e) && e.status === 404) await navigateTo('/sites')
    else loadError.value = errorMessage(e)
  }
}
await refresh()
watch(id, refresh)

// Reload when this site or its staging copy changes (a status, an applied
// configuration, a staging copy made or removed); every 10 s only while the
// event stream is down, as before it existed.
const events = useEvents()
// Reloads the panel makes on its own are background requests (they do not
// keep the session alive); the first load and the one after an action are not.
const refreshQuietly = () => inBackground(refresh)
const refreshSoon = useDebounceFn(refreshQuietly, 250)
const ours = { types: ['site.'], where: (ev: PanelEvent) => ev.site_id === id.value || ev.site_id === staging.value?.id || ev.data?.staging_of === id.value }
events.on((ev) => {
  if (matchesEvent(ours, ev)) void refreshSoon()
})
events.onResync(() => void refreshSoon())
useIntervalFn(refreshQuietly, computed(() => events.down.value ? 10000 : 5 * 60_000))

provide(currentSiteKey, { site: site as Ref<Site>, staging, refresh })
useHead({ title: () => site.value?.domain ?? t('nav.sites') })

// The tab bar: every tab has an icon. The less frequent tools go under one
// menu so the bar fits a laptop; on a phone the bar scrolls.
const base = computed(() => `/sites/${id.value}`)
interface Tab { label: string, icon: string, to: string, exact?: boolean }
const links = computed(() => {
  const s = site.value
  if (!s) return { items: [] as Tab[], more: [] as Tab[] }
  const items: Tab[] = [
    { label: t('tabs.overview'), icon: 'i-cg-dashboard', to: base.value, exact: true },
    { label: t('tabs.cache'), icon: 'i-cg-cache', to: `${base.value}/cache` }
  ]
  // A Node.js or Python site's app: its health, settings and environment (#28).
  if (isAppSite(s)) items.splice(1, 1, { label: t('tabs.app'), icon: s.type === 'node' ? 'i-brand-node' : 'i-brand-python', to: `${base.value}/app` })
  if (isWordPress(s)) items.push({ label: 'WordPress', icon: 'i-cg-plug', to: `${base.value}/wordpress` })
  // The malware scan covers every site with files; the vulnerability check WordPress.
  if (s.type !== 'proxy') items.push({ label: t('tabs.security'), icon: 'i-cg-shield-check', to: `${base.value}/security` })
  if (!s.staging_of) items.push({ label: 'Staging', icon: 'i-cg-staging', to: `${base.value}/staging` })
  items.push(
    { label: t('tabs.deployments'), icon: 'i-cg-release', to: `${base.value}/deployments` },
    { label: t('tabs.backups'), icon: 'i-cg-backup', to: `${base.value}/backups` },
    { label: 'SSL', icon: 'i-cg-certificate', to: `${base.value}/ssl` },
    // Every site may have databases: a static or plain PHP one starts with none.
    { label: t('tabs.database'), icon: 'i-cg-database', to: `${base.value}/database` },
    { label: t('tabs.files'), icon: 'i-cg-folder', to: `${base.value}/files` }
  )
  const more: Tab[] = [{ label: 'Cron', icon: 'i-cg-cron', to: `${base.value}/cron` }]
  if (s.php_version) more.push({ label: 'PHP', icon: 'i-cg-code', to: `${base.value}/php` })
  // mail() of a PHP site goes through the panel's relay (#77).
  if (s.php_version) more.push({ label: t('tabs.mail'), icon: 'i-cg-bell', to: `${base.value}/mail` })
  more.push({ label: t('tabs.resources'), icon: 'i-cg-gauge', to: `${base.value}/resources` })
  // Visitor statistics from the access log (#33); a proxy site is logged too.
  more.push({ label: t('tabs.traffic'), icon: 'i-cg-chart-bar', to: `${base.value}/traffic` })
  more.push(
    { label: t('tabs.sftp'), icon: 'i-cg-key', to: `${base.value}/sftp` },
    // A staging copy asks for its own password everywhere already.
    ...(s.staging_of ? [] : [{ label: t('tabs.protection'), icon: 'i-cg-lock', to: `${base.value}/protection` }]),
    // Its names are not the original's, and it has no redirects of its own (#83).
    ...(s.staging_of ? [] : [{ label: t('tabs.redirects'), icon: 'i-cg-external-link', to: `${base.value}/redirects` }]),
    // The routing block is nginx configuration: an administrator's (R-5.4).
    ...(isAdmin.value ? [{ label: t('tabs.vhost'), icon: 'i-cg-file-code', to: `${base.value}/vhost` }] : []),
    { label: t('tabs.migration'), icon: 'i-cg-import', to: `${base.value}/migration` },
    { label: t('tabs.logs'), icon: 'i-cg-logs', to: `${base.value}/logs` }
  )
  return { items, more }
})
const isActive = (l: Tab) => l.exact ? route.path === l.to : route.path === l.to || route.path.startsWith(l.to + '/')
const tabItems = computed(() => links.value.items.map(l => ({ label: l.label, icon: l.icon, to: l.to, active: isActive(l) })))
const moreItems = computed(() => links.value.more.map(l => ({ label: l.label, icon: l.icon, to: l.to, active: isActive(l) })))

// The line under the domain: what the site is.
const meta = computed(() => {
  const s = site.value
  if (!s) return ''
  const parts = [s.staging_of ? `Staging · ${t(`site.type.${s.type}`)}` : t(`site.type.${s.type}`)]
  if (s.php_version) parts.push(`PHP ${s.php_version}`)
  if (s.config.cache_enabled) parts.push(t('cg.site.profile', { p: t(`site.profile.${s.profile}`).toLowerCase() }))
  return parts.join(' · ')
})

function wpLogin() {
  // Open the tab inside the click so it is not blocked, then point it at the
  // one-time login URL once the request returns.
  const tab = window.open('about:blank', '_blank')
  // The site's pages must not be able to navigate the panel's tab.
  if (tab) tab.opener = null
  void run(async () => {
    const r = await api.post<{ url: string }>(`/api/sites/${id.value}/wp/login`)
    if (tab) tab.location.href = r.url
    else window.location.href = r.url
  }).then((ok) => {
    if (!ok) tab?.close()
  })
}

async function remove() {
  const s = site.value
  if (!s) return
  const ok = await confirm({ title: t('site.deleteTitle', { domain: s.domain }), description: t('site.deleteHint'), danger: true, confirmLabel: t('common.delete'), typed: s.domain })
  if (ok && await run(() => api.del(`/api/sites/${s.id}`), t('site.deleting'))) await navigateTo('/sites')
}
const menu = computed<DropdownMenuItem[][]>(() => [[{ label: t('common.delete'), icon: 'i-cg-trash', color: 'error', onSelect: remove }]])
</script>

<template>
  <UDashboardPanel id="site">
    <template #header>
      <PageNavbar :title="site?.domain ?? ''" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader v-if="site" :description="meta">
          <template #title>
            <span class="min-w-0">{{ site.domain }}</span>
            <SiteStatusBadge :status="site.status" :staging="!!site.staging_of" size="lg" />
          </template>
          <template #links>
            <UButton :to="`https://${site.domain}`" target="_blank" color="neutral" variant="outline" trailing-icon="i-cg-external-link" :label="t('site.visit')" :aria-label="t('site.visit')" :ui="{ label: 'hidden sm:inline' }" />
            <UButton v-if="isWordPress(site) && site.status === 'active'" color="primary" variant="solid" icon="i-cg-login" :label="t('cg.site.wpLogin')" :disabled="!canManage" :loading="busy" @click="wpLogin" />
            <UDropdownMenu v-if="isAdmin" :items="menu" :content="{ align: 'end' }">
              <UButton icon="i-cg-more-horizontal" color="neutral" variant="outline" square :aria-label="t('common.more')" />
            </UDropdownMenu>
          </template>
        </UPageHeader>

        <TabBar v-if="site" :items="tabItems" :more="moreItems" :label="t('cg.site.sections')" />

        <UAlert v-if="loadError" color="error" variant="subtle" icon="i-cg-alert-circle" :title="loadError" />
        <NuxtPage v-if="site" />
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
