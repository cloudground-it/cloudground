<script setup lang="ts">
import type { Alert, DriftEvent, MetricSample, ServiceInfo, Site, SystemStatus, UpdateStatus } from '~/types'
import type { ScopeSeries } from '~/types/scope'

const { t, locale } = useI18n()
const api = useApi()
const { isAdmin, isOperator } = useAuth()
const { busy, run } = useAction()
const { sitesVersion } = useShell()
useHead({ title: () => t('nav.dashboard') })

// Server-wide capacity is not an operator's: the API refuses it, so it is not asked for.
// Each panel reloads when an event says its data changed (useLive); the old
// intervals apply only while the event stream is down. The server status
// arrives whole in system.status, every 15 s while someone watches.
const { data: status, error: statusError } = useLive<SystemStatus | null>(() => isOperator.value ? Promise.resolve(null) : api.get<SystemStatus>('/api/system/status'), {
  on: { types: ['system.status'] },
  apply: (ev, d) => {
    d.value = ev.data as unknown as SystemStatus
    return true
  },
  fallbackMs: 15000
})
const { data: sites, refresh: refreshSites } = useLive(() => api.get<Site[]>('/api/sites'), { on: { types: ['site.'] }, fallbackMs: 15000 })
const { data: metrics } = useLive(() => isOperator.value ? Promise.resolve([] as MetricSample[]) : api.get<MetricSample[]>('/api/system/metrics'), {
  on: { types: ['metrics.sample'] },
  // Oldest first, as the API lists them; it keeps 288 (24 hours).
  apply: (ev, d) => {
    if (!d.value) return false
    d.value = [...d.value, ev.data as unknown as MetricSample].slice(-288)
    return true
  },
  fallbackMs: 60000
})
const { data: services } = useLive(() => isOperator.value ? Promise.resolve([] as ServiceInfo[]) : api.get<ServiceInfo[]>('/api/services'), { on: { types: ['service.changed'] }, fallbackMs: 15000 })
// Reading the drift list also scans for it: never on a timer while events flow.
const { data: drift, refresh: refreshDrift } = useLive(() => isOperator.value ? Promise.resolve([] as DriftEvent[]) : api.get<DriftEvent[]>('/api/system/drift'), { on: { types: ['drift.changed'] }, fallbackMs: 30000 })
const alerts = useState<Alert[]>('alerts:list', () => [])
const alertsOpen = useState<boolean>('alerts:open', () => false)
watch(sitesVersion, refreshSites)

// Checked when someone opens the panel, never on a timer: it is a request to
// the update server, and installing is the operator's decision.
const { data: update } = useLoad(() => api.get<UpdateStatus>('/api/system/update'))

const updatedAt = ref(Date.now())
watch([status, sites], () => { updatedAt.value = Date.now() })
const now = useNow({ scheduler: cb => useIntervalFn(cb, 30000) })
const updatedLabel = computed(() => now.value.getTime() - updatedAt.value < 60000 ? t('cg.dashboard.updatedNow') : t('cg.dashboard.updatedAgo', { when: fmtAgo(new Date(updatedAt.value).toISOString(), locale.value) }))

const errors = computed(() => alerts.value.filter(a => a.level === 'error').length)
const mood = computed(() => errors.value ? 'err' : alerts.value.length ? 'warn' : 'ok')
const activeSites = computed(() => (sites.value ?? []).filter(s => s.status === 'active').length)
const productionSites = computed(() => (sites.value ?? []).filter(s => !s.staging_of).length)
const stagingSites = computed(() => (sites.value ?? []).filter(s => s.staging_of).length)

const gb = (mb: number) => fmtBytes(mb * 1048576)
// Headroom: how much is still free. Below 35% it needs a look, below 15% action.
function state(free: number) {
  if (free < 15) return { label: t('cg.state.err'), tone: 'err' as const }
  if (free < 35) return { label: t('cg.state.warn'), tone: 'warn' as const }
  return { label: t('cg.state.ok'), tone: 'ok' as const }
}
const meters = computed(() => {
  const s = status.value
  if (!s) return []
  return [
    { key: 'cpu', label: t('cg.dashboard.cpuFree'), free: s.cpu_headroom_pct, note: t('cg.dashboard.cpuNote', { load: fmtNumber(s.load1, 2), cores: s.cpu_count }) },
    { key: 'mem', label: t('cg.dashboard.memFree'), free: s.mem_headroom_pct, note: t('cg.dashboard.inUse', { used: gb(s.mem_total_mb - s.mem_available_mb), total: gb(s.mem_total_mb) }) },
    { key: 'disk', label: t('cg.dashboard.diskFree'), free: s.disk_headroom_pct, note: t('cg.dashboard.inUse', { used: gb(s.disk_total_mb - s.disk_free_mb), total: gb(s.disk_total_mb) }) }
  ].map(m => ({ ...m, value: Math.round(m.free), state: state(m.free), bar: 100 - m.free, barTone: m.free < 15 ? 'err' as const : m.free < 35 ? 'warn' as const : 'blue' as const }))
})

// Samples arrive every five minutes; averaged into 48 half-hour buckets the
// trace shows the trend instead of every spike of a 2-core box.
const buckets = computed(() => {
  const s = metrics.value ?? []
  if (s.length < 2) return []
  const size = Math.max(1, Math.ceil(s.length / 48))
  const out: { cpu: number, mem: number, disk: number }[] = []
  for (let i = 0; i < s.length; i += size) {
    const c = s.slice(i, i + size)
    const avg = (k: 'cpu_headroom_pct' | 'mem_headroom_pct' | 'disk_headroom_pct') => 100 - c.reduce((n, x) => n + x[k], 0) / c.length
    out.push({ cpu: avg('cpu_headroom_pct'), mem: avg('mem_headroom_pct'), disk: avg('disk_headroom_pct') })
  }
  return out
})
const series = computed<ScopeSeries[]>(() => [
  { key: 'cpu', label: 'CPU', color: 'var(--cg-blue)', values: buckets.value.map(b => b.cpu) },
  { key: 'mem', label: t('dashboard.memory'), color: 'var(--cg-ink)', values: buckets.value.map(b => b.mem) },
  { key: 'disk', label: t('dashboard.disk'), color: 'var(--cg-muted)', values: buckets.value.map(b => b.disk), dashed: true }
])
const chartLabel = computed(() => {
  const last = buckets.value[buckets.value.length - 1]
  return last ? t('cg.dashboard.chartAria', { cpu: Math.round(last.cpu), mem: Math.round(last.mem), disk: Math.round(last.disk) }) : t('dashboard.capacityEmpty')
})
const ticks = computed(() => ['-24 h', '-18 h', '-12 h', '-6 h', t('dashboard.now')])

const listed = computed(() => orderSites(sites.value ?? []).slice(0, 8))
const { facts } = useSiteFacts(computed(() => listed.value.map(r => r.site)))

const serviceTone = { running: 'success', stopped: 'error', failed: 'error', disabled: 'neutral' } as const
const serviceLabel = (s: ServiceInfo) => s.name === 'site-php' ? t('services.sitePhp') : s.name

function resolveDrift(id: number, action: 'restore' | 'accept') {
  void run(() => api.post(`/api/system/drift/${id}/resolve`, { action }), action === 'restore' ? t('dashboard.driftRestored') : t('dashboard.driftAccepted')).then(refreshDrift)
}
const siteColumns = computed(() => [
  { id: 'site', header: t('cg.col.site') },
  { id: 'status', header: t('cg.col.status') },
  { id: 'type', header: t('cg.col.type'), meta: { class: { th: 'hidden sm:table-cell', td: 'hidden sm:table-cell' } } },
  { id: 'cert', header: t('cg.col.cert'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'backup', header: t('cg.col.backup'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } }
])
const updateShown = computed(() => !!update.value?.update_available && isAdmin.value)
const moodBadge = computed(() => ({ ok: 'success', warn: 'warning', err: 'error' } as const)[mood.value])

function startUpdate() {
  void run(() => api.post('/api/panel/update', { version: update.value?.latest }), t('dashboard.updateStarted'))
}
</script>

<template>
  <UDashboardPanel id="dashboard">
    <template #header>
      <PageNavbar :title="t('nav.dashboard')" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.dashboard')">
          <template #description>
            <span class="flex flex-wrap items-center gap-x-2.5 gap-y-1">
              <UBadge :color="moodBadge" variant="subtle" :label="`${t(`cg.dashboard.title.${mood}`)} ${t(`cg.dashboard.accent.${mood}`)}`" />
              <span>{{ t('cg.dashboard.sitesActive', activeSites) }} · {{ t('cg.dashboard.alertsToRead', alerts.length) }} · {{ updatedLabel }}</span>
            </span>
          </template>
          <template v-if="isAdmin" #links>
            <!-- The update, when there is one, is the screen's one primary action. -->
            <UButton v-bind="updateShown ? {} : { color: 'primary', variant: 'solid' }" icon="i-cg-plus" :label="t('site.new')" to="/sites/new" />
          </template>
        </UPageHeader>

        <UAlert v-if="statusError" color="error" variant="subtle" icon="i-cg-unlink" :title="t('dashboard.agentDown')" :description="statusError" />

        <UAlert
          v-if="update?.update_available && isAdmin"
          color="info"
          variant="subtle"
          icon="i-cg-download"
          :title="t('dashboard.updateAvailable', { version: update.latest })"
          :description="t('dashboard.updateRunning', { version: update.current })"
          :actions="[
            ...(update.notes_url ? [{ label: t('dashboard.whatsNew'), to: update.notes_url, target: '_blank', color: 'neutral' as const, variant: 'outline' as const, size: 'sm' as const }] : []),
            { label: t('dashboard.updateNow'), loading: busy, color: 'primary' as const, variant: 'solid' as const, size: 'sm' as const, onClick: startUpdate }
          ]"
        />

        <section :aria-label="t('cg.dashboard.capacity')" class="cg-grid grid-cols-2 lg:grid-cols-4">
          <template v-if="status">
            <StatCell v-for="m in meters" :key="m.key" :label="m.label" :value="m.value" unit="%" :note="m.note" :state="m.state" :bar="m.bar" :bar-tone="m.barTone" />
          </template>
          <template v-else-if="!isOperator && !statusError">
            <USkeleton v-for="i in 3" :key="i" class="h-32" />
          </template>
          <StatCell
            to="/sites"
            :label="t('nav.sites')"
            :value="sites ? productionSites : '—'"
            :unit="t('dashboard.production')"
            :note="t('cg.dashboard.sitesNote', { staging: stagingSites, active: activeSites })"
          />
        </section>

        <PanelSection :title="t('alerts.title')" flush>
          <template v-if="alerts.length" #actions>
            <UButton color="neutral" variant="outline" size="sm" :label="t('cg.common.all')" trailing-icon="i-cg-arrow-right" @click="alertsOpen = true" />
          </template>
          <AlertList :alerts="alerts" :limit="4" inset @dismissed="id => alerts = alerts.filter(a => a.id !== id)" />
        </PanelSection>

        <PanelSection :title="t('nav.sites')" flush>
          <template #actions>
            <UButton color="neutral" variant="outline" size="sm" :label="t('cg.dashboard.allSites')" trailing-icon="i-cg-arrow-right" to="/sites" />
          </template>
          <USkeleton v-if="!sites" class="m-4 h-12" />
          <UEmpty
            v-else-if="!sites.length"
            size="sm"
            icon="i-cg-sites"
            :title="t('site.emptyTitle')"
            :description="t('site.emptyHint')"
            :actions="isAdmin ? [{ label: t('site.createFirst'), icon: 'i-cg-plus', to: '/sites/new' }] : []"
          />
          <UTable v-else :data="listed" :columns="siteColumns">
            <template #site-cell="{ row: { original: { site, staging } } }">
              <NuxtLink :to="`/sites/${site.id}`" class="break-all text-highlighted decoration-1 underline-offset-[0.2em] hover:text-cg-blue hover:underline" :class="staging ? 'ps-4 font-medium' : 'font-semibold'">
                <span v-if="staging" class="text-dimmed" aria-hidden="true">↳ </span>{{ site.domain }}
              </NuxtLink>
            </template>
            <template #status-cell="{ row: { original: { site } } }">
              <SiteStatusBadge :status="site.status" :staging="!!site.staging_of" />
            </template>
            <template #type-cell="{ row: { original: { site } } }">
              <span class="text-muted">{{ site.staging_of ? 'Staging' : t(`site.type.${site.type}`) }}</span>
            </template>
            <template #cert-cell="{ row: { original: { site } } }">
              <SiteFactCells :site="site" :facts="facts[site.id]" show="cert" />
            </template>
            <template #backup-cell="{ row: { original: { site } } }">
              <SiteFactCells :site="site" :facts="facts[site.id]" show="backup" />
            </template>
          </UTable>
        </PanelSection>

        <div class="grid gap-6 lg:grid-cols-2">
          <PanelSection v-if="!isOperator" :title="t('cg.dashboard.last24')">
            <ScopeChart :series="series" :ticks="ticks" :label="chartLabel" :height="220">
              <p v-if="buckets.length < 2" class="absolute inset-0 m-0 grid place-items-center px-6 text-center text-sm text-muted">
                {{ t('dashboard.capacityEmpty') }}
              </p>
            </ScopeChart>
          </PanelSection>

          <PanelSection v-if="!isOperator" :title="t('nav.services')" flush>
            <template #actions>
              <UButton color="neutral" variant="outline" size="sm" :label="t('common.manage')" trailing-icon="i-cg-arrow-right" to="/services" />
            </template>
            <ul v-if="services" class="m-0 list-none divide-y divide-default p-0">
              <li v-for="s in services" :key="s.name" class="flex items-center justify-between gap-3 px-4 py-3 sm:px-5">
                <span class="mono truncate text-[13px] text-highlighted">{{ serviceLabel(s) }}</span>
                <UBadge :color="serviceTone[s.state] ?? 'neutral'" variant="subtle" :label="t(`services.state.${s.state}`)" />
              </li>
            </ul>
            <USkeleton v-else class="m-4 h-24" />
          </PanelSection>

          <PanelSection :title="t('tasks.recent')" flush :class="isOperator ? '' : 'lg:col-span-2'">
            <TaskFeed />
          </PanelSection>
        </div>

        <PanelSection v-if="drift?.length" :title="t('dashboard.drift')" :description="t('dashboard.driftHint')" flush>
          <ul class="m-0 list-none divide-y divide-default p-0">
            <li v-for="d in drift" :key="d.id" class="flex flex-wrap items-center gap-3 px-4 py-3 sm:px-5">
              <UBadge color="warning" variant="subtle" :label="t('cg.state.changed')" />
              <span class="mono min-w-0 flex-1 text-[13px] break-all text-highlighted">{{ d.path }}</span>
              <span class="flex gap-2">
                <UButton size="sm" :label="t('dashboard.restore')" :disabled="busy || !isAdmin" @click="resolveDrift(d.id, 'restore')" />
                <UButton size="sm" color="neutral" variant="outline" :label="t('dashboard.keepChange')" :disabled="busy || !isAdmin" @click="resolveDrift(d.id, 'accept')" />
              </span>
            </li>
          </ul>
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
