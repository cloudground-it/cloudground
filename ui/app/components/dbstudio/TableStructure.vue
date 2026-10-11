<script setup lang="ts">
import SqlPreview from '~/components/dbstudio/SqlPreview.vue'
import type { DBAlterResult, DBIndexColumn, DBNewColumn, DBStructure } from '~/types/dbstudio'

/*
 * A table's structure: columns, indexes, foreign keys and its CREATE
 * statement, with the guided ALTERs (a column, an index), each previewed.
 * base is the table's Quarry API path.
 */
const props = defineProps<{ base: string, canWrite: boolean }>()
const { t } = useI18n()
const { api } = useStudio()
const { busy, run } = useAction()
const preview = useOverlay().create(SqlPreview)

const base = computed(() => props.base)
const canManage = computed(() => props.canWrite)
const { data, error, refresh } = useLoad(() => api.get<DBStructure>(base.value))
const isView = computed(() => data.value?.type === 'view')

const COLUMN_TYPES = ['TINYINT', 'SMALLINT', 'MEDIUMINT', 'INT', 'BIGINT', 'DECIMAL', 'FLOAT', 'DOUBLE', 'BOOLEAN',
  'CHAR', 'VARCHAR', 'BINARY', 'VARBINARY', 'TINYTEXT', 'TEXT', 'MEDIUMTEXT', 'LONGTEXT', 'BLOB', 'MEDIUMBLOB',
  'LONGBLOB', 'DATE', 'DATETIME', 'TIMESTAMP', 'TIME', 'YEAR', 'JSON']
const WITH_LENGTH = ['CHAR', 'VARCHAR', 'BINARY', 'VARBINARY', 'DECIMAL']
const NUMERIC = ['TINYINT', 'SMALLINT', 'MEDIUMINT', 'INT', 'BIGINT', 'DECIMAL', 'FLOAT', 'DOUBLE']

// Add a column.
const colOpen = ref(false)
const col = reactive<DBNewColumn & { hasDefault: boolean, defaultValue: string }>({
  name: '', type: 'VARCHAR', length: 255, scale: 0, unsigned: false, nullable: true, default: null, after: '', hasDefault: false, defaultValue: ''
})
const afterItems = computed(() => [
  { label: t('dbstudio.atEnd'), value: '' },
  ...(data.value?.columns ?? []).map(c => ({ label: c.name, value: c.name }))
])
function openColumn() {
  Object.assign(col, { name: '', type: 'VARCHAR', length: 255, scale: 0, unsigned: false, nullable: true, after: '', hasDefault: false, defaultValue: '' })
  colOpen.value = true
}

// Add an index.
const idxOpen = ref(false)
const idx = reactive<{ name: string, unique: boolean, columns: DBIndexColumn[] }>({ name: '', unique: false, columns: [] })
const columnItems = computed(() => (data.value?.columns ?? []).map(c => c.name))
const idxColumnNames = computed({
  get: () => idx.columns.map(c => c.name),
  set: (names: string[]) => {
    idx.columns = names.map(n => idx.columns.find(c => c.name === n) ?? { name: n, length: 0 })
  }
})
function openIndex() {
  Object.assign(idx, { name: '', unique: false, columns: [] })
  idxOpen.value = true
}

async function alter(body: Record<string, unknown>, done: () => void) {
  let res: DBAlterResult | null = null
  const ok = await run(async () => {
    res = await api.post<DBAlterResult>(`${base.value}/alter`, { ...body, dry_run: true })
  })
  if (!ok || !res) return
  const confirmed = await preview.open({ title: t('dbstudio.alterPreview'), description: t('dbstudio.alterPreviewHint'), sql: (res as DBAlterResult).sql })
  if (confirmed !== true) return
  if (await run(() => api.post(`${base.value}/alter`, { ...body, dry_run: false }), t('dbstudio.altered'))) {
    done()
    await refresh()
  }
}
function submitColumn() {
  const c = col
  void alter({
    kind: 'add_column',
    column: {
      name: c.name.trim(), type: c.type,
      length: WITH_LENGTH.includes(c.type) ? Number(c.length) || 0 : 0,
      scale: c.type === 'DECIMAL' ? Number(c.scale) || 0 : 0,
      unsigned: NUMERIC.includes(c.type) && c.unsigned,
      nullable: c.nullable, default: c.hasDefault ? c.defaultValue : null, after: c.after
    },
    index: null
  }, () => { colOpen.value = false })
}
function submitIndex() {
  void alter({
    kind: 'add_index',
    column: null,
    index: { name: idx.name.trim(), unique: idx.unique, columns: idx.columns.map(c => ({ name: c.name, length: Number(c.length) || 0 })) }
  }, () => { idxOpen.value = false })
}
const colColumns = computed(() => [
  { id: 'name', header: t('dbstudio.column') },
  { id: 'type', header: t('dbstudio.type') },
  { id: 'nullable', header: t('dbstudio.nullable') },
  { id: 'default', header: t('dbstudio.default') },
  { id: 'extra', header: t('dbstudio.extra') },
  { id: 'collation', header: t('dbstudio.collation'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } }
])
const idxColumns = computed(() => [
  { id: 'name', header: t('dbstudio.name') },
  { id: 'columns', header: t('dbstudio.columns') },
  { id: 'type', header: t('dbstudio.type') }
])
const fkColumns = computed(() => [
  { id: 'name', header: t('dbstudio.name') },
  { id: 'columns', header: t('dbstudio.columns') },
  { id: 'ref', header: t('dbstudio.references') },
  { id: 'rules', header: 'ON UPDATE / ON DELETE' }
])
</script>

<template>
  <div class="flex flex-col gap-6">
    <UAlert v-if="error" color="error" variant="subtle" :description="error" />
    <template v-if="data">
      <PanelSection :title="t('dbstudio.columns')" flush>
        <template v-if="canManage && !isView" #actions>
          <UButton size="sm" color="neutral" variant="outline" icon="i-cg-columns" :label="t('dbstudio.addColumn')" @click="openColumn" />
          <UButton size="sm" color="neutral" variant="outline" icon="i-cg-sort" :label="t('dbstudio.addIndex')" @click="openIndex" />
        </template>
        <UTable :data="data.columns" :columns="colColumns">
          <template #name-cell="{ row: { original: c } }">
            <span class="mono text-[13px] font-semibold text-highlighted">{{ c.name }}</span>
            <UBadge v-if="c.key === 'PRI'" size="sm" color="info" variant="subtle" class="ms-1.5" label="PK" />
            <UBadge v-else-if="c.key === 'UNI'" size="sm" color="neutral" variant="subtle" class="ms-1.5" label="UNI" />
            <UBadge v-else-if="c.key === 'MUL'" size="sm" color="neutral" variant="outline" class="ms-1.5" label="IDX" />
          </template>
          <template #type-cell="{ row: { original: c } }">
            <span class="mono text-xs">{{ c.type }}</span>
          </template>
          <template #nullable-cell="{ row: { original: c } }">
            {{ c.nullable ? t('common.on') : t('common.off') }}
          </template>
          <template #default-cell="{ row: { original: c } }">
            <span v-if="c.default === null" class="mono text-xs text-muted italic">NULL</span>
            <span v-else class="mono text-xs">{{ c.default }}</span>
          </template>
          <template #extra-cell="{ row: { original: c } }">
            <span class="mono text-xs">{{ c.extra }}</span>
          </template>
          <template #collation-cell="{ row: { original: c } }">
            <span class="mono text-xs text-muted">{{ c.collation }}</span>
          </template>
        </UTable>
      </PanelSection>

      <PanelSection v-if="!isView" :title="t('dbstudio.indexes')" flush>
        <UTable v-if="data.indexes.length" :data="data.indexes" :columns="idxColumns">
          <template #name-cell="{ row: { original: x } }">
            <span class="mono text-[13px] font-semibold text-highlighted">{{ x.name }}</span>
          </template>
          <template #columns-cell="{ row: { original: x } }">
            <span class="mono text-xs">{{ x.columns.map(c => c.length ? `${c.name}(${c.length})` : c.name).join(', ') }}</span>
          </template>
          <template #type-cell="{ row: { original: x } }">
            <span class="text-xs">{{ x.name === 'PRIMARY' ? 'PRIMARY' : x.unique ? 'UNIQUE' : 'INDEX' }} · {{ x.type }}</span>
          </template>
        </UTable>
        <UEmpty v-else size="sm" icon="i-cg-sort" :description="t('dbstudio.noIndexes')" />
      </PanelSection>

      <PanelSection v-if="data.foreign_keys.length" :title="t('dbstudio.foreignKeys')" flush>
        <UTable :data="data.foreign_keys" :columns="fkColumns">
          <template #name-cell="{ row: { original: fk } }">
            <span class="mono text-[13px] font-semibold text-highlighted">{{ fk.name }}</span>
          </template>
          <template #columns-cell="{ row: { original: fk } }">
            <span class="mono text-xs">{{ fk.columns.join(', ') }}</span>
          </template>
          <template #ref-cell="{ row: { original: fk } }">
            <span class="mono text-xs">{{ fk.ref_table }}({{ fk.ref_columns.join(', ') }})</span>
          </template>
          <template #rules-cell="{ row: { original: fk } }">
            <span class="text-xs">{{ fk.on_update }} / {{ fk.on_delete }}</span>
          </template>
        </UTable>
      </PanelSection>

      <UCollapsible>
        <UButton color="neutral" variant="outline" size="sm" icon="i-cg-code" trailing-icon="i-cg-chevron-down" :label="t('dbstudio.createStatement')" />
        <template #content>
          <pre class="mt-2 overflow-x-auto overflow-y-hidden bg-elevated p-3 font-mono text-xs whitespace-pre">{{ data.create_sql }}</pre>
        </template>
      </UCollapsible>
    </template>

    <UModal v-model:open="colOpen" :title="t('dbstudio.addColumn')" :description="t('dbstudio.alterHint')">
      <template #body>
        <form class="grid gap-3 sm:grid-cols-2" @submit.prevent="submitColumn">
          <UFormField :label="t('dbstudio.name')" class="sm:col-span-2">
            <UInput v-model="col.name" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField :label="t('dbstudio.type')">
            <USelect v-model="col.type" :items="COLUMN_TYPES" class="w-full" />
          </UFormField>
          <UFormField v-if="WITH_LENGTH.includes(col.type)" :label="col.type === 'DECIMAL' ? t('dbstudio.precision') : t('dbstudio.length')">
            <UInput v-model.number="col.length" type="number" min="1" class="w-full" />
          </UFormField>
          <UFormField v-if="col.type === 'DECIMAL'" :label="t('dbstudio.scale')">
            <UInput v-model.number="col.scale" type="number" min="0" class="w-full" />
          </UFormField>
          <UFormField :label="t('dbstudio.after')">
            <USelect v-model="col.after" :items="afterItems" class="w-full" />
          </UFormField>
          <div class="flex flex-wrap gap-4 sm:col-span-2">
            <USwitch v-model="col.nullable" :label="t('dbstudio.nullable')" />
            <USwitch v-if="NUMERIC.includes(col.type)" v-model="col.unsigned" label="UNSIGNED" />
            <USwitch v-model="col.hasDefault" :label="t('dbstudio.setDefault')" />
          </div>
          <UFormField v-if="col.hasDefault" :label="t('dbstudio.default')" :hint="t('dbstudio.defaultHint')" class="sm:col-span-2">
            <UInput v-model="col.defaultValue" class="w-full font-mono" />
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="colOpen = false" />
          <UButton :label="t('dbstudio.previewSql')" icon="i-cg-eye" :loading="busy" :disabled="!col.name.trim()" @click="submitColumn" />
        </div>
      </template>
    </UModal>

    <UModal v-model:open="idxOpen" :title="t('dbstudio.addIndex')" :description="t('dbstudio.alterHint')">
      <template #body>
        <div class="space-y-3">
          <UFormField :label="t('dbstudio.name')">
            <UInput v-model="idx.name" class="w-full font-mono" autofocus />
          </UFormField>
          <USwitch v-model="idx.unique" label="UNIQUE" />
          <UFormField :label="t('dbstudio.columns')">
            <USelectMenu v-model="idxColumnNames" :items="columnItems" multiple class="w-full" />
          </UFormField>
          <div v-for="c in idx.columns" :key="c.name" class="flex items-center gap-2">
            <span class="w-40 truncate font-mono text-xs">{{ c.name }}</span>
            <UInput v-model.number="c.length" type="number" min="0" size="sm" class="w-32" :placeholder="t('dbstudio.prefixLength')" />
          </div>
          <p v-if="idx.columns.length" class="text-xs text-muted">
            {{ t('dbstudio.prefixLengthHint') }}
          </p>
        </div>
      </template>
      <template #footer>
        <div class="flex w-full justify-end gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="idxOpen = false" />
          <UButton :label="t('dbstudio.previewSql')" icon="i-cg-eye" :loading="busy" :disabled="!idx.name.trim() || !idx.columns.length" @click="submitIndex" />
        </div>
      </template>
    </UModal>
  </div>
</template>
