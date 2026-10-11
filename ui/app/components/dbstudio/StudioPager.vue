<script setup lang="ts">
/*
 * Browse's pager, as phpMyAdmin has it: which rows are shown out of how
 * many (an estimate marked ~), rows per page, and first, previous, a page
 * number to type, next and last. The last page is the furthest the agent
 * serves (an offset of at most 10 000 000 rows, ops/dbstudio SPEC §4).
 */
const props = defineProps<{ total: number, estimated?: boolean, shown: number, sizes?: boolean }>()
const page = defineModel<number>('page', { required: true })
const size = defineModel<number>('size', { required: true })
const { t, locale } = useI18n()

const MAX_OFFSET = 10_000_000
const SIZES = [25, 50, 100, 250, 500]
const last = computed(() => Math.max(1, Math.min(Math.ceil(props.total / size.value), Math.floor(MAX_OFFSET / size.value) + 1)))
const from = computed(() => (page.value - 1) * size.value + 1)
const showing = computed(() => {
  if (!props.shown) return props.total && page.value > 1 ? t('studio.pager.empty') : t('studio.pager.none')
  const total = (props.estimated ? '~' : '') + fmtNumber(props.total, undefined, locale.value)
  return t('studio.pager.showing', { from: fmtNumber(from.value, undefined, locale.value), to: fmtNumber(from.value + props.shown - 1, undefined, locale.value), total })
})
const typed = ref(String(page.value))
watch(page, (p) => { typed.value = String(p) })
function go(p: number) {
  const n = Math.min(Math.max(1, Math.trunc(p) || 1), last.value)
  typed.value = String(n)
  if (n !== page.value) page.value = n
}
function setSize(v: number) {
  // Keep the first row shown in view: the page that holds it at the new size.
  const first = (page.value - 1) * size.value
  size.value = v
  page.value = Math.floor(first / v) + 1
}
</script>

<template>
  <div data-pager class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 text-[13px]">
    <span data-pager-showing class="mono text-muted" aria-live="polite">{{ showing }}</span>
    <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
      <label v-if="sizes" class="flex items-center gap-2 text-muted">
        <span>{{ t('studio.pager.perPage') }}</span>
        <USelect data-pager-size :model-value="size" :items="SIZES" size="sm" class="w-20" @update:model-value="v => setSize(Number(v))" />
      </label>
      <nav class="flex items-center gap-1" :aria-label="t('studio.pager.nav')">
        <UButton size="sm" color="neutral" variant="ghost" icon="i-cg-arrow-left" :aria-label="t('studio.pager.first')" :title="t('studio.pager.first')" :disabled="page <= 1" @click="go(1)" />
        <UButton data-pager-prev size="sm" color="neutral" variant="ghost" icon="i-cg-chevron-left" :aria-label="t('studio.pager.prev')" :title="t('studio.pager.prev')" :disabled="page <= 1" @click="go(page - 1)" />
        <span class="flex items-center gap-1.5 px-1 text-muted">
          <UInput
            v-model="typed"
            type="number"
            inputmode="numeric"
            :min="1"
            :max="last"
            size="sm"
            class="w-20"
            :ui="{ base: 'mono text-center' }"
            :aria-label="t('studio.pager.page')"
            @keydown.enter="go(Number(typed))"
            @blur="go(Number(typed))"
          />
          <span class="mono whitespace-nowrap">{{ t('studio.pager.of', { n: fmtNumber(last, undefined, locale) }) }}</span>
        </span>
        <UButton data-pager-next size="sm" color="neutral" variant="ghost" icon="i-cg-chevron-right" :aria-label="t('studio.pager.next')" :title="t('studio.pager.next')" :disabled="page >= last" @click="go(page + 1)" />
        <UButton size="sm" color="neutral" variant="ghost" icon="i-cg-arrow-right" :aria-label="t('studio.pager.last')" :title="t('studio.pager.last')" :disabled="page >= last" @click="go(last)" />
      </nav>
    </div>
  </div>
</template>
