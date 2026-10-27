<script setup lang="ts">
/**
 * A function's new token, shown the one time the API hands it over
 * (internal/functions SPEC §1.1): the token, a copy button and how a
 * request sends it.
 */
defineProps<{ token: string }>()
defineEmits<{ done: [] }>()
const { t } = useI18n()
</script>

<template>
  <UAlert color="warning" variant="subtle" icon="i-cg-key" :title="t('fn.token.title')" data-testid="fn-token">
    <template #description>
      <div class="flex flex-col gap-2">
        <p class="m-0">{{ t('fn.token.hint') }}</p>
        <div class="flex min-w-0 items-center gap-1 border border-default bg-default px-3 py-2">
          <code class="mono min-w-0 flex-1 text-[13px] [overflow-wrap:anywhere] text-highlighted">{{ token }}</code>
          <CopyButton :value="token" />
        </div>
        <code class="mono text-xs text-muted">Authorization: Bearer {{ token.slice(0, 8) }}…</code>
      </div>
    </template>
    <template #actions>
      <UButton size="sm" color="neutral" variant="outline" :label="t('fn.token.done')" @click="$emit('done')" />
    </template>
  </UAlert>
</template>
