<script setup lang="ts">
/*
 * Operations on one table: empty it (TRUNCATE) or drop it, each confirmed
 * by typing the table's name. Whether it runs is the grants': a read-only
 * user's is refused by the server, and the refusal is shown as it came.
 */
import type { DBAlterResult } from '~/types/dbstudio'

const props = defineProps<{ db: string, table: string, view?: boolean }>()
const { t } = useI18n()
const { api, tableBase, dbLink } = useStudio()
const { loadTables } = useStudioSchema()
const { busy, run } = useAction()
const confirm = useConfirm()

async function operate(kind: 'truncate' | 'drop') {
  const base = tableBase(props.db, props.table)
  let preview: DBAlterResult | null = null
  if (!await run(async () => {
    preview = await api.post<DBAlterResult>(`${base}/alter`, { kind, confirm: props.table, dry_run: true })
  })) return
  const ok = await confirm({
    title: kind === 'drop' ? t('studio.dropTitle', { name: props.table }) : t('studio.truncateTitle', { name: props.table }),
    description: `${kind === 'drop' ? t('studio.dropHint') : t('studio.truncateHint')} ${(preview as DBAlterResult | null)?.sql ?? ''}`,
    danger: true, typed: props.table, confirmLabel: kind === 'drop' ? t('studio.drop') : t('studio.truncate')
  })
  if (!ok) return
  const done = await run(() => api.post(`${base}/alter`, { kind, confirm: props.table }), kind === 'drop' ? t('studio.dropped') : t('studio.truncated'))
  if (!done) return
  await loadTables(props.db, true).catch(() => {})
  if (kind === 'drop') await navigateTo(dbLink(props.db))
}
</script>

<template>
  <PanelSection v-if="!view" :title="t('studio.truncate')" :description="t('studio.truncateHint')">
    <template #footer>
      <UButton color="warning" variant="soft" icon="i-cg-purge" :label="t('studio.truncate')" :loading="busy" @click="operate('truncate')" />
    </template>
  </PanelSection>
  <PanelSection :title="t('studio.drop')" :description="t('studio.dropHint')">
    <template #footer>
      <UButton color="error" icon="i-cg-trash" :label="t('studio.drop')" :loading="busy" @click="operate('drop')" />
    </template>
  </PanelSection>
</template>
