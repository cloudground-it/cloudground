<script setup lang="ts">
/**
 * One stat tile (a card): a label, the value, an optional state word
 * (never colour alone), a bar and a one-line note. With `to` the whole
 * tile is a link (UPageCard's own).
 */
const props = defineProps<{
  label: string
  value: string | number
  unit?: string
  note?: string
  state?: { label: string, tone: 'ok' | 'warn' | 'err' | 'muted' }
  /** 0–100: how much of the bar is filled. */
  bar?: number
  barTone?: 'blue' | 'warn' | 'err'
  to?: string
}>()
const badge = { ok: 'success', warn: 'warning', err: 'error', muted: 'neutral' } as const
const barColor = computed(() => ({ blue: 'bg-cg-blue', warn: 'bg-cg-orange', err: 'bg-cg-err' })[props.barTone ?? 'blue'])
</script>

<template>
  <UPageCard variant="outline" :to="to" :ui="{ container: 'p-3 sm:p-4 gap-y-2.5' }">
    <div class="flex items-start justify-between gap-3">
      <span class="hud min-w-0 flex-1 hyphens-auto [overflow-wrap:anywhere] text-muted">{{ label }}</span>
      <UBadge v-if="state" :color="badge[state.tone]" variant="subtle" size="sm" :label="state.label" class="shrink-0" />
    </div>
    <span class="disp min-w-0 text-[22px] sm:text-[28px] leading-none text-highlighted hyphens-auto [overflow-wrap:anywhere]">
      {{ value }}<span v-if="unit" class="mono ms-1.5 text-sm tracking-normal text-muted">{{ unit }}</span>
    </span>
    <span v-if="bar !== undefined" class="block h-1 bg-accented" aria-hidden="true">
      <span class="block h-1" :class="barColor" :style="{ width: `${Math.max(0, Math.min(100, bar))}%` }" />
    </span>
    <span v-if="note" class="text-[13px] text-muted">{{ note }}</span>
  </UPageCard>
</template>
