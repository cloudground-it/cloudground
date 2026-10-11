<script setup lang="ts">
/**
 * A WordPress site's known vulnerabilities (#31): once a day the panel looks
 * the versions of WordPress, its plugins and themes up in a public database
 * and lists what affects them (GET /api/sites/{id}/vulnerabilities; POST
 * …/check checks now). Findings are also an alert, sent through the webhook.
 */
import type { SummaryRow } from '~/types/summary'
import type { VulnFinding, VulnView } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const { site } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/vulnerabilities`)
const { data: view, refresh } = useLive(() => api.get<VulnView>(base.value), {
  on: { types: ['task.finished', 'alert.'], sites: () => site.value.id },
  fallbackMs: 30000
})

const state = computed<{ label: string, color: 'success' | 'error' | 'warning' | 'neutral' }>(() => {
  const v = view.value
  if (!v) return { label: '…', color: 'neutral' }
  if (!v.enabled) return { label: t('vulns.off'), color: 'neutral' }
  if (!v.checked) return { label: t('vulns.pending'), color: 'neutral' }
  const n = v.findings.length
  if (n === 0) return { label: t('vulns.none'), color: 'success' }
  const bad = v.findings.some(f => f.severity === 'critical' || f.severity === 'high')
  return { label: t('vulns.found', { n }), color: bad ? 'error' : 'warning' }
})
const rows = computed<SummaryRow[]>(() => {
  const v = view.value
  if (!v?.checked) return []
  const out: SummaryRow[] = [{ k: t('vulns.lastCheck'), v: fmtWhen(v.checked_at, locale.value) }]
  if (v.core) out.push({ k: 'WordPress', v: v.core, mono: true })
  out.push({ k: t('vulns.components'), v: fmtNumber(v.components, 0, locale.value) })
  return out
})

const severityColor = { critical: 'error', high: 'error', medium: 'warning', low: 'neutral' } as const
const componentLabel = (f: VulnFinding) => f.component === 'core' ? 'WordPress' : f.slug ?? ''
const columns = computed(() => [
  { id: 'severity', header: t('vulns.severity') },
  { id: 'component', header: t('vulns.component') },
  { id: 'title', header: t('vulns.vulnerability') },
  { id: 'fix', header: t('vulns.fix') }
])

async function checkNow() {
  if (await run(() => api.post(`${base.value}/check`), t('vulns.started'))) await refresh()
}
</script>

<template>
  <PanelSection :title="t('vulns.title')" :description="t('vulns.hint', { source: view?.source.name ?? 'WPVulnerability' })" flush>
    <template #actions>
      <UBadge :color="state.color" variant="subtle" size="lg" :label="state.label" />
      <UButton v-if="canManage && view?.enabled" icon="i-cg-refresh" color="neutral" variant="outline" :label="t('vulns.checkNow')" :loading="busy" @click="checkNow" />
    </template>

    <div class="flex flex-col gap-4 px-4 py-4 sm:px-5">
      <UAlert v-if="view && !view.enabled" color="neutral" variant="subtle" icon="i-cg-info" :title="t('vulns.offHint')">
        <template v-if="isAdmin" #description>
          <ULink to="/settings#notifications" class="underline">{{ t('vulns.offLink') }}</ULink>
        </template>
      </UAlert>
      <UAlert v-if="view?.error" color="warning" variant="subtle" icon="i-cg-alert-triangle" :title="t('vulns.failed', { when: fmtWhen(view.attempted_at, locale) })" :description="view.error" />
      <SummaryList v-if="rows.length" :rows="rows" />
    </div>

    <UTable v-if="view?.findings.length" :data="view.findings" :columns="columns" class="border-t border-default">
      <template #severity-cell="{ row: { original: f } }">
        <UBadge v-if="f.severity" :color="severityColor[f.severity]" variant="subtle" :label="t(`vulns.level.${f.severity}`)" />
        <span v-else class="text-dimmed">—</span>
      </template>
      <template #component-cell="{ row: { original: f } }">
        <div class="flex min-w-0 flex-col">
          <span class="mono text-[13px] text-highlighted">{{ componentLabel(f) }} {{ f.version }}</span>
          <span class="text-xs text-muted">{{ t(`vulns.kind.${f.component}`) }}</span>
        </div>
      </template>
      <template #title-cell="{ row: { original: f } }">
        <div class="flex min-w-0 max-w-[48ch] flex-col whitespace-normal">
          <ULink v-if="f.link" :to="f.link" target="_blank" rel="noopener noreferrer" class="text-highlighted underline-offset-2 hover:underline">{{ f.title }}</ULink>
          <span v-else class="text-highlighted">{{ f.title }}</span>
          <span v-if="f.cve" class="mono text-xs text-muted">{{ f.cve }}</span>
        </div>
      </template>
      <template #fix-cell="{ row: { original: f } }">
        <span v-if="f.fixed_in" class="text-sm">{{ t('vulns.fixedIn', { v: f.fixed_in }) }}</span>
        <span v-else-if="f.unfixed" class="text-sm text-error">{{ t('vulns.unfixed') }}</span>
        <span v-else class="text-sm text-muted">{{ t('vulns.fixUnknown') }}</span>
      </template>
    </UTable>

    <p v-if="view" class="m-0 border-t border-default px-4 py-3 text-[13px] text-muted sm:px-5">
      {{ t('vulns.sourceNote') }}
      <ULink :to="view.source.url" target="_blank" rel="noopener noreferrer" class="underline">{{ view.source.name }}</ULink>
      ({{ view.source.license }})
    </p>
  </PanelSection>
</template>
