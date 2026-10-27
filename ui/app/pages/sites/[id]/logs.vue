<script setup lang="ts">
/**
 * A site's logs: the last lines of each (any signed-in account), and for the
 * accounts that manage the site the whole files nginx writes and logrotate
 * keeps, current and rotated, to download (#84; GET
 * /api/sites/{id}/logs/files and /logs/download?file=, audited).
 */
interface LogFile { name: string, size: number, modified: string }

const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { site } = useCurrentSite()
const source = ref('access')
const items = computed(() => [
  { label: t('logs.access'), value: 'access', icon: 'i-cg-activity' },
  { label: t('logs.error'), value: 'error', icon: 'i-cg-alert-triangle' },
  { label: t('logs.php'), value: 'php', icon: 'i-cg-code' },
  // The weekly database cleanup writes its results here (WordPress sites).
  ...(site.value.type === 'wordpress' || site.value.type === 'woocommerce' ? [{ label: t('logs.cleanup'), value: 'cleanup', icon: 'i-cg-purge' }] : [])
])

const { data, refresh } = useLoad(() => canManage.value ? api.get<{ files: LogFile[] }>(`/api/sites/${site.value.id}/logs/files`) : Promise.resolve({ files: [] as LogFile[] }))
const files = computed(() => data.value?.files ?? [])
const rotated = (f: LogFile) => /\.log\.\d/.test(f.name)
const href = (f: LogFile) => `/api/sites/${site.value.id}/logs/download?file=${encodeURIComponent(f.name)}`
const columns = computed(() => [
  { id: 'name', header: t('logs.file') },
  { id: 'size', header: t('logs.size') },
  { id: 'modified', header: t('logs.modified') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="scrollbar-none overflow-x-auto overflow-y-hidden">
      <UTabs v-model="source" :items="items" :content="false" class="w-max min-w-full" />
    </div>
    <LogViewer :source="source" :site-id="site.id" :site="site.domain" :lines="200" />

    <PanelSection v-if="canManage" class="mt-2" :title="t('logs.downloadTitle')" :description="t('logs.downloadHint')" flush>
      <template #actions>
        <UButton color="neutral" variant="outline" :label="t('logs.reload')" @click="refresh()" />
      </template>
      <UEmpty v-if="!files.length" icon="i-cg-logs" :description="t('logs.noFiles')" />
      <UTable v-else :data="files" :columns="columns">
        <template #name-cell="{ row: { original: f } }">
          <span class="mono text-[13px] text-highlighted">{{ f.name }}</span>
          <UBadge class="ms-2" color="neutral" variant="subtle" :label="rotated(f) ? (f.name.endsWith('.gz') ? t('logs.compressed') : t('logs.rotated')) : t('logs.current')" />
        </template>
        <template #size-cell="{ row: { original: f } }">
          <span class="mono text-[13px]">{{ fmtBytes(f.size) }}</span>
        </template>
        <template #modified-cell="{ row: { original: f } }">
          <span class="text-[13px] text-muted">{{ fmtDateTime(f.modified) }}</span>
        </template>
        <template #actions-cell="{ row: { original: f } }">
          <UButton size="sm" color="neutral" variant="outline" icon="i-cg-download" :label="t('logs.download')" :href="href(f)" :download="`${site.domain}-${f.name}`" />
        </template>
      </UTable>
      <p v-if="files.length" class="m-0 border-t border-default px-4 py-3 text-[13px] text-muted sm:px-5">
        {{ t('logs.currentNote') }}
      </p>
    </PanelSection>
  </div>
</template>
