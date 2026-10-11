<script setup lang="ts">
/*
 * Edits one row (or a new one): a field per column, each with a NULL
 * switch; on insert a column can also be left to its default. Emits the
 * values that changed (update) or were set (insert); the parent previews
 * the SQL and runs it.
 */
import type { DBColumn } from '~/types/dbstudio'

const props = defineProps<{
  columns: DBColumn[]
  /** The row's current values, or null for a new row. */
  row: (string | null)[] | null
  /** Indexes of cells cut for display: not editable. */
  cut?: number[]
  focus?: number
  busy?: boolean
}>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ submit: [values: Record<string, string | null>] }>()
const { t } = useI18n()

interface Field { value: string, isNull: boolean, omit: boolean, locked: boolean, reason: string }
const fields = ref<Field[]>([])

watch(open, (v) => {
  if (!v) return
  fields.value = props.columns.map((c, i) => {
    const cur = props.row ? props.row[i] ?? null : null
    const cutCell = !!props.cut?.includes(i)
    const locked = !!c.binary || cutCell
    const reason = c.binary ? t('dbstudio.binaryLocked') : cutCell ? t('dbstudio.cutLocked') : ''
    if (!props.row) {
      // A new row: auto-increment and defaulted columns start omitted.
      const omit = locked || (c.extra ?? '').includes('auto_increment') || c.default !== null || c.nullable
      return { value: '', isNull: false, omit, locked, reason }
    }
    return { value: cur ?? '', isNull: cur === null, omit: false, locked, reason }
  })
}, { immediate: true })

const multiline = (c: DBColumn) => /text|json|blob/i.test(c.data_type)

function submit() {
  const out: Record<string, string | null> = {}
  props.columns.forEach((c, i) => {
    const f = fields.value[i]
    if (!f || f.locked) return
    const v = f.isNull ? null : f.value
    if (!props.row) {
      if (!f.omit) out[c.name] = v
      return
    }
    if (v !== (props.row[i] ?? null)) out[c.name] = v
  })
  emit('submit', out)
}
</script>

<template>
  <UModal v-model:open="open" :title="row ? t('dbstudio.editRow') : t('dbstudio.insertRow')" :ui="{ content: 'sm:max-w-2xl' }">
    <template #body>
      <form class="space-y-3" @submit.prevent="submit">
        <div v-for="(c, i) in columns" :key="c.name" class="space-y-1">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <label :for="`dbs-field-${i}`" class="font-mono text-xs font-medium text-highlighted">
              {{ c.name }} <span class="font-normal text-muted">{{ c.type }}</span>
            </label>
            <div v-if="fields[i] && !fields[i].locked" class="flex items-center gap-3">
              <USwitch v-if="!row" v-model="fields[i].omit" size="xs" :label="t('dbstudio.useDefault')" />
              <USwitch v-if="c.nullable" v-model="fields[i].isNull" size="xs" label="NULL" :disabled="fields[i].omit" />
            </div>
          </div>
          <template v-if="fields[i]">
            <p v-if="fields[i].locked" class="text-xs text-muted">
              {{ fields[i].reason }}
            </p>
            <UTextarea
              v-else-if="multiline(c)"
              :id="`dbs-field-${i}`"
              v-model="fields[i].value"
              :rows="3"
              autoresize
              :maxrows="12"
              class="w-full font-mono text-xs"
              :disabled="fields[i].isNull || fields[i].omit"
              :autofocus="focus === i"
            />
            <UInput
              v-else
              :id="`dbs-field-${i}`"
              v-model="fields[i].value"
              class="w-full font-mono text-xs"
              :disabled="fields[i].isNull || fields[i].omit"
              :autofocus="focus === i"
            />
          </template>
        </div>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="open = false" />
        <UButton icon="i-cg-eye" :label="t('dbstudio.previewSql')" :loading="busy" @click="submit" />
      </div>
    </template>
  </UModal>
</template>
