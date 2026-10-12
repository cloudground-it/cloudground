<script setup lang="ts">
/**
 * One measured value, a cell of a ruled grid (cg-grid, never a card): a
 * label, the value, an optional state word (never colour alone), a bar and
 * a one-line note. With `to` the whole cell is a link.
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
const NuxtLink = resolveComponent('NuxtLink')
const badge = { ok: 'success', warn: 'warning', err: 'error', muted: 'neutral' } as const
const barColor = computed(() => ({ blue: 'bg-cg-blue', warn: 'bg-cg-warn', err: 'bg-cg-err' })[props.barTone ?? 'blue'])
</script>

<template>
  <component
    :is="to ? NuxtLink : 'div'"
    :to="to"
    class="flex min-w-0 flex-col gap-2 p-4"
    :class="to ? 'transition-colors hover:bg-elevated' : ''"
  >
    <div class="flex items-start justify-between gap-3">
      <span class="min-w-0 flex-1 text-[13px] leading-snug hyphens-auto [overflow-wrap:anywhere] text-muted">{{ label }}</span>
      <UBadge v-if="state" :color="badge[state.tone]" variant="subtle" size="sm" :label="state.label" class="shrink-0" />
    </div>
    <span class="min-w-0 text-[1.375rem] leading-tight font-semibold text-highlighted hyphens-auto [overflow-wrap:anywhere] sm:text-2xl">
      {{ value }}<span v-if="unit" class="ms-1 text-sm font-normal text-muted">{{ unit }}</span>
    </span>
    <span v-if="bar !== undefined" class="block h-1.5 bg-accented" aria-hidden="true">
      <span class="block h-1.5" :class="barColor" :style="{ width: `${Math.max(0, Math.min(100, bar))}%` }" />
    </span>
    <span v-if="note" class="text-[13px] text-muted">{{ note }}</span>
  </component>
</template>
