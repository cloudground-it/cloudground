<script setup lang="ts">
import SqlPreview from '~/components/dbstudio/SqlPreview.vue'
import RowEditor from '~/components/dbstudio/RowEditor.vue'
import StudioPager from '~/components/dbstudio/StudioPager.vue'
import type { DBChange, DBEditResult, DBFilter, DBFilterOp, DBRow, DBRows, DBSort } from '~/types/dbstudio'

/*
 * Browse: a page of a table's rows, sorted, filtered and searched on the
 * server; edit, insert and delete by primary key, each change previewed as
 * the SQL that runs. base is the table's Quarry API path; canWrite is
 * whether the panel lets the caller write (a database user's grants decide
 * on the server).
 */
const props = defineProps<{ base: string, canWrite: boolean, search?: string, startInsert?: boolean }>()
const { t } = useI18n()
const { api } = useStudio()
const { busy, run } = useAction()
const preview = useOverlay().create(SqlPreview)

const base = computed(() => props.base)
const page = ref(1)
const pageSize = ref(25)
const sort = ref<DBSort[]>([])
const filters = ref<DBFilter[]>([])
const data = ref<DBRows | null>(null)
const error = ref<string | null>(null)
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    data.value = await api.post<DBRows>(`${base.value}/rows`, {
      page: page.value - 1, page_size: pageSize.value, sort: sort.value, filters: filters.value, search: props.search ?? ''
    })
    if (props.startInsert && !started) {
      started = true
      openEdit(null)
    }
    error.value = null
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}
let started = false
void load()
watch([page, pageSize], load)
watch(() => props.search, () => {
  if (page.value !== 1) page.value = 1
  else void load()
})
watch([sort, filters], () => {
  if (page.value !== 1) page.value = 1
  else void load()
}, { deep: true })

// Sorting: a click cycles ascending → descending → off; shift-click adds a
// further key instead of replacing.
function sortState(col: string) {
  return sort.value.find(s => s.column === col)
}
function toggleSort(col: string, e: MouseEvent) {
  const cur = sortState(col)
  const state: DBSort | null = !cur ? { column: col, desc: false } : !cur.desc ? { column: col, desc: true } : null
  if (!e.shiftKey) {
    sort.value = state ? [state] : []
    return
  }
  const list = [...sort.value]
  const i = list.findIndex(s => s.column === col)
  if (i < 0) {
    if (state) list.push(state)
  } else if (state) list[i] = state
  else list.splice(i, 1)
  sort.value = list
}

// Filters.
const OPS: { value: DBFilterOp, label: string }[] = [
  { value: 'eq', label: '=' }, { value: 'ne', label: '≠' }, { value: 'lt', label: '<' }, { value: 'le', label: '≤' },
  { value: 'gt', label: '>' }, { value: 'ge', label: '≥' }, { value: 'contains', label: 'contains' },
  { value: 'starts', label: 'starts' }, { value: 'null', label: 'IS NULL' }, { value: 'notnull', label: 'IS NOT NULL' }
]
const opLabel = (op: DBFilterOp) => op === 'contains' ? t('dbstudio.opContains') : op === 'starts' ? t('dbstudio.opStarts') : OPS.find(o => o.value === op)?.label ?? op
const opItems = computed(() => OPS.map(o => ({ value: o.value, label: opLabel(o.value) })))
const draft = reactive<DBFilter>({ column: '', op: 'eq', value: '' })
const columnNames = computed(() => (data.value?.columns ?? []).map(c => c.name))
const noValue = (op: DBFilterOp) => op === 'null' || op === 'notnull'
function addFilter() {
  if (!draft.column) return
  filters.value = [...filters.value, { column: draft.column, op: draft.op, value: noValue(draft.op) ? '' : draft.value }]
  draft.value = ''
}
function removeFilter(i: number) {
  filters.value = filters.value.filter((_, j) => j !== i)
}

// Editing.
const pk = computed(() => data.value?.primary_key ?? [])
const editable = computed(() => props.canWrite && pk.value.length > 0)
const editorOpen = ref(false)
const editRow = ref<DBRow | null>(null)
const editFocus = ref<number | undefined>(undefined)

function keyOf(row: DBRow): Record<string, string> | null {
  const cols = data.value?.columns ?? []
  const key: Record<string, string> = {}
  for (const name of pk.value) {
    const i = cols.findIndex(c => c.name === name)
    const v = row.v[i]
    if (i < 0 || v === null || v === undefined || row.cut?.includes(i) || cols[i]?.binary) return null
    key[name] = v
  }
  return key
}
const isCut = (row: DBRow, i: number) => !!row.cut?.includes(i)

function openEdit(row: DBRow | null, focus?: number) {
  // A new row needs no key; an edit does.
  if (!props.canWrite || (row && !editable.value)) return
  if (row && !keyOf(row)) return
  if (row && focus !== undefined && (isCut(row, focus) || data.value?.columns[focus]?.binary)) return
  editRow.value = row
  editFocus.value = focus
  editorOpen.value = true
}

async function change(c: DBChange, success: string): Promise<boolean> {
  let res: DBEditResult | null = null
  const ok = await run(async () => {
    res = await api.post<DBEditResult>(`${base.value}/edit`, { changes: [c], dry_run: true })
  })
  if (!ok || !res) return false
  const confirmed = await preview.open({
    title: t('dbstudio.changePreview'), description: t('dbstudio.changePreviewHint'),
    sql: (res as DBEditResult).sql.join(';\n') + ';', danger: c.op === 'delete'
  })
  if (confirmed !== true) return false
  if (await run(() => api.post(`${base.value}/edit`, { changes: [c], dry_run: false }), success)) {
    await load()
    return true
  }
  return false
}

async function submitEdit(values: Record<string, string | null>) {
  const row = editRow.value
  if (row) {
    if (!Object.keys(values).length) {
      editorOpen.value = false
      return
    }
    const key = keyOf(row)
    if (!key) return
    if (await change({ op: 'update', key, values }, t('dbstudio.rowUpdated'))) editorOpen.value = false
  } else if (await change({ op: 'insert', values }, t('dbstudio.rowInserted'))) {
    editorOpen.value = false
  }
}
async function removeRow(row: DBRow) {
  const key = keyOf(row)
  if (key) await change({ op: 'delete', key }, t('dbstudio.rowDeleted'))
}

// The sticky header row: a copy of the table's own (hidden) header, its
// columns as wide as the table lays them out, scrolled sideways with it.
const headEl = ref<HTMLElement | null>(null)
const bodyEl = ref<HTMLElement | null>(null)
const tableEl = ref<HTMLTableElement | null>(null)
const widths = reactive<{ table: number, cols: number[] }>({ table: 0, cols: [] })
function measure() {
  const tb = tableEl.value
  if (!tb) return
  const cells = [...tb.querySelectorAll<HTMLTableCellElement>('thead th')]
  widths.cols = cells.map(c => c.getBoundingClientRect().width)
  widths.table = tb.getBoundingClientRect().width
}
let syncing = false
function syncFrom(from: 'head' | 'body') {
  if (syncing) return
  const [src, dst] = from === 'head' ? [headEl.value, bodyEl.value] : [bodyEl.value, headEl.value]
  if (!src || !dst || dst.scrollLeft === src.scrollLeft) return
  syncing = true
  dst.scrollLeft = src.scrollLeft
  requestAnimationFrame(() => { syncing = false })
}
let resize: ResizeObserver | null = null
watch(tableEl, (el, old) => {
  if (old) resize?.unobserve(old)
  if (!el) return
  resize ??= new ResizeObserver(() => measure())
  resize.observe(el)
  measure()
})
watch(data, () => void nextTick(() => {
  measure()
  syncFrom('body')
}), { flush: 'post' })
onBeforeUnmount(() => resize?.disconnect())
</script>

<template>
  <div class="space-y-3">
    <div class="flex flex-wrap items-end gap-2">
      <USelect v-model="draft.column" :items="columnNames" :placeholder="t('dbstudio.column')" class="w-44" size="sm" />
      <USelect v-model="draft.op" :items="opItems" class="w-36" size="sm" />
      <UInput v-if="!noValue(draft.op)" v-model="draft.value" size="sm" class="w-48 font-mono" :placeholder="t('dbstudio.value')" @keydown.enter="addFilter" />
      <UButton size="sm" color="neutral" variant="outline" icon="i-cg-filter" :label="t('dbstudio.addFilter')" :disabled="!draft.column" @click="addFilter" />
      <div class="ms-auto flex items-center gap-2">
        <UButton v-if="canWrite && data?.columns.length" size="sm" icon="i-cg-plus" :label="t('dbstudio.insertRow')" @click="openEdit(null)" />
        <UButton size="sm" color="neutral" variant="ghost" icon="i-cg-refresh" :aria-label="t('common.refresh')" :loading="loading" @click="load" />
      </div>
    </div>
    <div v-if="filters.length || sort.length" class="flex flex-wrap items-center gap-2">
      <UBadge v-for="(f, i) in filters" :key="`f${i}`" color="neutral" variant="outline" class="gap-1 font-mono">
        {{ f.column }} {{ opLabel(f.op) }} <template v-if="!noValue(f.op)">"{{ f.value }}"</template>
        <UButton size="xs" color="neutral" variant="link" icon="i-cg-x" :aria-label="t('common.remove')" class="p-0" @click="removeFilter(i)" />
      </UBadge>
      <UBadge v-for="s in sort" :key="`s${s.column}`" color="info" variant="outline" class="font-mono">
        {{ s.column }} {{ s.desc ? 'DESC' : 'ASC' }}
      </UBadge>
      <UButton size="xs" color="neutral" variant="link" :label="t('dbstudio.clearAll')" @click="filters = []; sort = []" />
    </div>

    <UAlert v-if="error" color="error" variant="subtle" :description="error" />
    <UAlert v-else-if="data && !pk.length && canWrite" color="neutral" variant="subtle" icon="i-cg-info" :description="t('dbstudio.noPrimaryKey')" />

    <StudioPager v-if="data" v-model:page="page" v-model:size="pageSize" :total="data.total" :estimated="data.total_estimated" :shown="data.rows.length" sizes />

    <!--
      The rows are laid out in the page, which scrolls: no box of its own
      scrolls up and down. A wide table scrolls sideways in its frame, and
      its header row is a copy that sticks under the top bar (a sticky
      header inside a sideways scroller would stick to that scroller, not
      the page), sized from the table's own hidden header and kept in step
      with its sideways scroll.
    -->
    <div v-if="data" class="border border-default">
      <div
        ref="headEl"
        class="sticky top-(--studio-head) z-10 overflow-x-auto overflow-y-hidden border-b border-default bg-elevated [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
        @scroll="syncFrom('head')"
      >
        <table class="table-fixed text-xs" :style="{ width: `${widths.table}px` }">
          <colgroup>
            <col v-for="(w, i) in widths.cols" :key="i" :style="{ width: `${w}px` }">
          </colgroup>
          <thead class="text-left">
            <tr>
              <th v-if="editable" class="px-2 py-2" />
              <th v-for="c in data.columns" :key="c.name" class="overflow-hidden px-3 py-2 font-medium whitespace-nowrap">
                <button type="button" class="inline-flex items-center gap-1 hover:text-highlighted" :title="`${c.type}${c.comment ? ' — ' + c.comment : ''}`" @click="toggleSort(c.name, $event)">
                  <span class="font-mono">{{ c.name }}</span>
                  <UIcon v-if="pk.includes(c.name)" name="i-cg-key" class="size-3 text-primary" />
                  <UIcon v-if="sortState(c.name)" :name="sortState(c.name)?.desc ? 'i-cg-arrow-down' : 'i-cg-arrow-up'" class="size-3" />
                </button>
              </th>
            </tr>
          </thead>
        </table>
      </div>
      <div ref="bodyEl" class="overflow-x-auto overflow-y-hidden" @scroll="syncFrom('body')">
        <table ref="tableEl" class="w-full text-xs">
          <thead aria-hidden="true" class="invisible text-left leading-none">
            <tr>
              <th v-if="editable" class="w-16 px-2 py-0">
                <div class="h-0 overflow-hidden" />
              </th>
              <th v-for="c in data.columns" :key="c.name" class="px-3 py-0 font-medium whitespace-nowrap">
                <div class="flex h-0 items-center gap-1 overflow-hidden">
                  <span class="font-mono">{{ c.name }}</span>
                  <span v-if="pk.includes(c.name)" class="inline-block size-3" />
                  <span v-if="sortState(c.name)" class="inline-block size-3" />
                </div>
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-default font-mono">
            <tr v-for="(row, ri) in data.rows" :key="ri" class="hover:bg-elevated/50">
              <td v-if="editable" class="px-2 py-1 whitespace-nowrap">
                <template v-if="keyOf(row)">
                  <UButton size="xs" color="neutral" variant="ghost" icon="i-cg-edit" :aria-label="t('dbstudio.editRow')" @click="openEdit(row)" />
                  <UButton size="xs" color="error" variant="ghost" icon="i-cg-trash" :aria-label="t('dbstudio.deleteRow')" @click="removeRow(row)" />
                </template>
              </td>
              <td
                v-for="(cell, ci) in row.v"
                :key="ci"
                class="max-w-80 truncate px-3 py-1.5"
                :title="isCut(row, ci) ? t('dbstudio.cutLocked') : data.columns[ci]?.binary ? t('dbstudio.binaryLocked') : (cell ?? 'NULL')"
                @dblclick="openEdit(row, ci)"
              >
                <span v-if="cell === null" class="text-muted italic">NULL</span>
                <template v-else>
                  {{ cell }}<span v-if="isCut(row, ci)" class="text-muted">…</span>
                </template>
              </td>
            </tr>
            <tr v-if="!data.rows.length">
              <td :colspan="data.columns.length + (editable ? 1 : 0)" class="px-3 py-6 text-center font-sans text-muted">
                {{ t('dbstudio.noRows') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <StudioPager v-if="data && data.rows.length > 10" v-model:page="page" v-model:size="pageSize" :total="data.total" :estimated="data.total_estimated" :shown="data.rows.length" />

    <UCollapsible v-if="data?.sql">
      <UButton color="neutral" variant="ghost" size="sm" icon="i-cg-code" trailing-icon="i-cg-chevron-down" :label="t('dbstudio.generatedSql')" />
      <template #content>
        <pre class="mt-2 overflow-x-auto rounded-md bg-elevated p-3 font-mono text-xs whitespace-pre-wrap break-all">{{ data.sql }}</pre>
      </template>
    </UCollapsible>
    <p v-if="editable" class="text-xs text-muted">
      {{ t('dbstudio.editHint') }}
    </p>

    <RowEditor
      v-if="data"
      v-model:open="editorOpen"
      :columns="data.columns"
      :row="editRow?.v ?? null"
      :cut="editRow?.cut"
      :focus="editFocus"
      :busy="busy"
      @submit="submitEdit"
    />
  </div>
</template>
