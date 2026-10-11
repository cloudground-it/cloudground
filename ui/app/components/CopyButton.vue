<script setup lang="ts">
const props = defineProps<{ value: string }>()
const { t } = useI18n()
const { copy, copied } = useClipboard({ legacy: true })
</script>

<template>
  <UTooltip :text="copied ? t('common.copied') : t('common.copy')">
    <UButton size="xs" color="neutral" variant="ghost" square :icon="copied ? 'i-cg-check' : 'i-cg-copy'" :aria-label="t('common.copy')" @click="copy(props.value)" />
  </UTooltip>
</template>
