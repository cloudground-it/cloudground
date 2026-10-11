<script setup lang="ts">
const props = defineProps<{
  title: string
  description?: string
  confirmLabel?: string
  danger?: boolean
  typed?: string
}>()
const emit = defineEmits<{ close: [boolean] }>()
const { t } = useI18n()
const typedValue = ref('')
const ready = computed(() => !props.typed || typedValue.value.trim() === props.typed)
</script>

<template>
  <UModal :title="title" :description="description" :close="false" :dismissible="false">
    <template v-if="typed" #body>
      <UFormField :label="t('confirm.typeToContinue', { text: typed })">
        <UInput v-model="typedValue" class="w-full font-mono" autofocus :placeholder="typed" @keydown.enter="ready && emit('close', true)" />
      </UFormField>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="emit('close', false)" />
        <UButton :color="danger ? 'error' : 'primary'" :label="confirmLabel || t('common.confirm')" :disabled="!ready" @click="emit('close', true)" />
      </div>
    </template>
  </UModal>
</template>
