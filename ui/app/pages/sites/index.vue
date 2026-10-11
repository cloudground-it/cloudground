<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Site } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { isAdmin, canManage } = useAuth()
const { run } = useAction()
const confirm = useConfirm()
const { creating, sitesVersion } = useShell()
useHead({ title: () => t('nav.sites') })

const { data: sites, error, refresh } = useLive(() => api.get<Site[]>('/api/sites'), { on: { types: ['site.'] }, fallbackMs: 8000 })
watch(sitesVersion, refresh)

type Kind = 'all' | 'wp' | 'woo' | 'other'
const kindOf = (s: Site): Exclude<Kind, 'all'> => s.type === 'wordpress' ? 'wp' : s.type === 'woocommerce' ? 'woo' : 'other'
const filter = ref<Kind>('all')
const q = ref('')
const filters = computed(() => (['all', 'wp', 'woo', 'other'] as Kind[]).map(id => ({
  id,
  label: t(`cg.sites.${id}`),
  n: (sites.value ?? []).filter(s => id === 'all' || kindOf(s) === id).length
})))
const rows = computed(() => {
  const needle = q.value.trim().toLowerCase()
  return orderSites(sites.value ?? []).filter(({ site }) =>
    (filter.value === 'all' || kindOf(site) === filter.value) && (!needle || site.domain.includes(needle)))
})
const { facts } = useSiteFacts(computed(() => rows.value.map(r => r.site)), { cert: false, backup: true })

function sub(s: Site): string {
  const all = sites.value ?? []
  if (s.status === 'error' && s.type === 'proxy' && s.config.proxy_upstream) return t('cg.sites.upstream', { u: s.config.proxy_upstream })
  if (s.staging_of) return t('cg.sites.stagingOf', { d: all.find(p => p.id === s.staging_of)?.domain ?? `#${s.staging_of}` })
  if (all.some(x => x.staging_of === s.id)) return t('cg.sites.stagingLinked')
  if (s.aliases?.length) return s.aliases.length === 1 ? t('cg.sites.alias', { a: s.aliases[0] }) : t('cg.sites.aliases', { n: s.aliases.length })
  if (s.type === 'proxy' && s.config.proxy_upstream) return t('cg.sites.upstream', { u: s.config.proxy_upstream })
  return fmtDateTime(s.created_at, locale.value)
}

function actions(s: Site): DropdownMenuItem[][] {
  const groups: DropdownMenuItem[][] = [[
    { label: t('alerts.open'), icon: 'i-cg-arrow-right', to: `/sites/${s.id}` },
    { label: t('site.visit'), icon: 'i-cg-external-link', to: `https://${s.domain}`, target: '_blank' }
  ]]
  if (canManage.value && s.status === 'active') {
    groups.push([
      { label: t('cg.site.actions.purge'), icon: 'i-cg-purge', onSelect: () => void run(() => api.post(`/api/sites/${s.id}/purge`), t('cache.purgedAll')) },
      { label: t('cg.site.actions.backup'), icon: 'i-cg-backup', onSelect: () => void run(() => api.post(`/api/sites/${s.id}/backups`), t('backup.started')) }
    ])
  }
  if (isAdmin.value) {
    groups.push([{ label: t('common.delete'), icon: 'i-cg-trash', color: 'error', onSelect: () => void remove(s) }])
  }
  return groups
}
async function remove(s: Site) {
  const ok = await confirm({ title: t('site.deleteTitle', { domain: s.domain }), description: t('site.deleteHint'), danger: true, confirmLabel: t('common.delete'), typed: s.domain })
  if (ok && await run(() => api.del(`/api/sites/${s.id}`), t('site.deleting'))) await refresh()
}

const filterTabs = computed(() => filters.value.map(f => ({ label: f.label, value: f.id, badge: { label: String(f.n), color: 'neutral' as const, variant: 'subtle' as const } })))
const columns = computed(() => [
  { id: 'site', header: t('cg.col.site') },
  { id: 'status', header: t('cg.col.status') },
  { id: 'type', header: t('cg.col.type'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'php', header: t('cg.col.php'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { id: 'profile', header: t('cg.col.profile'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { id: 'backup', header: t('cg.col.backup'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <UDashboardPanel id="sites">
    <template #header>
      <PageNavbar :title="t('cg.dashboard.allSites')" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.sites')" :description="t('ux.pages.sites', sites?.length ?? 0)">
          <template v-if="isAdmin" #links>
            <UButton icon="i-cg-plus" :label="t('site.new')" @click="creating = true" />
          </template>
        </UPageHeader>

        <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />

        <div v-if="!sites && !error" class="flex flex-col gap-2">
          <USkeleton v-for="i in 4" :key="i" class="h-14" />
        </div>

        <PanelSection v-else-if="sites && !sites.length">
          <UEmpty
            icon="i-cg-sites"
            :title="t('site.emptyTitle')"
            :description="t('site.emptyHint')"
            :actions="isAdmin ? [{ label: t('site.createFirst'), icon: 'i-cg-plus', onClick: () => { creating = true } }] : []"
          />
        </PanelSection>

        <PanelSection v-else-if="sites" flush :label="t('nav.sites')">
          <template #toolbar>
            <div class="flex flex-wrap items-center gap-3">
              <div class="scrollbar-none max-w-full overflow-x-auto overflow-y-hidden">
                <UTabs v-model="filter" :items="filterTabs" :content="false" variant="pill" size="sm" class="w-max" :aria-label="t('cg.sites.filterLabel')" />
              </div>
              <UInput v-model="q" type="search" icon="i-cg-search" :placeholder="t('cg.sites.search')" :aria-label="t('cg.sites.search')" class="min-w-0 flex-[1_1_14rem]" />
            </div>
          </template>
          <UTable :data="rows" :columns="columns" :empty="t('cg.sites.noMatch')">
            <template #site-cell="{ row: { original: { site, staging } } }">
              <span class="flex min-w-0 flex-col gap-0.5 whitespace-normal" :class="staging ? 'ps-4' : ''">
                <NuxtLink :to="`/sites/${site.id}`" class="text-[15px] font-semibold break-all text-highlighted hover:underline">
                  <span v-if="staging" class="text-dimmed" aria-hidden="true">↳ </span>{{ site.domain }}
                </NuxtLink>
                <span class="text-xs break-all text-muted">{{ sub(site) }}</span>
                <span class="text-xs text-muted md:hidden">{{ site.staging_of ? 'Staging' : t(`site.type.${site.type}`) }}<template v-if="site.php_version"> · PHP {{ site.php_version }}</template></span>
              </span>
            </template>
            <template #status-cell="{ row: { original: { site } } }">
              <SiteStatusBadge :status="site.status" :staging="!!site.staging_of" />
            </template>
            <template #type-cell="{ row: { original: { site } } }">
              {{ site.staging_of ? 'Staging' : t(`site.type.${site.type}`) }}
            </template>
            <template #php-cell="{ row: { original: { site } } }">
              <span class="mono text-[13px] text-muted">{{ site.php_version || '—' }}</span>
            </template>
            <template #profile-cell="{ row: { original: { site } } }">
              <span class="text-muted">{{ site.type === 'proxy' && !site.config.cache_enabled ? '—' : t(`site.profile.${site.profile}`) }}</span>
            </template>
            <template #backup-cell="{ row: { original: { site } } }">
              <SiteFactCells :site="site" :facts="facts[site.id]" show="backup" />
            </template>
            <template #actions-cell="{ row: { original: { site } } }">
              <UDropdownMenu :items="actions(site)" :content="{ align: 'end' }">
                <UButton icon="i-cg-more-horizontal" color="neutral" variant="ghost" size="sm" square :aria-label="t('cg.sites.actionsFor', { d: site.domain })" />
              </UDropdownMenu>
            </template>
          </UTable>
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
