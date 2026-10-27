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

// Ubuntu's own updates (#27): the agent lists them, security first, and
// upgrades them; the panel only says when a reboot is needed.
interface PendingUpdate { name: string, from?: string, to: string, origin?: string, security?: boolean, new?: boolean }
interface SystemUpdates { packages: PendingUpdate[], security: number, reboot_required: boolean, reboot_packages?: string[], lists_updated_at?: string, checked_at: string }
const { data: updates, error: updatesError, refresh: refreshUpdates } = useLoad(() => api.get<SystemUpdates>('/api/system/updates'))
async function refreshLists() {
  if (await run(() => api.post('/api/system/updates/refresh'), t('osupdates.refreshStarted'))) await refreshUpdates()
}
async function upgrade(securityOnly: boolean) {
  const ok = await confirm({
    title: securityOnly ? t('osupdates.securityTitle', { n: updates.value?.security ?? 0 }) : t('osupdates.allTitle', { n: updates.value?.packages.length ?? 0 }),
    description: t('osupdates.applyHint'),
    confirmLabel: securityOnly ? t('osupdates.applySecurity') : t('osupdates.applyAll')
  })
  if (ok) await run(() => api.post('/api/system/updates/apply', { security_only: securityOnly }), t('osupdates.applyStarted'))
}
const updateColumns = computed<TableColumn<PendingUpdate>[]>(() => [
  { accessorKey: 'name', header: t('osupdates.col.package') },
  { accessorKey: 'from', header: t('osupdates.col.installed'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { accessorKey: 'to', header: t('osupdates.col.candidate') },
  { accessorKey: 'origin', header: t('osupdates.col.origin'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } }
])

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

        <PanelSection
          id="updates"
          flush
          :title="t('osupdates.title')"
          :description="updates ? (updates.packages.length ? t('osupdates.pending', { n: updates.packages.length, security: updates.security }) : t('osupdates.none')) : t('osupdates.hint')"
        >
          <template v-if="isAdmin" #actions>
            <UButton color="neutral" variant="outline" icon="i-cg-refresh" :label="t('osupdates.refresh')" :disabled="busy" @click="refreshLists" />
            <UButton v-if="updates?.security" color="neutral" variant="outline" :label="t('osupdates.applySecurity')" :disabled="busy" @click="upgrade(true)" />
            <UButton v-if="updates?.packages.length" icon="i-cg-download" :label="t('osupdates.applyAll')" :disabled="busy" @click="upgrade(false)" />
          </template>
          <div v-if="updates?.reboot_required || updatesError" class="px-4 pt-4">
            <UAlert v-if="updatesError" color="error" variant="subtle" icon="i-cg-alert-circle" :title="updatesError" />
            <UAlert
              v-else
              color="warning"
              variant="subtle"
              icon="i-cg-refresh"
              :title="t('osupdates.rebootNeeded')"
              :description="updates?.reboot_packages?.length ? t('osupdates.rebootFor', { list: updates.reboot_packages.join(', ') }) : t('osupdates.rebootHint')"
            />
          </div>
          <UTable :data="updates?.packages ?? []" :columns="updateColumns" :empty="t('osupdates.none')">
            <template #name-cell="{ row }">
              <span class="flex items-center gap-2">
                <span class="mono text-[13px] font-semibold text-highlighted">{{ row.original.name }}</span>
                <UBadge v-if="row.original.security" color="error" variant="subtle" :label="t('osupdates.security')" />
                <UBadge v-if="row.original.new" color="neutral" variant="subtle" :label="t('osupdates.new')" />
              </span>
            </template>
            <template #from-cell="{ row }">
              <span class="mono text-[13px] text-muted">{{ row.original.from || '—' }}</span>
            </template>
            <template #to-cell="{ row }">
              <span class="mono text-[13px]">{{ row.original.to }}</span>
            </template>
            <template #origin-cell="{ row }">
              <span class="text-xs text-muted">{{ row.original.origin }}</span>
            </template>
          </UTable>
          <p v-if="updates?.lists_updated_at" class="m-0 px-4 pb-4 text-xs text-muted">
            {{ t('osupdates.listsUpdated', { ago: fmtAgo(updates.lists_updated_at, locale) }) }}
          </p>
        </PanelSection>

        <ServerRuntimes v-if="isAdmin" />
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
