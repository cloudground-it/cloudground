<script setup lang="ts">
/**
 * A function's runs: the latest invocations (a request, its schedule, a
 * test) and what it printed (GET /api/functions/{name}/logs).
 */
import type { FnLogs } from '~/utils/functions'

const { t, locale } = useI18n()
const api = useApi()
const { base } = useCurrentFunction()

const { data: logs, error, pending, refresh } = useLoad(() => api.get<FnLogs>(`${base.value}/logs`))
const reloading = ref(false)
async function reload() {
  reloading.value = true
  await refresh()
  reloading.value = false
}

const columns = computed(() => [
  { id: 'time', header: t('fn.runs.when') },
  { id: 'method', header: t('fn.runs.request') },
  { id: 'status', header: t('fn.runs.status'), meta: { class: { th: 'text-end', td: 'text-end' } } },
  { id: 'ms', header: t('fn.runs.duration'), meta: { class: { th: 'text-end', td: 'text-end' } } },
  { id: 'source', header: t('fn.runs.source'), meta: { class: { th: 'hidden sm:table-cell', td: 'hidden sm:table-cell' } } }
])
</script>

<template>
  <div class="flex flex-col gap-6">
    <PanelSection :title="t('fn.runs.title')" :description="t('fn.runs.hint')" flush>
      <template #actions>
        <UButton color="neutral" variant="outline" size="sm" icon="i-cg-refresh" :label="t('common.refresh')" :loading="reloading" @click="reload" />
      </template>
      <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" class="rounded-none" />
      <div v-if="pending" class="flex flex-col gap-2 p-4">
        <USkeleton v-for="i in 4" :key="i" class="h-8" />
      </div>
      <UTable v-else :data="logs?.invocations ?? []" :columns="columns" :empty="t('fn.runs.none')">
        <template #time-cell="{ row: { original: r } }">
          <span class="whitespace-nowrap" :title="fmtAgo(r.time, locale)">{{ fmtDateTime(r.time, locale) }}</span>
        </template>
        <template #method-cell="{ row: { original: r } }">
          <span class="mono text-[13px]"><span class="font-medium text-highlighted">{{ r.method }}</span> <span class="break-all text-muted">{{ r.path }}</span></span>
        </template>
        <template #status-cell="{ row: { original: r } }">
          <UBadge :color="httpStatusColor(r.status)" variant="subtle" class="mono" :label="String(r.status || '—')" />
        </template>
        <template #ms-cell="{ row: { original: r } }">
          <span class="mono text-[13px] tabular-nums">{{ fmtDuration(r.ms) }}</span>
        </template>
        <template #source-cell="{ row: { original: r } }">
          <span class="text-muted">{{ t(`fn.runs.src.${r.source}`, r.source) }}</span>
        </template>
      </UTable>
    </PanelSection>

    <PanelSection :title="t('fn.runs.output')" :description="t('fn.runs.outputHint')" flush>
      <pre v-if="logs?.output.length" class="m-0 max-h-[28rem] overflow-auto bg-muted px-4 py-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-highlighted">{{ logs.output.join('\n') }}</pre>
      <p v-else-if="logs" class="m-0 px-4 py-3 text-sm text-muted sm:px-5">{{ t('fn.runs.noOutput') }}</p>
    </PanelSection>
  </div>
</template>
