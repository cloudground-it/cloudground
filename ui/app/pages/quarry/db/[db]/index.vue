<script setup lang="ts">
/*
 * A database in Quarry, in tabs: its tables (Structure), the
 * SQL editor, Search, Export and Import. A session opened by a read-only
 * panel account sees the tables only.
 */
import type { TabBarItem } from '~/types/tabbar'
import StudioSql from '~/components/dbstudio/StudioSql.vue'
import StudioSearch from '~/components/dbstudio/StudioSearch.vue'
import StudioExport from '~/components/dbstudio/StudioExport.vue'
import StudioImport from '~/components/dbstudio/StudioImport.vue'

definePageMeta({ layout: 'dbstudio', middleware: 'studio' })

const { t, locale } = useI18n()
const route = useRoute()
const { canRead, tableLink } = useStudio()
const { tables, errors, loadTables } = useStudioSchema()
const db = computed(() => String(route.params.db ?? ''))
useHead({ title: () => db.value })

const TABS = ['structure', 'sql', 'search', 'export', 'import'] as const
type Tab = typeof TABS[number]
const tab = computed<Tab>({
  get: () => {
    const q = String(route.query.tab ?? '') as Tab
    return TABS.includes(q) && (q === 'structure' || canRead.value) ? q : 'structure'
  },
  set: v => void navigateTo({ query: { ...route.query, tab: v } }, { replace: true })
})
const ICONS: Record<Tab, string> = { structure: 'i-cg-table', sql: 'i-cg-sql', search: 'i-cg-search', export: 'i-cg-export', import: 'i-cg-import' }
// Tabs are links (the tab is in the query, the history entry replaced).
const items = computed<TabBarItem[]>(() => (canRead.value ? [...TABS] : ['structure' as const]).map(v => ({
  label: t(`studio.tab.${v}`), icon: ICONS[v], to: { query: { ...route.query, tab: v } }, replace: true, active: tab.value === v
})))
const columns = computed(() => [
  { id: 'name', header: t('dbstudio.table') },
  { id: 'rows', header: t('dbstudio.rows'), meta: { class: { th: 'text-end', td: 'text-end' } } },
  { id: 'size', header: t('dbstudio.size'), meta: { class: { th: 'text-end', td: 'text-end' } } },
  { id: 'engine', header: t('dbstudio.engine'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'collation', header: t('dbstudio.collation'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])

const loading = ref(false)
async function reload(force = false) {
  loading.value = true
  try {
    await loadTables(db.value, force)
  } catch {
    // Shown from errors.
  } finally {
    loading.value = false
  }
}
watch(db, () => void reload(), { immediate: true })
const list = computed(() => tables.value[db.value]?.tables ?? [])
const totalBytes = computed(() => list.value.reduce((n, x) => n + x.data_bytes + x.index_bytes, 0))
</script>

<template>
  <UPageHeader :headline="t('studio.database')">
    <template #title>
      <span class="mono min-w-0 truncate">{{ db }}</span>
    </template>
    <template v-if="tables[db]" #description>
      {{ t('dbstudio.tableCount', { n: fmtNumber(list.length, undefined, locale) }) }} · {{ fmtBytes(totalBytes, locale) }}
    </template>
  </UPageHeader>
  <TabBar :items="items" :label="t('studio.database')" />
  <UAlert v-if="!canRead" color="neutral" variant="subtle" icon="i-cg-lock" :description="t('dbstudio.dataReadOnly')" />

  <template v-if="tab === 'structure'">
    <UAlert v-if="errors[db]" color="error" variant="subtle" :description="errors[db]" />
    <PanelSection :title="t('studio.group.tables')" :description="t('dbstudio.rowsEstimate')" flush>
      <template #actions>
        <UButton color="neutral" variant="outline" size="sm" icon="i-cg-refresh" :label="t('common.refresh')" :loading="loading" @click="reload(true)" />
      </template>
      <USkeleton v-if="!tables[db] && !errors[db]" class="m-4 h-24" />
      <UTable v-else :data="list" :columns="columns" :empty="t('dbstudio.noTables')">
        <template #name-cell="{ row: { original: x } }">
          <span class="flex items-center gap-2">
            <UIcon :name="x.type === 'view' ? 'i-cg-eye' : 'i-cg-table'" class="size-4 shrink-0 text-muted" />
            <NuxtLink :to="tableLink(db, x.name)" class="mono text-[13px] font-semibold text-highlighted hover:underline">{{ x.name }}</NuxtLink>
            <UBadge v-if="x.type === 'view'" color="neutral" variant="subtle" :label="t('dbstudio.view')" />
          </span>
        </template>
        <template #rows-cell="{ row: { original: x } }">
          <span class="mono">{{ x.type === 'view' ? '—' : fmtNumber(x.rows, undefined, locale) }}</span>
        </template>
        <template #size-cell="{ row: { original: x } }">
          <span class="mono">{{ fmtBytes(x.data_bytes + x.index_bytes, locale) }}</span>
        </template>
        <template #engine-cell="{ row: { original: x } }">
          {{ x.engine || '—' }}
        </template>
        <template #collation-cell="{ row: { original: x } }">
          <span class="mono text-xs">{{ x.collation || '—' }}</span>
        </template>
        <template #actions-cell="{ row: { original: x } }">
          <span class="flex items-center justify-end gap-1">
            <UButton v-if="canRead" size="sm" color="neutral" variant="outline" icon="i-cg-eye" :label="t('studio.tab.browse')" :aria-label="`${t('studio.tab.browse')} ${x.name}`" :ui="{ label: 'hidden sm:inline' }" :to="tableLink(db, x.name, 'browse')" />
            <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-columns" :aria-label="`${t('studio.tab.structure')} ${x.name}`" :title="t('studio.tab.structure')" :to="tableLink(db, x.name, 'structure')" />
          </span>
        </template>
      </UTable>
    </PanelSection>
  </template>
  <StudioSql v-else-if="tab === 'sql'" :key="`sql-${db}`" :db="db" />
  <StudioSearch v-else-if="tab === 'search'" :key="`search-${db}`" :db="db" />
  <StudioExport v-else-if="tab === 'export'" :key="`export-${db}`" :db="db" />
  <StudioImport v-else-if="tab === 'import'" :key="`import-${db}`" :db="db" />
</template>
