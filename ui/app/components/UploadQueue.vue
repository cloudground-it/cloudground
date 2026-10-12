<script setup lang="ts">
import type { UploadItem } from '~/composables/useUploadQueue'

defineProps<{
  items: UploadItem[]
  active: boolean
  counts: { total: number, done: number, failed: number, finished: number }
}>()
const emit = defineEmits<{ cancel: [id: number], cancelAll: [], clear: [] }>()
const { t } = useI18n()
const open = ref(true)

const icons: Record<UploadItem['status'], string> = {
  queued: 'i-cg-clock',
  uploading: 'i-cg-loader',
  done: 'i-cg-check-circle',
  failed: 'i-cg-alert-circle',
  canceled: 'i-cg-x-circle',
  skipped: 'i-cg-arrow-right'
}
const tone: Record<UploadItem['status'], string> = {
  queued: 'text-dimmed',
  uploading: 'text-primary animate-spin',
  done: 'text-success',
  failed: 'text-error',
  canceled: 'text-dimmed',
  skipped: 'text-dimmed'
}
const pct = (i: UploadItem) => (i.size ? Math.min(100, Math.round((i.loaded / i.size) * 100)) : i.status === 'done' ? 100 : 0)
</script>

<template>
  <section class="overflow-hidden border border-default bg-default" :aria-label="t('upload.queue')">
    <header class="flex items-center gap-2 px-4 py-2">
      <UIcon :name="active ? 'i-cg-upload' : counts.failed ? 'i-cg-alert-circle' : 'i-cg-check-circle'" class="size-4 shrink-0" :class="active ? 'text-primary' : counts.failed ? 'text-error' : 'text-success'" />
      <p class="min-w-0 flex-1 truncate text-sm font-medium text-highlighted" aria-live="polite">
        {{ active ? t('upload.progress', { done: counts.finished, total: counts.total }) : t('upload.finished', { done: counts.done, total: counts.total }) }}
        <span v-if="counts.failed" class="text-error">· {{ t('upload.failedCount', { n: counts.failed }) }}</span>
      </p>
      <UButton v-if="active" size="xs" color="neutral" variant="ghost" :label="t('upload.cancelAll')" @click="emit('cancelAll')" />
      <UButton v-else size="xs" color="neutral" variant="ghost" :label="t('upload.clear')" @click="emit('clear')" />
      <UButton size="xs" color="neutral" variant="ghost" square :icon="open ? 'i-cg-chevron-up' : 'i-cg-chevron-down'" :aria-label="open ? t('upload.collapse') : t('upload.expand')" :aria-expanded="open" @click="open = !open" />
    </header>
    <ul v-if="open" class="max-h-72 divide-y divide-default overflow-y-auto border-t border-default">
      <li v-for="i in items" :key="i.id" class="flex items-start gap-3 px-4 py-2 text-sm">
        <UIcon :name="icons[i.status]" class="mt-0.5 size-4 shrink-0" :class="tone[i.status]" />
        <div class="min-w-0 flex-1 space-y-1">
          <div class="flex items-center gap-2">
            <span class="min-w-0 flex-1 truncate" :title="i.dest">{{ i.name }}</span>
            <span class="shrink-0 text-xs tabular-nums text-muted">
              <template v-if="i.status === 'uploading'">{{ fmtPercent(pct(i)) }}</template>
              <template v-else-if="i.size">{{ fmtBytes(i.size) }}</template>
            </span>
          </div>
          <UProgress v-if="i.status === 'uploading'" :model-value="pct(i)" size="xs" />
          <p v-if="i.error || i.status === 'canceled'" class="text-xs" :class="i.status === 'failed' ? 'text-error' : 'text-muted'">
            {{ i.error || t('upload.canceled') }}
          </p>
        </div>
        <UButton
          v-if="i.status === 'queued' || i.status === 'uploading'"
          size="xs"
          color="neutral"
          variant="ghost"
          square
          icon="i-cg-x"
          :aria-label="t('upload.cancel', { name: i.name })"
          @click="emit('cancel', i.id)"
        />
      </li>
    </ul>
  </section>
</template>
