<script setup lang="ts">
/*
 * Search: the rows that contain a term in any text column,
 * counted table by table on the server; a table's count opens Browse on
 * those rows. With table set, it searches that table only.
 */
import type { DBSearchResult } from '~/types/dbstudio'

const props = defineProps<{ db: string, table?: string }>()
const { t } = useI18n()
const { api, dbBase, dbLink } = useStudio()
const { busy, run } = useAction()

const term = ref('')
const result = ref<DBSearchResult | null>(null)
const searched = ref('')

async function search() {
  const q = term.value
  if (!q.trim()) return
  if (props.table) {
    await navigateTo({ path: `${dbLink(props.db)}/${encodeURIComponent(props.table)}`, query: { tab: 'browse', search: q } })
    return
  }
  await run(async () => {
    result.value = await api.post<DBSearchResult>(`${dbBase(props.db)}/search`, { term: q })
    searched.value = q
  })
}
const browse = (table: string) => ({ path: `${dbLink(props.db)}/${encodeURIComponent(table)}`, query: { tab: 'browse', search: searched.value } })
</script>

<template>
  <PanelSection :title="t('studio.tab.search')" :description="table ? t('studio.searchTableHint') : t('studio.searchHint')">
    <form id="studio-search" class="flex max-w-2xl flex-wrap items-end gap-2" @submit.prevent="search">
      <UFormField :label="t('studio.searchTerm')" class="min-w-0 flex-[1_1_18rem]">
        <UInput v-model="term" icon="i-cg-search" class="w-full font-mono" maxlength="256" />
      </UFormField>
      <UButton type="submit" icon="i-cg-search" :label="t('studio.searchRun')" :loading="busy" :disabled="!term.trim()" />
    </form>
  </PanelSection>
  <PanelSection v-if="result" :title="t('studio.searchSummary', { n: fmtNumber(result.tables.length), searched: fmtNumber(result.searched), term: searched })" flush>
    <UAlert v-if="result.truncated" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('studio.searchTruncated')" class="m-4" />
    <ul v-if="result.tables.length" class="m-0 list-none divide-y divide-default p-0">
      <li v-for="h in result.tables" :key="h.table" class="flex flex-wrap items-center gap-3 px-4 py-2.5 sm:px-5">
        <UIcon name="i-cg-table" class="size-4 text-muted" />
        <span class="mono min-w-0 flex-1 truncate text-[13px] font-semibold text-highlighted">{{ h.table }}</span>
        <span class="text-[13px] text-muted">{{ t('dbstudio.rowCount', { n: fmtNumber(h.matches) }) }}</span>
        <UButton size="sm" color="neutral" variant="outline" icon="i-cg-eye" :label="t('studio.tab.browse')" :to="browse(h.table)" />
      </li>
    </ul>
    <UEmpty v-else size="sm" icon="i-cg-search" :description="t('studio.noMatch')" />
  </PanelSection>
</template>
