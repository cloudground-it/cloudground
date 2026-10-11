<script setup lang="ts">
/**
 * The site's resources (#18, #19): what its slice uses, over a day of
 * five-minute samples or 30 days of hourly averages (GET
 * /api/sites/{id}/usage), the limits the kernel holds it to, and the
 * thresholds over which it is an alert. The limits are an administrator's;
 * the thresholds anyone who manages the site may set.
 */
import type { ScopeSeries } from '~/types/scope'

interface UsageSample { sampled_at: string, cpu_cores: number, mem_bytes: number, mem_max_bytes: number, io_read_bps: number, io_write_bps: number, tasks: number, oom_kills: number, disk_bytes: number, disk_inodes: number }
interface UsagePoint { at: string, cpu_pct: number, cpu_max_pct?: number, mem_bytes: number, mem_pct: number, io_read_bps: number, io_write_bps: number, disk_bytes: number }
interface Limits { memory_max_mb: number, memory_high_mb: number, cpu_quota_pct: number, tasks_max: number }
interface UsageView { range: '24h' | '30d', cpus: number, limits: Limits, points: UsagePoint[], latest?: UsageSample }

const { t, locale } = useI18n()
const api = useApi()
const toast = useToast()
const { canManage, isAdmin } = useAuth()
const { busy } = useAction()
const { site, refresh: refreshSite } = useCurrentSite()

const range = ref<'24h' | '30d'>('24h')
const { data: usage, refresh } = useLive(() => api.get<UsageView>(`/api/sites/${site.value.id}/usage?range=${range.value}`), {
  on: { types: ['alert.'], sites: () => site.value.id },
  fallbackMs: 60000
})
watch(range, () => void refresh())

// The figures above the chart: the newest sample.
const latest = computed(() => usage.value?.latest)
const allowance = computed(() => {
  const u = usage.value
  if (!u) return 1
  return u.limits.cpu_quota_pct > 0 ? u.limits.cpu_quota_pct / 100 : u.cpus
})
const tone = (pct: number) => pct >= 90 ? 'err' as const : pct >= 70 ? 'warn' as const : 'blue' as const
const cells = computed(() => {
  const u = usage.value
  const s = latest.value
  if (!u || !s) return []
  const cpuPct = Math.round(s.cpu_cores * 100 / allowance.value)
  const memMax = u.limits.memory_max_mb * 1024 * 1024
  const memPct = Math.round(s.mem_bytes * 100 / memMax)
  return [
    { key: 'cpu', label: 'CPU', value: cpuPct, unit: '%', bar: cpuPct, barTone: tone(cpuPct),
      note: u.limits.cpu_quota_pct > 0 ? t('resources.cpuOfQuota', { pct: u.limits.cpu_quota_pct }) : t('resources.cpuOfServer', { n: u.cpus }) },
    { key: 'mem', label: t('resources.memory'), value: fmtBytes(s.mem_bytes, locale.value), unit: '', bar: memPct, barTone: tone(memPct),
      note: s.oom_kills > 0 ? t('resources.killed', { n: s.oom_kills }) : t('resources.ofLimit', { limit: fmtBytes(memMax, locale.value) }) },
    { key: 'disk', label: t('resources.disk'), value: s.disk_bytes >= 0 ? fmtBytes(s.disk_bytes, locale.value) : '—', unit: '',
      note: s.disk_inodes >= 0 ? t('resources.files', { n: fmtNumber(s.disk_inodes) }) : t('resources.diskPending') },
    { key: 'io', label: t('resources.io'), value: fmtBytes(s.io_read_bps + s.io_write_bps, locale.value), unit: '/s',
      note: t('resources.ioNote', { read: fmtBytes(s.io_read_bps, locale.value), write: fmtBytes(s.io_write_bps, locale.value) }) }
  ]
})

// The chart: CPU as a share of what the site may use, memory as a share of
// its limit, both 0–100. A day of samples is averaged into 48 buckets.
const buckets = computed(() => {
  const p = usage.value?.points ?? []
  const size = Math.max(1, Math.ceil(p.length / 48))
  const out: { cpu: number, mem: number }[] = []
  for (let i = 0; i < p.length; i += size) {
    const c = p.slice(i, i + size)
    out.push({ cpu: c.reduce((n, x) => n + x.cpu_pct, 0) / c.length, mem: c.reduce((n, x) => n + x.mem_pct, 0) / c.length })
  }
  return out
})
const series = computed<ScopeSeries[]>(() => [
  { key: 'cpu', label: 'CPU', color: 'var(--cg-wave-blue)', values: buckets.value.map(b => b.cpu) },
  { key: 'mem', label: t('resources.memory'), color: 'var(--cg-wave-orange)', values: buckets.value.map(b => b.mem) }
])
const ticks = computed(() => range.value === '24h'
  ? ['-24 h', '-18 h', '-12 h', '-6 h', t('dashboard.now')]
  : [t('resources.daysAgo', { n: 30 }), t('resources.daysAgo', { n: 22 }), t('resources.daysAgo', { n: 15 }), t('resources.daysAgo', { n: 7 }), t('dashboard.now')])
const chartLabel = computed(() => {
  const last = buckets.value[buckets.value.length - 1]
  return last ? t('resources.chartAria', { cpu: Math.round(last.cpu), mem: Math.round(last.mem) }) : t('resources.empty')
})

// Limits (an administrator's) and thresholds, sent as one PATCH of what changed.
const stored = () => {
  const r = site.value.config.resources
  const a = r?.alerts ?? {}
  return {
    memory_max_mb: r?.memory_max_mb ?? 0,
    cpu_quota_pct: r?.cpu_quota_pct ?? 0,
    tasks_max: r?.tasks_max ?? 0,
    alerts_on: !a.off,
    alert_cpu_pct: a.cpu_pct ?? 0,
    alert_memory_pct: a.memory_pct ?? 0,
    alert_disk_mb: a.disk_mb ?? 0
  }
}
const form = reactive(stored())
watch(() => site.value.config, () => Object.assign(form, stored()), { deep: true })
const now = computed(() => stored())
const limitsChanged = computed(() => (['memory_max_mb', 'cpu_quota_pct', 'tasks_max'] as const).some(k => form[k] !== now.value[k]))
const alertsChanged = computed(() => (['alerts_on', 'alert_cpu_pct', 'alert_memory_pct', 'alert_disk_mb'] as const).some(k => form[k] !== now.value[k]))

async function save(body: Record<string, unknown>) {
  busy.value = true
  try {
    await api.patch(`/api/sites/${site.value.id}`, body)
    toast.add({ title: t('common.applied'), color: 'success', icon: 'i-cg-check-circle' })
    await refreshSite()
    await refresh()
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    busy.value = false
  }
}
function saveLimits() {
  const body: Record<string, unknown> = {}
  for (const k of ['memory_max_mb', 'cpu_quota_pct', 'tasks_max'] as const) {
    if (form[k] !== now.value[k]) body[k] = form[k]
  }
  void save(body)
}
function saveAlerts() {
  void save({ usage_alerts: { off: !form.alerts_on, cpu_pct: form.alert_cpu_pct, memory_pct: form.alert_memory_pct, disk_mb: form.alert_disk_mb } })
}
</script>

<template>
  <PanelSection :title="t('resources.title')" :description="t('resources.hint')">
    <template #actions>
      <UFieldGroup>
        <UButton :color="range === '24h' ? 'primary' : 'neutral'" :variant="range === '24h' ? 'solid' : 'outline'" size="sm" :label="t('resources.range24h')" @click="range = '24h'" />
        <UButton :color="range === '30d' ? 'primary' : 'neutral'" :variant="range === '30d' ? 'solid' : 'outline'" size="sm" :label="t('resources.range30d')" @click="range = '30d'" />
      </UFieldGroup>
    </template>
    <USkeleton v-if="!usage" class="h-64" />
    <p v-else-if="!latest" class="m-0 text-sm text-muted">{{ t('resources.empty') }}</p>
    <div v-else class="flex flex-col gap-6">
      <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCell v-for="c in cells" :key="c.key" :label="c.label" :value="c.value" :unit="c.unit" :note="c.note" :bar="c.bar" :bar-tone="c.barTone" />
      </div>
      <ScopeChart :series="series" :ticks="ticks" :label="chartLabel" :height="220" />
    </div>
  </PanelSection>

  <PanelSection :title="t('resources.limitsTitle')" :description="t('resources.limitsHint')">
    <div class="grid max-w-3xl gap-4 sm:grid-cols-3">
      <UFormField :label="t('resources.memoryMax')" :hint="'MB'" :help="t('resources.memoryMaxHelp', { mb: fmtNumber(usage?.limits.memory_max_mb ?? 0) })">
        <UInputNumber v-model="form.memory_max_mb" :min="0" :max="1048576" :step="128" class="w-full" :disabled="!isAdmin" />
      </UFormField>
      <UFormField :label="t('resources.cpuQuota')" hint="%" :help="t('resources.cpuQuotaHelp')">
        <UInputNumber v-model="form.cpu_quota_pct" :min="0" :max="25600" :step="10" class="w-full" :disabled="!isAdmin" />
      </UFormField>
      <UFormField :label="t('resources.tasksMax')" :help="t('resources.tasksMaxHelp', { n: usage?.limits.tasks_max ?? 512 })">
        <UInputNumber v-model="form.tasks_max" :min="0" :max="65536" :step="32" class="w-full" :disabled="!isAdmin" />
      </UFormField>
    </div>
    <template #footer>
      <UButton v-if="isAdmin" :label="t('common.apply')" :loading="busy" :disabled="!limitsChanged" @click="saveLimits" />
      <span v-else class="text-[13px] text-muted">{{ t('resources.adminOnly') }}</span>
    </template>
  </PanelSection>

  <PanelSection :title="t('resources.alertsTitle')" :description="t('resources.alertsHint')">
    <div class="flex max-w-3xl flex-col gap-4">
      <USwitch v-model="form.alerts_on" :label="t('resources.alertsOn')" :disabled="!canManage" />
      <div class="grid gap-4 sm:grid-cols-3">
        <UFormField :label="t('resources.alertCpu')" hint="%" :help="t('resources.alertPctHelp')">
          <UInputNumber v-model="form.alert_cpu_pct" :min="0" :max="100" class="w-full" :disabled="!canManage || !form.alerts_on" />
        </UFormField>
        <UFormField :label="t('resources.alertMemory')" hint="%" :help="t('resources.alertPctHelp')">
          <UInputNumber v-model="form.alert_memory_pct" :min="0" :max="100" class="w-full" :disabled="!canManage || !form.alerts_on" />
        </UFormField>
        <UFormField :label="t('resources.alertDisk')" hint="MB" :help="t('resources.alertDiskHelp')">
          <UInputNumber v-model="form.alert_disk_mb" :min="0" :step="1024" class="w-full" :disabled="!canManage || !form.alerts_on" />
        </UFormField>
      </div>
    </div>
    <template v-if="canManage" #footer>
      <UButton :label="t('common.apply')" :loading="busy" :disabled="!alertsChanged" @click="saveAlerts" />
    </template>
  </PanelSection>
</template>
