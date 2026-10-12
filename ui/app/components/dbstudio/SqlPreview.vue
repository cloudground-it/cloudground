<script setup lang="ts">
/* Shows the SQL a change will run, before it runs; resolves true to run it. */
const props = defineProps<{
  title: string
  description?: string
  sql: string
  confirmLabel?: string
  danger?: boolean
}>()
const emit = defineEmits<{ close: [boolean] }>()
const { t } = useI18n()
</script>

<template>
  <UModal :title="props.title" :description="props.description" :close="false" :dismissible="false" :ui="{ content: 'sm:max-w-2xl' }">
    <template #body>
      <pre class="max-h-96 overflow-auto rounded-md bg-elevated p-3 font-mono text-xs whitespace-pre-wrap break-all">{{ props.sql }}</pre>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="emit('close', false)" />
        <UButton variant="solid" :color="props.danger ? 'error' : 'primary'" icon="i-cg-play" :label="props.confirmLabel || t('dbstudio.execute')" @click="emit('close', true)" />
      </div>
    </template>
  </UModal>
</template>
