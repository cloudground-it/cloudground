<script setup lang="ts">
/**
 * The recent tasks, newest first: when, what, its state. Meant for the
 * body of a flush PanelSection (rows run edge to edge).
 */
import type { Task } from '~/types'

const props = withDefaults(defineProps<{ siteId?: number, limit?: number }>(), { limit: 8 })
const { t, locale } = useI18n()
const { tasks } = useTasks()
const { sites } = useShell()

const visible = computed(() => tasks.value.filter(x => !props.siteId || x.site_id === props.siteId).slice(0, props.limit))
const domain = (id: number) => sites.value?.find(s => s.id === id)?.domain
const color: Record<Task['status'], 'neutral' | 'info' | 'success' | 'error'> = {
  pending: 'neutral',
  running: 'info',
  succeeded: 'success',
  failed: 'error'
}
</script>

<template>
  <UEmpty v-if="!visible.length" size="sm" icon="i-cg-history" :description="t('tasks.none')" />
  <ol v-else class="m-0 list-none divide-y divide-default p-0">
    <li v-for="task in visible" :key="task.id" class="flex gap-3 px-4 py-3 sm:px-5">
      <span class="mono min-w-14 shrink-0 pt-0.5 text-xs whitespace-nowrap text-muted" :title="fmtDateTime(task.created_at, locale)">{{ fmtWhen(task.created_at, locale) }}</span>
      <span class="flex min-w-0 flex-1 flex-col gap-0.5">
        <span class="text-sm text-highlighted">
          {{ taskKindLabel(t, task.kind) }}<span v-if="!siteId && domain(task.site_id)" class="mono text-xs text-muted"> · {{ domain(task.site_id) }}</span>
        </span>
        <span v-if="task.error || task.message" class="line-clamp-2 text-[13px] break-words" :class="task.error ? 'text-cg-err' : 'text-muted'" :title="task.error || task.message">
          {{ task.error || task.message }}
        </span>
        <UProgress v-if="task.status === 'running'" :model-value="task.progress" size="2xs" class="mt-1.5" />
      </span>
      <UBadge :color="color[task.status]" variant="subtle" size="sm" :label="t(`tasks.status.${task.status}`)" class="shrink-0 self-start" />
    </li>
  </ol>
</template>
