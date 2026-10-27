<script setup lang="ts">
/**
 * Node.js and Python on the server (#28): the versions installed for the
 * Node.js and Python sites, and installing another (GET /api/runtimes,
 * POST /api/runtimes/install).
 */
import type { Runtimes } from '~/types'

interface RuntimesView { installed: Runtimes, available: { node: string[], python: string[] } }

const { t } = useI18n()
const api = useApi()
const { busy, run } = useAction()
const { data, refresh } = useLoad(() => api.get<RuntimesView>('/api/runtimes'))

const rows = computed(() => {
  const v = data.value
  if (!v) return []
  const row = (runtime: 'node' | 'python', label: string) => v.available[runtime].map(version => {
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
  <PanelSection id="runtimes" :title="t('runtimes.title')" :description="t('runtimes.hint')">
    <USkeleton v-if="!data" class="h-20" />
    <ul v-else class="m-0 flex list-none flex-col p-0">
      <li v-for="r in rows" :key="r.runtime + r.version" class="flex items-center justify-between gap-3 border-b border-default py-2 last:border-b-0">
        <span class="flex items-center gap-2">
          <UIcon :name="r.runtime === 'node' ? 'i-brand-node' : 'i-brand-python'" class="size-4" aria-hidden="true" />
          <span class="font-semibold text-highlighted">{{ r.label }}</span>
          <span v-if="r.installed" class="font-mono text-xs text-muted">{{ r.full }}</span>
        </span>
        <UBadge v-if="r.installed" color="success" variant="subtle" :label="t('runtimes.installed')" />
        <UButton v-else size="sm" color="neutral" variant="outline" icon="i-cg-download" :label="t('runtimes.install')" :disabled="busy" @click="install(r.runtime, r.version)" />
      </li>
    </ul>
  </PanelSection>
</template>
