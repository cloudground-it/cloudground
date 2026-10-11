<script setup lang="ts">
/*
 * Quarry's SQL editor: the file manager's CodeMirror editor, given the
 * database's tables and columns for completion and Mod-Enter from here (the
 * view is found from its DOM node once it has mounted). Opened from the
 * panel it is read-only unless writes are allowed; a database user's
 * statement runs as that user, and the server's grants decide. History is
 * per user and per site, in this browser only: statements can hold data
 * that must not sit on the server.
 */
import { Compartment, EditorState, Prec, StateEffect } from '@codemirror/state'
import { EditorView, keymap } from '@codemirror/view'
import { MariaSQL, schemaCompletionSource } from '@codemirror/lang-sql'
import type { DBQueryResult, DBSimulateResult, DBStudioTables } from '~/types/dbstudio'

const props = defineProps<{ db: string, initial?: string }>()
const { t, locale } = useI18n()
const { api, session, dbBase, canWrite } = useStudio()
const { busy, run } = useAction()
const confirm = useConfirm()

const login = computed(() => session.value?.mode === 'login')
const allowWrites = ref(false)
const result = ref<DBQueryResult | null>(null)
const ranSQL = ref('')
const wrap = ref<HTMLElement | null>(null)
let view: EditorView | null = null
const schemaConf = new Compartment()

interface Entry { sql: string, at: number }
const historyKey = computed(() => `cg:dbstudio:history:${session.value?.user ?? ''}:${session.value?.site.id ?? 0}`)
const history = ref<Entry[]>([])
const historyAll = ref(false)
function loadHistory() {
  try {
    const raw = localStorage.getItem(historyKey.value)
    history.value = raw ? (JSON.parse(raw) as Entry[]).filter(e => typeof e?.sql === 'string').slice(0, 50) : []
  } catch {
    history.value = []
  }
}
function remember(sql: string) {
  history.value = [{ sql, at: Date.now() }, ...history.value.filter(e => e.sql !== sql)].slice(0, 50)
  try {
    localStorage.setItem(historyKey.value, JSON.stringify(history.value))
  } catch {
    // Storage full or blocked: the history lives for this page only.
  }
}
function clearHistory() {
  history.value = []
  try {
    localStorage.removeItem(historyKey.value)
  } catch {
    // Nothing to do.
  }
}
onMounted(loadHistory)

const text = () => view?.state.doc.toString() ?? ''
function setText(s: string) {
  if (!view) return
  view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: s }, selection: { anchor: s.length } })
  view.focus()
}

async function execute(explain = false): Promise<boolean> {
  const sql = text().trim()
  if (!sql || busy.value) return false
  const writes = !login.value && allowWrites.value && !explain
  if (writes) {
    const ok = await confirm({ title: t('db.writeTitle'), description: t('db.writeHint'), danger: true, confirmLabel: t('db.run') })
    if (!ok) return false
  }
  return run(async () => {
    result.value = await api.post<DBQueryResult>(`${dbBase(props.db)}/query`, { sql, allow_writes: writes, explain })
    simulated.value = null
    ranSQL.value = explain ? `EXPLAIN ${sql}` : sql
    remember(sql)
  })
}

// Simulate, as phpMyAdmin has it: the statement runs in a transaction the
// agent always rolls back, and says how many rows it would change (and,
// for an UPDATE or DELETE, shows up to 25 of them as they are now). What
// cannot be rolled back is refused, with the reason. Nothing stays, so it
// asks for no confirmation and needs no "allow writes".
const simulated = ref<DBSimulateResult | null>(null)
const canSimulate = computed(() => login.value || canWrite.value)
async function simulate() {
  const sql = text().trim()
  if (!sql || busy.value) return
  await run(async () => {
    simulated.value = await api.post<DBSimulateResult>(`${dbBase(props.db)}/simulate`, { sql })
    result.value = null
    ranSQL.value = sql
    remember(sql)
  })
}
const save = async () => {
  await execute()
  return true
}

const { data: schema } = useLoad(() => api.get<DBStudioTables>(`${dbBase(props.db)}/tables?columns=1`))
function schemaExtension() {
  const cols = schema.value?.columns ?? {}
  const s: Record<string, string[]> = {}
  for (const tb of schema.value?.tables ?? []) s[tb.name] = cols[tb.name] ?? []
  for (const [name, list] of Object.entries(cols)) s[name] ??= list
  return EditorState.languageData.of(() => [{ autocomplete: schemaCompletionSource({ dialect: MariaSQL, schema: s, upperCaseKeywords: true }) }])
}
function attach() {
  const el = wrap.value?.querySelector<HTMLElement>('.cm-editor')
  const v = el ? EditorView.findFromDOM(el) : null
  if (!v) return false
  view = v
  v.dispatch({
    effects: StateEffect.appendConfig.of([
      schemaConf.of(schemaExtension()),
      Prec.highest(keymap.of([{ key: 'Mod-Enter', run: () => { void execute(); return true } }]))
    ])
  })
  return true
}
let observer: MutationObserver | null = null
onMounted(() => {
  if (attach() || !wrap.value) return
  observer = new MutationObserver(() => {
    if (attach()) {
      observer?.disconnect()
      observer = null
    }
  })
  observer.observe(wrap.value, { childList: true, subtree: true })
})
onBeforeUnmount(() => {
  observer?.disconnect()
  view = null
})
watch(schema, () => {
  view?.dispatch({ effects: schemaConf.reconfigure(schemaExtension()) })
})
</script>

<template>
  <div class="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_18rem]">
    <PanelSection :title="t('studio.tab.sql')">
      <div ref="wrap" class="h-72 overflow-hidden border border-default">
        <LazyCodeEditor :content="initial || 'SELECT * FROM '" filename="query.sql" :save="save" />
      </div>
      <USwitch v-if="!login && canWrite" v-model="allowWrites" :label="t('db.allowWrites')" />
      <span v-else-if="login" class="text-[13px] text-muted">{{ t('studio.sqlAsUser', { user: session?.user ?? '' }) }}</span>
      <span v-else class="text-[13px] text-muted">{{ t('dbstudio.readOnlyAccount') }}</span>
      <p class="m-0 text-xs text-muted">
        {{ t('dbstudio.sqlLimits') }}
        <template v-if="canSimulate">
          {{ t('studio.simulateLimits') }}
        </template>
      </p>
      <template #footer>
        <UButton icon="i-cg-play" :label="t('db.run')" :loading="busy" @click="execute()" />
        <UButton color="neutral" variant="outline" icon="i-cg-list-checks" :label="t('studio.explain')" :title="t('studio.explainHint')" :loading="busy" @click="execute(true)" />
        <UButton
          v-if="canSimulate"
          data-simulate
          color="neutral"
          variant="outline"
          icon="i-cg-rollback"
          :label="t('studio.simulate')"
          :title="t('studio.simulateHint')"
          :loading="busy"
          @click="simulate"
        />
      </template>
    </PanelSection>
    <PanelSection :title="t('dbstudio.history')" flush>
      <ul v-if="history.length" class="m-0 list-none p-2">
        <li v-for="h in (historyAll ? history : history.slice(0, 10))" :key="h.at + h.sql">
          <button
            type="button"
            class="block w-full truncate px-2 py-1 text-left font-mono text-xs text-default hover:bg-elevated"
            :title="h.sql"
            @click="setText(h.sql)"
          >
            {{ h.sql }}
          </button>
        </li>
      </ul>
      <UEmpty v-else size="sm" icon="i-cg-history" :description="t('dbstudio.historyEmpty')" />
      <template v-if="history.length" #footer>
        <UButton v-if="history.length > 10 && !historyAll" size="xs" color="neutral" variant="link" icon="i-cg-chevron-down" :label="t('studio.historyMore', { n: history.length - 10 })" @click="historyAll = true" />
        <UButton size="xs" color="neutral" variant="link" icon="i-cg-trash" :label="t('dbstudio.clearHistory')" @click="clearHistory" />
      </template>
    </PanelSection>
  </div>

    <PanelSection v-if="simulated" data-simulate-result :title="t('studio.simulateResult')">
      <div class="flex min-w-0 flex-wrap items-center gap-2 text-xs text-muted">
        <UBadge size="sm" color="neutral" variant="subtle" icon="i-cg-rollback" :label="t('studio.simulated')" />
        <code class="min-w-0 truncate font-mono">{{ ranSQL }}</code>
      </div>
      <UAlert v-if="simulated.refused" color="warning" variant="subtle" icon="i-cg-alert-triangle" :title="t('studio.simulateRefused')" :description="simulated.refused" />
      <UAlert v-else-if="simulated.message" color="error" variant="subtle" :title="simulated.rolled_back ? t('studio.simulateErrorRolledBack') : undefined" :description="simulated.message" />
      <template v-else>
        <UAlert
          color="info"
          variant="subtle"
          icon="i-cg-check-circle"
          :title="t('studio.simulateAffected', { n: fmtNumber(simulated.affected, undefined, locale) }, simulated.affected)"
          :description="t('studio.simulateRolledBack')"
        />
        <template v-if="simulated.preview?.columns?.length">
          <div class="flex flex-wrap items-baseline justify-between gap-2">
            <span class="text-[13px] text-muted">{{ t('studio.simulatePreview', { n: simulated.preview.rows?.length ?? 0 }) }}</span>
            <code class="min-w-0 truncate font-mono text-xs text-muted" :title="simulated.preview_sql">{{ simulated.preview_sql }}</code>
          </div>
          <div class="overflow-x-auto overflow-y-hidden border border-default">
            <table class="w-full text-xs">
              <thead class="bg-elevated text-left">
                <tr>
                  <th v-for="(c, i) in simulated.preview.columns" :key="i" class="px-3 py-2 font-medium whitespace-nowrap">
                    {{ c }}
                  </th>
                </tr>
              </thead>
              <tbody class="divide-y divide-default font-mono">
                <tr v-for="(row, i) in simulated.preview.rows ?? []" :key="i">
                  <td v-for="(cell, j) in row" :key="j" class="max-w-80 truncate px-3 py-1.5" :title="cell">
                    {{ cell }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </template>
    </PanelSection>

    <PanelSection v-if="result" :title="t('ux.studio.result')">
      <div class="flex min-w-0 flex-wrap items-center gap-2 text-xs text-muted">
        <UBadge size="sm" :color="result.read_only ? 'neutral' : 'warning'" variant="subtle" :label="result.read_only ? t('dbstudio.ranReadOnly') : (login ? t('studio.ranAsUser') : t('dbstudio.ranWrite'))" />
        <code class="min-w-0 truncate font-mono">{{ ranSQL }}</code>
      </div>
      <UAlert v-if="result.message" :color="result.message.startsWith('ERROR') ? 'error' : 'info'" variant="subtle" :description="result.message" />
      <UAlert v-if="result.truncated" color="warning" variant="subtle" :description="t('dbstudio.truncated')" />
      <div v-if="result.columns?.length" class="overflow-x-auto overflow-y-hidden border border-default">
        <table class="w-full text-xs">
          <thead class="sticky top-0 bg-elevated text-left">
            <tr>
              <th v-for="(c, i) in result.columns" :key="i" class="px-3 py-2 font-medium whitespace-nowrap">
                {{ c }}
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-default font-mono">
            <tr v-for="(row, i) in result.rows ?? []" :key="i">
              <td v-for="(cell, j) in row" :key="j" class="max-w-80 truncate px-3 py-1.5" :title="cell">
                {{ cell }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </PanelSection>
</template>
