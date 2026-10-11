<script setup lang="ts">
/*
 * A table in Quarry, in tabs: Browse (its rows, sorted,
 * filtered, searched, edited by primary key), Structure, SQL, Search,
 * Insert, Export, Import and Operations. A session opened by a read-only
 * panel account sees the structure only (SR-T4); a view is browsed and
 * dropped, never edited.
 */
import type { TabBarItem } from '~/types/tabbar'
import TableData from '~/components/dbstudio/TableData.vue'
import TableStructure from '~/components/dbstudio/TableStructure.vue'
import StudioSql from '~/components/dbstudio/StudioSql.vue'
import StudioSearch from '~/components/dbstudio/StudioSearch.vue'
import StudioExport from '~/components/dbstudio/StudioExport.vue'
import StudioImport from '~/components/dbstudio/StudioImport.vue'
import StudioOperations from '~/components/dbstudio/StudioOperations.vue'

definePageMeta({ layout: 'dbstudio', middleware: 'studio' })

const { t } = useI18n()
const route = useRoute()
const { canRead, canWrite, tableBase, dbLink } = useStudio()
const { tables, loadTables } = useStudioSchema()
const db = computed(() => String(route.params.db ?? ''))
const table = computed(() => String(route.params.table ?? ''))
useHead({ title: () => `${table.value} · ${db.value}` })
watch(db, d => void loadTables(d).catch(() => {}), { immediate: true })
const isView = computed(() => tables.value[db.value]?.tables.find(x => x.name === table.value)?.type === 'view')

const TABS = ['browse', 'structure', 'sql', 'search', 'insert', 'export', 'import', 'operations'] as const
type Tab = typeof TABS[number]
const allowed = computed<Tab[]>(() => {
  if (!canRead.value) return ['structure']
  const out: Tab[] = ['browse', 'structure', 'sql', 'search']
  if (canWrite.value && !isView.value) out.push('insert')
  out.push('export')
  if (canWrite.value && !isView.value) out.push('import')
  if (canWrite.value) out.push('operations')
  return out
})
const tab = computed<Tab>({
  get: () => {
    const q = String(route.query.tab ?? '') as Tab
    return allowed.value.includes(q) ? q : allowed.value[0]!
  },
  set: v => void navigateTo({ query: { ...route.query, tab: v, search: v === 'browse' ? route.query.search : undefined } }, { replace: true })
})
const icons: Record<Tab, string> = {
  browse: 'i-cg-eye', structure: 'i-cg-columns', sql: 'i-cg-sql', search: 'i-cg-search',
  insert: 'i-cg-plus', export: 'i-cg-export', import: 'i-cg-import', operations: 'i-cg-sliders'
}
// Tabs are links: the tab is in the query (the history entry replaced),
// and a search applies to Browse only, as the old setter did.
const items = computed<TabBarItem[]>(() => allowed.value.map(v => ({
  label: t(`studio.tab.${v}`), icon: icons[v], replace: true, active: tab.value === v,
  to: { query: { ...route.query, tab: v, search: v === 'browse' ? route.query.search : undefined } }
})))
const search = computed(() => typeof route.query.search === 'string' ? route.query.search : '')
const base = computed(() => tableBase(db.value, table.value))
const quoted = computed(() => '`' + table.value.replace(/`/g, '``') + '`')
function clearSearch() {
  void navigateTo({ query: { ...route.query, search: undefined } }, { replace: true })
}
</script>

<template>
  <div class="contents">
    <UPageHeader>
      <template #headline>
        <NuxtLink :to="dbLink(db)" class="mono text-muted hover:text-highlighted">{{ db }}</NuxtLink>
      </template>
      <template #title>
        <UIcon :name="isView ? 'i-cg-eye' : 'i-cg-table'" class="size-6 shrink-0 text-muted" />
        <span class="mono min-w-0 truncate">{{ table }}</span>
        <UBadge v-if="isView" color="neutral" variant="subtle" :label="t('dbstudio.view')" />
      </template>
    </UPageHeader>
    <TabBar :items="items" :label="t('dbstudio.table')" />
    <UAlert v-if="!canRead" color="neutral" variant="subtle" icon="i-cg-lock" :description="t('dbstudio.dataReadOnly')" />

    <template v-if="tab === 'browse'">
      <div v-if="search" class="flex flex-wrap items-center gap-2 text-[13px] text-muted">
        <UIcon name="i-cg-search" class="size-4" />
        <span>{{ t('studio.browseSearch', { term: search }) }}</span>
        <UButton size="xs" color="neutral" variant="link" :label="t('dbstudio.clearAll')" @click="clearSearch" />
      </div>
      <TableData :key="`b-${db}-${table}`" :base="base" :can-write="canWrite && !isView" :search="search" />
    </template>
    <TableStructure v-else-if="tab === 'structure'" :key="`s-${db}-${table}`" :base="base" :can-write="canWrite && canRead" />
    <StudioSql v-else-if="tab === 'sql'" :key="`q-${db}-${table}`" :db="db" :initial="`SELECT * FROM ${quoted} LIMIT 50`" />
    <StudioSearch v-else-if="tab === 'search'" :key="`f-${db}-${table}`" :db="db" :table="table" />
    <template v-else-if="tab === 'insert'">
      <p class="m-0 text-sm text-muted">
        {{ t('studio.insertHint') }}
      </p>
      <TableData :key="`i-${db}-${table}`" :base="base" :can-write="canWrite" start-insert />
    </template>
    <StudioExport v-else-if="tab === 'export'" :key="`e-${db}-${table}`" :db="db" :table="table" />
    <StudioImport v-else-if="tab === 'import'" :key="`m-${db}-${table}`" :db="db" :table="table" />
    <StudioOperations v-else-if="tab === 'operations'" :key="`o-${db}-${table}`" :db="db" :table="table" :view="isView" />
  </div>
</template>
