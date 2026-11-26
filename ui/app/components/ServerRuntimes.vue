<script setup lang="ts">
/**
 * Node.js and Python on the server (#28): the versions installed for the
 * Node.js and Python sites, and installing another (GET /api/runtimes,
 * POST /api/runtimes/install).
 */
import type { TableColumn } from '@nuxt/ui'
import type { Runtimes } from '~/types'

interface RuntimesView { installed: Runtimes, available: { node: string[], python: string[] } }
interface RuntimeRow { runtime: 'node' | 'python', label: string, version: string, full: string, installed: boolean }

const { t } = useI18n()
const api = useApi()
const { busy, run } = useAction()
const { data, refresh } = useLoad(() => api.get<RuntimesView>('/api/runtimes'))

// The same table as the services above it, so the two line up.
const columns = computed<TableColumn<RuntimeRow>[]>(() => [
  { accessorKey: 'label', header: t('runtimes.col.runtime') },
  { accessorKey: 'full', header: t('runtimes.col.version') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-right' } } }
])

const rows = computed<RuntimeRow[]>(() => {
  const v = data.value
  if (!v) return []
  const row = (runtime: 'node' | 'python', label: string): RuntimeRow[] => v.available[runtime].map(version => {
    const have = v.installed[runtime].find(i => i.version === version)
    return { runtime, label: `${label} ${version}`, version, full: have?.full ?? '', installed: !!have }
  })
  return [...row('node', 'Node.js'), ...row('python', 'Python')]
})

function install(runtime: string, version: string) {
  void run(() => api.post('/api/runtimes/install', { runtime, version }), t('runtimes.queued')).then(() => setTimeout(() => void refresh(), 5000))
}
</script>

<template>
  <PanelSection id="runtimes" flush :title="t('runtimes.title')" :description="t('runtimes.hint')">
    <UTable :data="rows" :columns="columns" :loading="!data">
      <template #label-cell="{ row }">
        <span class="flex items-center gap-3">
          <UIcon :name="row.original.runtime === 'node' ? 'i-brand-node' : 'i-brand-python'" class="size-4 shrink-0 text-muted" aria-hidden="true" />
          <span class="font-semibold text-highlighted">{{ row.original.label }}</span>
        </span>
      </template>
      <template #full-cell="{ row }">
        <span class="mono text-[13px] text-muted">{{ row.original.full || '—' }}</span>
      </template>
      <template #actions-cell="{ row }">
        <UBadge v-if="row.original.installed" color="success" variant="subtle" :label="t('runtimes.installed')" />
        <UButton v-else size="sm" color="neutral" variant="outline" icon="i-cg-download" :label="t('runtimes.install')" :disabled="busy" @click="install(row.original.runtime, row.original.version)" />
      </template>
    </UTable>
  </PanelSection>
</template>
