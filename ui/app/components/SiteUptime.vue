<script setup lang="ts">
/**
 * The site's uptime check (#32): every minute the panel requests the site
 * from its public address, as a visitor would; two failures in a row are
 * an alert, sent through the alert webhook (GET/PUT /api/sites/{id}/uptime).
 */
import type { SummaryRow } from '~/types/summary'

interface UptimeResult { checked_at: string, up: boolean, status?: number, latency_ms: number, error?: string, down_since?: string, failures?: number }
interface UptimeView { enabled: boolean, path: string, url: string, interval_seconds: number, last?: UptimeResult }

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/uptime`)
const { data: view, refresh } = useLive(() => api.get<UptimeView>(base.value), {
  on: { types: ['alert.'], sites: () => site.value.id },
  fallbackMs: 60000,
  safetyMs: 60000
})
const path = ref('/')
watch(view, (v) => { if (v) path.value = v.path }, { immediate: true })
const pathOK = computed(() => /^(\/[A-Za-z0-9._~-]*)+$/.test(path.value))

const state = computed<{ label: string, color: 'success' | 'error' | 'warning' | 'neutral' }>(() => {
  const v = view.value
  if (!v?.enabled) return { label: t('uptime.off'), color: 'neutral' }
  if (!v.last) return { label: t('uptime.pending'), color: 'neutral' }
  if (v.last.up) return { label: t('uptime.up'), color: 'success' }
  return (v.last.failures ?? 0) >= 2 ? { label: t('uptime.down'), color: 'error' } : { label: t('uptime.failing'), color: 'warning' }
})
const rows = computed<SummaryRow[]>(() => {
  const v = view.value
  if (!v) return []
  const out: SummaryRow[] = [{ k: t('uptime.url'), v: v.url, mono: true }]
  const r = v.last
  if (v.enabled && r) {
    out.push({ k: t('uptime.lastCheck'), v: fmtWhen(r.checked_at, locale.value) })
    out.push({ k: t('uptime.answer'), v: r.status ? `HTTP ${r.status} · ${fmtNumber(r.latency_ms)} ms` : (r.error ?? '—'), mono: true, tone: r.up ? 'ok' : 'err' })
    if (!r.up && r.down_since) out.push({ k: t('uptime.downSince'), v: fmtDateTime(r.down_since, locale.value), tone: 'err' })
  }
  return out
})
async function save(enabled: boolean) {
  if (await run(() => api.put(base.value, { enabled, path: path.value }), enabled ? t('uptime.saved') : t('uptime.turnedOff'))) await refresh()
}
</script>

<template>
  <PanelSection :title="t('uptime.title')" :description="t('uptime.hint', { n: view?.interval_seconds ?? 60 })">
    <template #actions>
      <UBadge :color="state.color" variant="subtle" size="lg" :label="state.label" />
    </template>
    <div class="grid items-start gap-6 lg:grid-cols-2">
      <SummaryList :rows="rows" />
      <div v-if="canManage" class="flex min-w-0 flex-col gap-3">
        <UFormField :label="t('uptime.path')" :help="t('uptime.pathHelp')" :error="pathOK ? undefined : t('uptime.pathBad')">
          <UInput v-model="path" class="w-full font-mono" placeholder="/" />
        </UFormField>
        <div class="flex flex-wrap gap-2">
          <UButton :label="view?.enabled ? t('common.save') : t('uptime.turnOn')" icon="i-cg-activity" :disabled="!pathOK" :loading="busy" @click="save(true)" />
          <UButton v-if="view?.enabled" color="neutral" variant="outline" :label="t('uptime.turnOff')" :disabled="busy" @click="save(false)" />
        </div>
      </div>
    </div>
  </PanelSection>
</template>
