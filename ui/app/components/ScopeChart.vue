<script setup lang="ts">
/**
 * An "oscilloscope" panel: an ink ground, a hairline grid and the series as
 * bright traces, in both themes. Values are 0–100 (percent), oldest first.
 * The legend sits outside, in text, so no line is told apart by colour alone
 * and the summary in `label` reaches screen readers.
 */
import type { ScopeSeries } from '~/types/scope'

const props = withDefaults(defineProps<{ series: ScopeSeries[], ticks: string[], label: string, title?: string, height?: number }>(), { height: 260 })

const W = 600
const H = computed(() => props.height)
function path(values: number[]) {
  if (values.length < 2) return ''
  const pad = 22 // keeps the bottom trace clear of the tick labels
  return values.map((v, i) => {
    const x = (i / (values.length - 1)) * W
    const y = (H.value - pad) - (Math.max(0, Math.min(100, v)) / 100) * (H.value - pad - 8)
    return `${i ? 'L' : 'M'}${x.toFixed(1)} ${y.toFixed(1)}`
  }).join(' ')
}
const grid = computed(() => {
  const h = H.value
  const rows = [1, 2, 3, 4].map(i => `M0 ${(h / 5) * i}H${W}`)
  const cols = [1, 2, 3, 4, 5].map(i => `M${(W / 6) * i} 0V${h}`)
  return [...rows, ...cols].join('')
})
</script>

<template>
  <div class="flex min-w-0 flex-col gap-3">
    <div class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
      <h2 v-if="title" class="m-0 text-base leading-snug font-semibold text-highlighted">
        {{ title }}
      </h2>
      <div class="flex flex-wrap gap-x-4 gap-y-1" aria-hidden="true">
        <span v-for="s in series" :key="s.key" class="flex items-center gap-2 text-[13px] text-muted">
          <span class="inline-block h-0.5 w-4" :style="{ background: s.color, opacity: s.dashed ? 0.75 : 1 }" />{{ s.label }}
        </span>
      </div>
    </div>
    <div role="img" :aria-label="label" class="relative overflow-hidden border border-default bg-default" :style="{ height: `${height}px` }">
      <svg :viewBox="`0 0 ${W} ${height}`" preserveAspectRatio="none" width="100%" height="100%" class="absolute inset-0" aria-hidden="true">
        <path :d="grid" stroke="var(--cg-rule)" stroke-width="1" fill="none" vector-effect="non-scaling-stroke" />
        <path
          v-for="s in series"
          :key="s.key"
          :d="path(s.values)"
          fill="none"
          :stroke="s.color"
          stroke-width="2"
          :stroke-dasharray="s.dashed ? '4 4' : undefined"
          stroke-linejoin="round"
          vector-effect="non-scaling-stroke"
        />
      </svg>
      <div class="mono absolute inset-x-3 bottom-2 flex justify-between text-xs text-muted" aria-hidden="true">
        <span v-for="tk in ticks" :key="tk">{{ tk }}</span>
      </div>
      <slot />
    </div>
  </div>
</template>
