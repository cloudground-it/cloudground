<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { ServiceInfo } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
useHead({ title: () => t('nav.services') })

const { data: services, error, pending, refresh } = useLive(() => api.get<ServiceInfo[]>('/api/services'), { on: { types: ['service.changed', 'system.status'] }, fallbackMs: 10000 })
const color = { running: 'success', stopped: 'error', failed: 'error', disabled: 'neutral' } as const
// Which services can be restarted is the agent's answer.
const caps = useCapabilities()
const restartable = (name: string) => !!caps.value?.services.find(s => s.name === name)?.restartable

function icon(name: string) {
  if (name === 'nginx') return 'i-cg-sites'
  if (name.includes('php')) return 'i-cg-code'
  if (name === 'mariadb') return 'i-cg-database'
  if (name === 'redis') return 'i-cg-cache'
  return 'i-cg-server'
}
// The agent reports site PHP as "N running[, M not running: …]".
function detailText(d: string) {
  return d.replace(/^(\d+) running/, (_, n) => t('services.running', { n })).replace(/(\d+) not running/, (_, n) => t('services.notRunning', { n }))
}
const label = (s: ServiceInfo) => s.name === 'site-php' ? t('services.sitePhp') : s.name

async function restart(s: ServiceInfo) {
  if (await confirm({ title: t('services.restartTitle', { name: label(s) }), description: t('services.restartHint'), confirmLabel: t('services.restart') })) {
    await run(() => api.post(`/api/services/${s.name}/restart`), t('services.restarted', { name: label(s) }))
    await refresh()
  }
}

const columns = computed<TableColumn<ServiceInfo>[]>(() => [
  { accessorKey: 'name', header: t('services.col.service') },
  { accessorKey: 'state', header: t('services.col.state') },
  { accessorKey: 'enabled', header: t('services.col.boot'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { accessorKey: 'memory_bytes', header: t('services.col.memory'), meta: { class: { th: 'hidden sm:table-cell text-right', td: 'hidden sm:table-cell text-right' } } },
  { accessorKey: 'since', header: t('services.col.since'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { accessorKey: 'version', header: t('services.col.version'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-right' } } }
])
</script>

<template>
  <UDashboardPanel id="services">
    <template #header>
      <PageNavbar :title="t('nav.services')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.services')" :description="t('services.hint')" />
        <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />
        <PanelSection flush :label="t('nav.services')">
          <UTable
            :data="services ?? []"
            :columns="columns"
            :loading="pending && !services"
          >
            <template #name-cell="{ row }">
              <div class="flex items-center gap-3">
                <UIcon :name="icon(row.original.name)" class="size-4 shrink-0 text-muted" />
                <div class="min-w-0">
                  <p class="font-semibold text-highlighted">
                    {{ label(row.original) }}
                  </p>
                  <p class="mono truncate text-xs text-dimmed">
                    {{ row.original.unit }}
                  </p>
                </div>
              </div>
            </template>
            <template #state-cell="{ row }">
              <div class="flex flex-col items-start gap-1">
                <UBadge :color="color[row.original.state] ?? 'neutral'" variant="subtle" :label="t(`services.state.${row.original.state}`)" />
                <span v-if="row.original.detail && !['active', 'inactive', 'failed'].includes(row.original.detail)" class="text-xs text-muted">{{ detailText(row.original.detail) }}</span>
              </div>
            </template>
            <template #enabled-cell="{ row }">
              <span class="text-sm" :class="row.original.enabled ? 'text-default' : 'text-muted'">{{ row.original.enabled ? t('services.bootYes') : t('services.bootNo') }}</span>
            </template>
            <template #memory_bytes-cell="{ row }">
              <span class="mono text-[13px] text-muted">{{ row.original.memory_bytes ? fmtBytes(row.original.memory_bytes) : '—' }}</span>
            </template>
            <template #since-cell="{ row }">
              <span class="text-sm text-muted" :title="fmtDateTime(row.original.since, locale)">{{ row.original.since ? fmtAgo(row.original.since, locale) : '—' }}</span>
            </template>
            <template #version-cell="{ row }">
              <span class="mono text-[13px] text-muted">{{ row.original.version || '—' }}</span>
            </template>
            <template #actions-cell="{ row }">
              <template v-if="isAdmin && restartable(row.original.name) && row.original.state !== 'disabled'">
                <UButton size="sm" color="neutral" variant="outline" icon="i-cg-refresh" :label="t('services.restart')" :aria-label="t('services.restart')" :disabled="busy" :ui="{ label: 'hidden sm:inline' }" @click="restart(row.original)" />
              </template>
            </template>
          </UTable>
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
