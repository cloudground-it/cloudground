<script setup lang="ts">
/**
 * With `siteId`, `source` is one of the site's own logs (access, error, php),
 * read through the site so operators can see it; without, it names a
 * server-wide log, which only global viewers may read.
 */
const props = defineProps<{ source: string, siteId?: number, site?: string, lines?: number }>()
const { t } = useI18n()
const api = useApi()
const content = ref('')
const loading = ref(false)
const pre = ref<HTMLElement | null>(null)
const label = computed(() => props.siteId ? `site:${props.site ?? props.siteId}:${props.source}` : props.source)

async function load() {
  loading.value = true
  try {
    const qs = new URLSearchParams({ source: props.source, lines: String(props.lines ?? 300) })
    const r = props.siteId
      ? await api.get<{ content: string }>(`/api/sites/${props.siteId}/logs?${qs}`)
      : await api.get<{ content: string }>(`/api/logs?${qs}`)
    content.value = r.content || t('logs.empty')
  } catch (e) {
    content.value = errorMessage(e)
  } finally {
    loading.value = false
    await nextTick()
    if (pre.value) pre.value.scrollTop = pre.value.scrollHeight
  }
}
watch(() => props.source, load, { immediate: true })
</script>

<template>
  <div class="overflow-hidden bg-cg-panel">
    <div class="flex items-center gap-2 border-b border-cg-panel-line ps-3.5">
      <span class="hud min-w-0 flex-1 truncate text-cg-panel-text">{{ label }}</span>
      <button
        type="button"
        class="hud flex min-h-10 items-center gap-1.5 border-s border-cg-panel-line px-3.5 text-cg-panel-text transition-colors hover:bg-[#F2F1EC] hover:text-[#0F0F12] disabled:opacity-60"
        :disabled="loading"
        @click="load"
      >
        <UIcon name="i-cg-refresh" class="size-3.5" :class="loading ? 'animate-spin' : ''" />{{ t('common.refresh') }}
      </button>
    </div>
    <pre ref="pre" class="mono max-h-[65vh] min-h-64 overflow-auto p-3.5 text-xs leading-relaxed whitespace-pre-wrap text-[#D9D8D0]">{{ content }}</pre>
  </div>
</template>
