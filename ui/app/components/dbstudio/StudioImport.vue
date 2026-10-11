<script setup lang="ts">
/*
 * Import: a .sql or .csv file uploaded in 8 MiB chunks into the site's
 * shared/imports (the panel's proxy takes 16 MB a request), then imported
 * as a task. An SQL import is confirmed by typing the database's name;
 * opened from the panel it keeps a rollback point, as a database user it
 * runs as that user, statement by statement. A CSV file goes into one
 * table, in one transaction.
 */
import type { DBStudioFile } from '~/types/dbstudio'
import type { StudioTask } from '~/composables/useStudioFiles'
import StudioTasks from '~/components/dbstudio/StudioTasks.vue'

const props = defineProps<{ db: string, table?: string }>()
const { t, locale } = useI18n()
const { api, dbBase, session } = useStudio()
const { tables, loadTables } = useStudioSchema()
const { busy, run } = useAction()
const confirm = useConfirm()
const { files, refresh, tasks, follow } = useStudioFiles()

onMounted(() => void loadTables(props.db).catch(() => {}))
const tableNames = computed(() => (tables.value[props.db]?.tables ?? []).filter(x => x.type === 'table').map(x => x.name))
const login = computed(() => session.value?.mode === 'login')

const CHUNK = 8 * 1024 * 1024
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.(sql|csv)$/
const picked = ref<File | null>(null)
const uploaded = ref(0)
const uploading = ref(false)
let abort: AbortController | null = null
const progress = computed(() => picked.value?.size ? Math.round(uploaded.value * 100 / picked.value.size) : 0)

function safeName(name: string): string {
  const m = /\.(sql|csv)$/i.exec(name)
  if (!m) return ''
  const stem = name.slice(0, -m[0].length).replace(/[^A-Za-z0-9._-]/g, '-').replace(/^[^A-Za-z0-9]+/, '').slice(0, 110) || 'import'
  return `${stem}.${m[1]!.toLowerCase()}`
}
function onPick(e: Event) {
  picked.value = (e.target as HTMLInputElement).files?.[0] ?? null
  uploaded.value = 0
}
const pickedName = computed(() => picked.value ? safeName(picked.value.name) : '')

async function upload() {
  const f = picked.value
  const name = pickedName.value
  if (!f || !NAME_RE.test(name)) return
  uploading.value = true
  abort = new AbortController()
  uploaded.value = 0
  const ok = await run(async () => {
    for (let offset = 0; offset < f.size || offset === 0; offset += CHUNK) {
      const resp = await fetch(`${STUDIO_API}/import/upload?name=${encodeURIComponent(name)}&offset=${offset}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream', 'Accept': 'application/json' },
        body: f.slice(offset, offset + CHUNK),
        credentials: 'same-origin',
        signal: abort!.signal
      })
      if (!resp.ok) {
        let msg = `HTTP ${resp.status}`
        try {
          const body = await resp.json() as { error?: string }
          if (body.error) msg = body.error
        } catch {
          // Not JSON: keep the status.
        }
        throw new Error(msg)
      }
      uploaded.value = Math.min(offset + CHUNK, f.size)
      if (f.size === 0) break
    }
  }, t('dbstudio.uploaded'))
  uploading.value = false
  abort = null
  if (ok) {
    picked.value = null
    await refresh()
  }
}

const csvTarget = ref(props.table ?? '')
async function importFile(f: DBStudioFile) {
  const base = dbBase(props.db)
  if (f.name.endsWith('.sql')) {
    const ok = await confirm({
      title: t('db.importTitle'),
      description: login.value ? t('studio.importAsUser', { name: f.name, user: session.value?.user ?? '' }) : t('dbstudio.importSqlHint', { name: f.name }),
      danger: true, typed: props.db, confirmLabel: t('db.import')
    })
    if (ok) {
      await run(async () => {
        follow((await api.post<{ task: StudioTask }>(`${base}/import`, { name: f.name, format: 'sql', confirm: props.db })).task)
      }, t('db.importStarted'))
    }
    return
  }
  if (!csvTarget.value) {
    useToast().add({ title: t('dbstudio.chooseTable'), color: 'warning', icon: 'i-cg-alert-triangle' })
    return
  }
  const ok = await confirm({ title: t('dbstudio.importCsvTitle', { table: csvTarget.value }), description: t('dbstudio.importCsvHint', { name: f.name }), confirmLabel: t('db.import') })
  if (ok) {
    await run(async () => {
      follow((await api.post<{ task: StudioTask }>(`${base}/import`, { name: f.name, format: 'csv', table: csvTarget.value })).task)
    }, t('db.importStarted'))
  }
}
async function remove(f: DBStudioFile) {
  const ok = await confirm({ title: t('files.deleteTitle', { name: f.name }), description: t('files.deleteFile'), danger: true, confirmLabel: t('common.delete') })
  if (ok && await run(() => api.del(`${STUDIO_API}/files/imports/${encodeURIComponent(f.name)}`), t('files.deleted'))) await refresh()
}
const hasCsv = computed(() => (files.value?.imports ?? []).some(f => f.name.endsWith('.csv')))
</script>

<template>
  <PanelSection :title="t('dbstudio.upload')" :description="t('dbstudio.uploadHint')">
    <UAlert v-if="login" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('studio.importNoRollback')" />
    <!-- A plain file input: the upload reads the change event as it is. -->
    <input type="file" accept=".sql,.csv" class="block w-full max-w-xl text-sm text-default file:me-3 file:border file:border-default file:bg-elevated file:px-3 file:py-2 file:font-mono file:text-[11px] file:tracking-[0.14em] file:text-highlighted file:uppercase" :disabled="uploading" :aria-label="t('dbstudio.upload')" @change="onPick">
    <p v-if="picked && !pickedName" class="m-0 text-sm text-error">
      {{ t('dbstudio.badFileType') }}
    </p>
    <p v-else-if="picked" class="m-0 text-xs text-muted">
      {{ t('dbstudio.uploadAs', { name: pickedName, size: fmtBytes(picked.size, locale) }) }}
    </p>
    <UProgress v-if="uploading || uploaded" :model-value="progress" class="max-w-xl" />
    <template #footer>
      <UButton icon="i-cg-upload" :label="t('dbstudio.upload')" :loading="uploading" :disabled="!picked || !pickedName" @click="upload" />
      <UButton v-if="uploading" color="neutral" variant="outline" :label="t('common.cancel')" @click="abort?.abort()" />
    </template>
  </PanelSection>
  <StudioTasks :tasks="tasks" />
  <PanelSection :title="t('dbstudio.imports')" :description="t('dbstudio.importsHint')" flush>
    <div v-if="hasCsv" class="px-4 py-4 sm:px-5">
      <UFormField :label="t('dbstudio.csvTarget')" class="max-w-sm">
        <USelect v-model="csvTarget" :items="tableNames" class="w-full" />
      </UFormField>
    </div>
    <UEmpty v-if="!files?.imports?.length" size="sm" icon="i-cg-import" :description="t('dbstudio.noFiles')" />
    <ul v-else class="m-0 list-none divide-y divide-default border-t border-default p-0" :class="hasCsv ? '' : 'border-t-0'">
      <li v-for="f in files.imports" :key="f.name" class="flex flex-wrap items-center gap-3 px-4 py-2.5 sm:px-5">
        <UIcon name="i-cg-file" class="size-4 shrink-0 text-muted" />
        <span class="mono min-w-0 flex-1 truncate text-[13px] text-highlighted">{{ f.name }}</span>
        <span class="text-xs text-muted">{{ fmtBytes(f.size, locale) }} · {{ fmtDateTime(f.modified, locale) }}</span>
        <span class="flex items-center gap-1">
          <UButton size="sm" color="warning" variant="soft" icon="i-cg-import" :label="t('db.import')" :disabled="busy" @click="importFile(f)" />
          <UButton size="sm" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.delete')" @click="remove(f)" />
        </span>
      </li>
    </ul>
  </PanelSection>
</template>
