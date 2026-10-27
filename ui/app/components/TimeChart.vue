<script setup lang="ts">
/**
 * A time series drawn at the container's real width, so strokes stay 2px and
 * text is never stretched: `line` (one or two series, the first with a light
 * area under it) or `bar` (thin columns with a surface gap and rounded tops).
 * A recessive y grid with a few round ticks, dates along the x axis, and a
 * hover layer (crosshair and tooltip for lines, the hovered column for bars)
 * that also answers the keyboard. Identity is never colour alone: the legend
 * names each series and the tooltip repeats the values in text.
 */
import { useElementSize } from '@vueuse/core'

export interface TimeSeries { key: string, label: string, color: string }
export interface TimePoint { key: string, label: string, short: string, values: Record<string, number> }

const props = withDefaults(defineProps<{
  points: TimePoint[]
  series: TimeSeries[]
  kind?: 'line' | 'bar'
  height?: number
  label: string
  format?: (n: number) => string
}>(), { kind: 'line', height: 220, format: (n: number) => String(n) })

const box = ref<HTMLElement | null>(null)
const { width } = useElementSize(box)
const W = computed(() => Math.max(240, Math.round(width.value)))
const H = computed(() => props.height)
const pad = { top: 12, right: 12, bottom: 26, left: 44 }
const plotW = computed(() => W.value - pad.left - pad.right)
const plotH = computed(() => H.value - pad.top - pad.bottom)

// Round ticks: 0 and three steps of 1, 2 or 5 × 10^n that cover the maximum.
const ticks = computed(() => {
  const max = Math.max(1, ...props.points.flatMap(p => props.series.map(s => p.values[s.key] ?? 0)))
  const raw = max / 3
  const mag = 10 ** Math.floor(Math.log10(raw))
  const step = ([1, 2, 5, 10].map(m => m * mag).find(s => s >= raw)) ?? raw
  return [0, step, step * 2, step * 3]
})
const top = computed(() => ticks.value[ticks.value.length - 1] || 1)
const y = (v: number) => pad.top + plotH.value - (v / top.value) * plotH.value

const n = computed(() => props.points.length)
// Lines put a point at each day's centre line; bars split the width in slots.
const slot = computed(() => plotW.value / Math.max(1, n.value))
const x = (i: number) => props.kind === 'bar'
  ? pad.left + slot.value * (i + 0.5)
  : pad.left + (n.value > 1 ? (i / (n.value - 1)) * plotW.value : plotW.value / 2)

function linePath(key: string) {
  return props.points.map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(p.values[key] ?? 0).toFixed(1)}`).join(' ')
}
function areaPath(key: string) {
  if (!n.value) return ''
  const base = y(0).toFixed(1)
  return `${linePath(key)} L${x(n.value - 1).toFixed(1)} ${base} L${x(0).toFixed(1)} ${base} Z`
}

// Columns: at most 14px wide, a 2px surface gap at least, 4px rounded top.
const bars = computed(() => {
  const s = props.series[0]
  if (!s) return []
  const w = Math.max(2, Math.min(14, slot.value - 2))
  return props.points.map((p, i) => {
    const v = p.values[s.key] ?? 0
    const h = Math.max(v > 0 ? 2 : 0, y(0) - y(v))
    const r = Math.min(4, w / 2, h)
    const x0 = x(i) - w / 2
    const y0 = y(0) - h
    const d = h <= 0 ? '' : `M${x0} ${y(0)}V${y0 + r}Q${x0} ${y0} ${x0 + r} ${y0}H${x0 + w - r}Q${x0 + w} ${y0} ${x0 + w} ${y0 + r}V${y(0)}Z`
    return { key: p.key, d }
  })
})

// About six date labels whatever the range, always the first and the last.
const xLabels = computed(() => {
  const every = Math.max(1, Math.ceil(n.value / 6))
  return props.points
    .map((p, i) => ({ i, text: p.short }))
    .filter(l => l.i % every === 0 || l.i === n.value - 1)
    .filter((l, k, all) => k === all.length - 1 || all[k + 1]!.i - l.i >= Math.max(1, every * 0.6))
})

const hover = ref<number | null>(null)
function onMove(e: PointerEvent) {
  const el = e.currentTarget as SVGElement
  const px = e.clientX - el.getBoundingClientRect().left
  if (!n.value) return
  const i = props.kind === 'bar'
    ? Math.floor((px - pad.left) / slot.value)
    : Math.round(((px - pad.left) / Math.max(1, plotW.value)) * (n.value - 1))
  hover.value = Math.max(0, Math.min(n.value - 1, i))
}
function onKey(e: KeyboardEvent) {
  if (!n.value) return
  if (e.key === 'ArrowRight') hover.value = Math.min(n.value - 1, (hover.value ?? -1) + 1)
  else if (e.key === 'ArrowLeft') hover.value = Math.max(0, (hover.value ?? n.value) - 1)
  else if (e.key === 'Escape') hover.value = null
  else return
  e.preventDefault()
}
const tip = computed(() => {
  const i = hover.value
  if (i === null || !props.points[i]) return null
  const left = x(i)
  return { i, left, flip: left > W.value * 0.62, point: props.points[i]! }
})
</script>

<template>
  <figure class="m-0 flex flex-col gap-2">
    <figcaption v-if="series.length > 1" class="flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-muted">
      <span v-for="s in series" :key="s.key" class="flex items-center gap-2">
        <span class="inline-block h-0.5 w-4 rounded-full" :style="{ background: s.color }" aria-hidden="true" />{{ s.label }}
      </span>
    </figcaption>
    <div ref="box" class="relative w-full" :style="{ height: `${H}px` }">
      <svg
        v-if="width > 0"
        :width="W"
        :height="H"
        :viewBox="`0 0 ${W} ${H}`"
        role="img"
        :aria-label="label"
        tabindex="0"
        class="block touch-pan-y outline-none focus-visible:ring-2 focus-visible:ring-[var(--cg-primary)]"
        @pointermove="onMove"
        @pointerleave="hover = null"
        @blur="hover = null"
        @keydown="onKey"
      >
        <g class="text-muted" font-size="11" fill="currentColor">
          <template v-for="tk in ticks" :key="tk">
            <line :x1="pad.left" :x2="W - pad.right" :y1="y(tk)" :y2="y(tk)" stroke="var(--cg-rule)" :stroke-dasharray="tk === 0 ? undefined : '2 4'" />
            <text :x="pad.left - 8" :y="y(tk)" text-anchor="end" dominant-baseline="middle">{{ format(tk) }}</text>
          </template>
          <text v-for="l in xLabels" :key="l.i" :x="x(l.i)" :y="H - 6" :text-anchor="l.i === 0 && kind === 'line' ? 'start' : l.i === n - 1 && kind === 'line' ? 'end' : 'middle'">{{ l.text }}</text>
        </g>
        <template v-if="kind === 'line'">
          <path v-if="series[0]" :d="areaPath(series[0].key)" :fill="series[0].color" fill-opacity="0.08" />
          <path v-for="s in series" :key="s.key" :d="linePath(s.key)" fill="none" :stroke="s.color" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
          <g v-if="tip">
            <line :x1="tip.left" :x2="tip.left" :y1="pad.top" :y2="y(0)" stroke="var(--cg-rule-strong)" stroke-width="1" />
            <circle v-for="s in series" :key="s.key" :cx="tip.left" :cy="y(tip.point.values[s.key] ?? 0)" r="4.5" :fill="s.color" stroke="var(--cg-paper)" stroke-width="2" />
          </g>
        </template>
        <template v-else>
          <path v-for="(b, i) in bars" :key="b.key" :d="b.d" :fill="series[0]?.color" :fill-opacity="tip && tip.i !== i ? 0.45 : 1" />
        </template>
      </svg>
      <div
        v-if="tip"
        class="pointer-events-none absolute top-1 z-10 min-w-36 rounded-md border border-[var(--cg-rule)] bg-[var(--cg-paper)] px-3 py-2 text-[13px] shadow-sm"
        :style="tip.flip ? { right: `${W - tip.left + 10}px` } : { left: `${tip.left + 10}px` }"
      >
        <div class="mb-1 font-medium text-[var(--cg-ink)]">
          {{ tip.point.label }}
        </div>
        <div v-for="s in series" :key="s.key" class="flex items-center justify-between gap-4 text-[var(--cg-text)]">
          <span class="flex items-center gap-2"><span class="inline-block h-0.5 w-3 rounded-full" :style="{ background: s.color }" aria-hidden="true" />{{ s.label }}</span>
          <span class="tabular-nums text-[var(--cg-ink)]">{{ format(tip.point.values[s.key] ?? 0) }}</span>
        </div>
      </div>
    </div>
  </figure>
</template>
