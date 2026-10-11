<script setup lang="ts">
import type { WPAutoload, WPSearchReplaceResult, WPTransientsResult } from '~/types/dbstudio'

const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

const sid = computed(() => site.value.id)
const wordpress = computed(() => isWordPress(site.value))

// Autoload: what WordPress loads on every request.
const AUTOLOAD_WARN = 800 * 1024
const autoload = ref<WPAutoload | null>(null)
const autoloadError = ref<string | null>(null)
const autoloadLoading = ref(false)
async function loadAutoload() {
  autoloadLoading.value = true
  try {
    autoload.value = await api.get<WPAutoload>(`/api/sites/${sid.value}/quarry/wp/autoload`)
    autoloadError.value = null
  } catch (e) {
    autoloadError.value = errorMessage(e)
  } finally {
    autoloadLoading.value = false
  }
}
onMounted(() => {
  if (wordpress.value) void loadAutoload()
})

// Search and replace, through wp-cli (serialized data stays valid).
const from = ref('')
const to = ref('')
const lastReplace = ref<WPSearchReplaceResult | null>(null)
async function searchReplace() {
  const body = { from: from.value, to: to.value }
  let dry: WPSearchReplaceResult | null = null
  if (!await run(async () => {
    dry = await api.post<WPSearchReplaceResult>(`/api/sites/${sid.value}/quarry/wp/search-replace`, { ...body, dry_run: true })
  }) || !dry) return
  lastReplace.value = dry
  const n = (dry as WPSearchReplaceResult).replacements
  if (!n) return
  const ok = await confirm({ title: t('dbstudio.replaceTitle'), description: t('dbstudio.replaceConfirm', { n, from: from.value, to: to.value }), danger: true, confirmLabel: t('dbstudio.replace') })
  if (!ok) return
  await run(async () => {
    lastReplace.value = await api.post<WPSearchReplaceResult>(`/api/sites/${sid.value}/quarry/wp/search-replace`, { ...body, dry_run: false })
  }, t('dbstudio.replaced'))
}

// Expired transients.
const transients = ref<WPTransientsResult | null>(null)
async function cleanTransients() {
  let dry: WPTransientsResult | null = null
  if (!await run(async () => {
    dry = await api.post<WPTransientsResult>(`/api/sites/${sid.value}/quarry/wp/transients`, { dry_run: true })
  }) || !dry) return
  transients.value = dry
  const n = (dry as WPTransientsResult).expired
  if (!n) return
  const ok = await confirm({ title: t('dbstudio.transientsTitle'), description: t('dbstudio.transientsConfirm', { n }), confirmLabel: t('common.delete') })
  if (!ok) return
  await run(async () => {
    transients.value = await api.post<WPTransientsResult>(`/api/sites/${sid.value}/quarry/wp/transients`, { dry_run: false })
  }, t('dbstudio.transientsDeleted'))
  void loadAutoload()
}
const autoloadColumns = computed(() => [
  { accessorKey: 'name', header: 'option_name' },
  { accessorKey: 'autoload', header: 'autoload' },
  { accessorKey: 'bytes', header: t('dbstudio.size'), meta: { class: { th: 'text-end', td: 'text-end' } } }
])
</script>

<template>
  <div>
    <UButton :to="`/sites/${site.id}/database`" color="neutral" variant="ghost" size="sm" icon="i-cg-chevron-left" :label="t('dbs.databases')" />
  </div>
  <UAlert v-if="!wordpress" color="neutral" variant="subtle" icon="i-cg-info" :description="t('dbstudio.notWordPress')" />
  <template v-else>
    <PanelSection icon="i-cg-gauge" :title="t('dbstudio.autoload')" :description="t('dbstudio.autoloadHint')" flush>
      <template #actions>
        <UButton size="sm" color="neutral" variant="outline" icon="i-cg-refresh" :label="t('common.refresh')" :loading="autoloadLoading" @click="loadAutoload" />
      </template>
      <div v-if="autoloadError || autoload" class="flex flex-col gap-3 px-4 py-4 sm:px-5">
        <UAlert v-if="autoloadError" color="error" variant="subtle" :description="autoloadError" />
        <template v-if="autoload">
          <div class="flex flex-wrap items-baseline gap-3">
            <span class="disp text-[28px] leading-none text-highlighted">{{ fmtBytes(autoload.bytes) }}</span>
            <span class="text-sm text-muted">{{ t('dbstudio.autoloadCount', { n: fmtNumber(autoload.count), table: autoload.table }) }}</span>
          </div>
          <UAlert v-if="autoload.bytes > AUTOLOAD_WARN" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('dbstudio.autoloadWarn')" />
        </template>
      </div>
      <UTable v-if="autoload?.top?.length" :data="autoload.top" :columns="autoloadColumns" class="border-t border-default">
        <template #name-cell="{ row }">
          <span class="mono block max-w-96 truncate text-xs" :title="row.original.name">{{ row.original.name }}</span>
        </template>
        <template #autoload-cell="{ row }">
          <span class="mono text-xs">{{ row.original.autoload }}</span>
        </template>
        <template #bytes-cell="{ row }">
          <span class="mono">{{ fmtBytes(row.original.bytes) }}</span>
        </template>
      </UTable>
    </PanelSection>

    <PanelSection icon="i-cg-diff" :title="t('dbstudio.searchReplace')" :description="t('dbstudio.searchReplaceHint')">
      <div class="grid gap-4 sm:grid-cols-2">
        <UFormField :label="t('dbstudio.search')">
          <UInput v-model="from" class="w-full font-mono" :disabled="!canManage" />
        </UFormField>
        <UFormField :label="t('dbstudio.replaceWith')">
          <UInput v-model="to" class="w-full font-mono" :disabled="!canManage" />
        </UFormField>
      </div>
      <template #footer>
        <UButton icon="i-cg-search" :label="t('dbstudio.dryRun')" :loading="busy" :disabled="!canManage || !from || !to || from === to" @click="searchReplace" />
        <span v-if="lastReplace" class="text-sm text-muted">
          {{ lastReplace.executed ? t('dbstudio.replacedCount', { n: fmtNumber(lastReplace.replacements) }) : t('dbstudio.wouldReplace', { n: fmtNumber(lastReplace.replacements) }) }}
        </span>
      </template>
    </PanelSection>

    <PanelSection icon="i-cg-clock" :title="t('dbstudio.transients')" :description="t('dbstudio.transientsHint')">
      <template #footer>
        <UButton color="neutral" variant="outline" icon="i-cg-trash" :label="t('dbstudio.cleanTransients')" :loading="busy" :disabled="!canManage" @click="cleanTransients" />
        <span v-if="transients" class="text-sm text-muted">
          {{ transients.executed ? t('dbstudio.transientsDone', { n: fmtNumber(transients.deleted) }) : t('dbstudio.transientsFound', { n: fmtNumber(transients.expired) }) }}
        </span>
      </template>
    </PanelSection>
  </template>
</template>
