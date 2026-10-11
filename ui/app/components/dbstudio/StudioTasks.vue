<script setup lang="ts">
/* The export and import tasks started from this page, with where they are. */
import type { StudioTask } from '~/composables/useStudioFiles'

defineProps<{ tasks: StudioTask[] }>()
const { t } = useI18n()
const tone = (s: string) => s === 'succeeded' ? 'text-cg-ok' : s === 'failed' ? 'text-error' : 'text-muted'
</script>

<template>
  <PanelSection v-if="tasks.length" :title="t('studio.tasks')" flush>
    <ul class="m-0 list-none divide-y divide-default p-0">
      <li v-for="x in tasks" :key="x.id" class="flex flex-wrap items-center gap-3 px-4 py-2.5 sm:px-5">
        <UIcon :name="x.status === 'succeeded' ? 'i-cg-check-circle' : x.status === 'failed' ? 'i-cg-alert-circle' : 'i-cg-loader'" class="size-4" :class="[tone(x.status), x.status === 'running' || x.status === 'pending' ? 'animate-spin' : '']" />
        <span class="min-w-0 flex-1 text-[13px] text-highlighted">{{ x.name || x.kind }}</span>
        <span class="min-w-0 truncate text-[13px] text-muted" :title="x.error || x.message">{{ x.error || x.message }}</span>
        <UProgress v-if="x.status === 'running'" :model-value="x.progress ?? 0" size="xs" class="w-24" />
      </li>
    </ul>
  </PanelSection>
</template>
