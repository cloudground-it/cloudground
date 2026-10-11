<script setup lang="ts">
/*
 * Export: the database, some of its tables (SQL) or one table (CSV),
 * written by the agent into the site's shared/exports as a task, then
 * downloaded as a stream from the list. A database user exports what it
 * may read, and sees only the exports it made.
 */
import type { DBStudioFile } from '~/types/dbstudio'
import type { StudioTask } from '~/composables/useStudioFiles'
import StudioTasks from '~/components/dbstudio/StudioTasks.vue'

const props = defineProps<{ db: string, table?: string }>()
const { t, locale } = useI18n()
const { api, dbBase } = useStudio()
const { tables, loadTables } = useStudioSchema()
const { busy, run } = useAction()
const confirm = useConfirm()
const { files, refresh, tasks, follow } = useStudioFiles()

onMounted(() => void loadTables(props.db).catch(() => {}))
const tableNames = computed(() => (tables.value[props.db]?.tables ?? []).filter(x => x.type === 'table').map(x => x.name))
const format = ref<'sql' | 'csv'>('sql')
const formatItems = [{ label: 'SQL', value: 'sql' }, { label: 'CSV', value: 'csv' }]
const sqlTables = ref<string[]>(props.table ? [props.table] : [])
const csvTable = ref(props.table ?? '')

const start = () => run(async () => {
  const r = await api.post<{ task: StudioTask }>(`${dbBase(props.db)}/export`, {
    format: format.value, tables: format.value === 'csv' ? [csvTable.value] : sqlTables.value
  })
  follow(r.task)
}, t('dbstudio.exportStarted'))
const downloadURL = (f: DBStudioFile) => `${STUDIO_API}/exports/${encodeURIComponent(f.name)}`
async function remove(f: DBStudioFile) {
  const ok = await confirm({ title: t('files.deleteTitle', { name: f.name }), description: t('files.deleteFile'), danger: true, confirmLabel: t('common.delete') })
  if (ok && await run(() => api.del(`${STUDIO_API}/files/exports/${encodeURIComponent(f.name)}`), t('files.deleted'))) await refresh()
}
</script>

<template>
  <PanelSection :title="t('dbstudio.export')" :description="t('dbstudio.exportHint')">
    <div class="flex max-w-xl flex-col gap-4">
      <UFormField :label="t('dbstudio.format')">
        <URadioGroup v-model="format" :items="formatItems" orientation="horizontal" />
      </UFormField>
      <UFormField v-if="format === 'sql'" :label="t('dbstudio.tables')" :hint="t('dbstudio.allTablesHint')">
        <USelectMenu v-model="sqlTables" :items="tableNames" multiple class="w-full" :placeholder="t('dbstudio.allTables')" />
      </UFormField>
      <UFormField v-else :label="t('dbstudio.table')" class="max-w-sm">
        <USelect v-model="csvTable" :items="tableNames" class="w-full" />
      </UFormField>
    </div>
    <template #footer>
      <UButton icon="i-cg-export" :label="t('dbstudio.startExport')" :loading="busy" :disabled="format === 'csv' && !csvTable" @click="start" />
    </template>
  </PanelSection>
  <StudioTasks :tasks="tasks" />
  <PanelSection :title="t('dbstudio.exports')" flush>
    <UEmpty v-if="!files?.exports?.length" size="sm" icon="i-cg-export" :description="t('dbstudio.noFiles')" />
    <ul v-else class="m-0 list-none divide-y divide-default p-0">
      <li v-for="f in files.exports" :key="f.name" class="flex flex-wrap items-center gap-3 px-4 py-2.5 sm:px-5">
        <UIcon name="i-cg-file" class="size-4 shrink-0 text-muted" />
        <span class="mono min-w-0 flex-1 truncate text-[13px] text-highlighted">{{ f.name }}</span>
        <span class="text-xs text-muted">{{ fmtBytes(f.size, locale) }} · {{ fmtDateTime(f.modified, locale) }}</span>
        <span class="flex items-center gap-1">
          <UButton size="sm" color="neutral" variant="outline" icon="i-cg-download" :label="t('files.download')" :to="downloadURL(f)" external download />
          <UButton size="sm" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.delete')" @click="remove(f)" />
        </span>
      </li>
    </ul>
  </PanelSection>
</template>
