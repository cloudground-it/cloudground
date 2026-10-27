<script setup lang="ts">
/**
 * The site's visitor statistics (#33), built on the server from its nginx
 * access log: no script in the pages, no cookie, no third party, and no
 * visitor's address kept (GET /api/sites/{id}/traffic).
 */
interface Counts {
  requests: number, pageviews: number, visitors: number, bots: number, bytes: number
  status_2xx: number, status_3xx: number, status_4xx: number, status_5xx: number
  cache_hit: number, cache_miss: number, cache_bypass: number
}
interface PathCount { path: string, count: number }
interface TrafficView {
  days: (Counts & { date: string })[]
  hours: { time: string, requests: number, pageviews: number }[]
  totals: Counts
  top_pages: PathCount[]
  top_not_found: PathCount[]
  collected_at?: string
}

const { t, locale } = useI18n()
const api = useApi()
const { site } = useCurrentSite()

const range = ref<7 | 30 | 90>(7)
const { data, refresh } = useLoad(() => api.get<TrafficView>(`/api/sites/${site.value.id}/traffic?days=${range.value}`))
watch(range, () => void refresh())

const pct = (n: number, of: number) => (of > 0 ? Math.round((n * 100) / of) : 0)
const cells = computed(() => {
  const v = data.value
  if (!v) return []
  const c = v.totals
  const cached = c.cache_hit + c.cache_miss + c.cache_bypass
  return [
    { key: 'views', label: t('traffic.pageviews'), value: fmtNumber(c.pageviews, 0, locale.value), note: t('traffic.pageviewsNote') },
    { key: 'visitors', label: t('traffic.visitors'), value: fmtNumber(c.visitors, 0, locale.value), note: t('traffic.visitorsNote') },
    { key: 'requests', label: t('traffic.requests'), value: fmtNumber(c.requests, 0, locale.value), note: t('traffic.botsNote', { pct: pct(c.bots, c.requests) }) },
    { key: 'bytes', label: t('traffic.bandwidth'), value: fmtBytes(c.bytes, locale.value), note: t('traffic.bandwidthNote') },
    { key: 'cache', label: t('traffic.cacheHits'), value: cached ? `${pct(c.cache_hit, cached)}` : '—', unit: cached ? '%' : '', note: t('traffic.cacheNote') },
    { key: 'errors', label: t('traffic.errors'), value: fmtNumber(c.status_5xx, 0, locale.value), note: t('traffic.errorsNote', { n: fmtNumber(c.status_4xx, 0, locale.value) }) }
  ]
})

// Bars: page views (blue) with the visitors (ink) inside each day's bar.
const W = 600
const H = 180
const dayBars = computed(() => {
  const d = data.value?.days ?? []
  const max = Math.max(1, ...d.map(x => x.pageviews))
  const step = W / Math.max(1, d.length)
  return d.map((x, i) => ({
    key: x.date,
    x: i * step + step * 0.15,
    w: step * 0.7,
    h: (x.pageviews / max) * (H - 10),
    hv: (x.visitors / max) * (H - 10),
    title: t('traffic.dayTitle', { date: new Date(`${x.date}T00:00:00Z`).toLocaleDateString(locale.value, { timeZone: 'UTC' }), views: x.pageviews, visitors: x.visitors })
  }))
})
const hourBars = computed(() => {
  const h = data.value?.hours ?? []
  const max = Math.max(1, ...h.map(x => x.pageviews))
  const step = W / Math.max(1, h.length)
  return h.map((x, i) => ({
    key: x.time,
    x: i * step + step * 0.1,
    w: step * 0.8,
    h: (x.pageviews / max) * (H / 2 - 6),
    title: t('traffic.hourTitle', { time: new Date(x.time).toLocaleString(locale.value, { weekday: 'short', hour: '2-digit', minute: '2-digit' }), views: x.pageviews })
  }))
})
const chartLabel = computed(() => {
  const v = data.value
  return v ? t('traffic.chartAria', { days: range.value, views: v.totals.pageviews, visitors: v.totals.visitors }) : ''
})
const empty = computed(() => !!data.value && data.value.totals.requests === 0)
</script>

<template>
  <PanelSection :title="t('traffic.title')" :description="t('traffic.hint')">
    <template #actions>
      <UFieldGroup>
        <UButton v-for="n in ([7, 30, 90] as const)" :key="n" color="neutral" :variant="range === n ? 'solid' : 'outline'" size="sm" :label="t('traffic.days', { n })" @click="range = n" />
      </UFieldGroup>
    </template>
    <USkeleton v-if="!data" class="h-64" />
    <div v-else class="flex flex-col gap-6">
      <p v-if="empty" class="m-0 text-sm text-muted">
        {{ data.collected_at ? t('traffic.emptyRange') : t('traffic.notYet') }}
      </p>
      <div class="cg-grid sm:grid-cols-2 xl:grid-cols-3">
        <StatCell v-for="c in cells" :key="c.key" :label="c.label" :value="c.value" :unit="c.unit" :note="c.note" />
      </div>
      <figure class="m-0 flex flex-col gap-2">
        <figcaption class="flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-muted">
          <span class="flex items-center gap-2"><span class="inline-block size-2.5 bg-cg-blue" aria-hidden="true" />{{ t('traffic.pageviews') }}</span>
          <span class="flex items-center gap-2"><span class="inline-block size-2.5 bg-[var(--cg-ink)]" aria-hidden="true" />{{ t('traffic.visitors') }}</span>
        </figcaption>
        <svg :viewBox="`0 0 ${W} ${H}`" class="h-48 w-full" preserveAspectRatio="none" role="img" :aria-label="chartLabel">
          <line x1="0" :y1="H - 0.5" :x2="W" :y2="H - 0.5" stroke="currentColor" class="text-muted" stroke-opacity="0.4" />
          <g v-for="b in dayBars" :key="b.key">
            <title>{{ b.title }}</title>
            <rect :x="b.x" :y="H - b.h" :width="b.w" :height="b.h" fill="var(--cg-blue)" />
            <rect :x="b.x + b.w * 0.25" :y="H - b.hv" :width="b.w * 0.5" :height="b.hv" fill="var(--cg-ink)" />
          </g>
        </svg>
      </figure>
      <figure class="m-0 flex flex-col gap-2">
        <figcaption class="text-[13px] text-muted">
          {{ t('traffic.last48') }}
        </figcaption>
        <svg :viewBox="`0 0 ${W} ${H / 2}`" class="h-24 w-full" preserveAspectRatio="none" role="img" :aria-label="t('traffic.last48')">
          <line x1="0" :y1="H / 2 - 0.5" :x2="W" :y2="H / 2 - 0.5" stroke="currentColor" class="text-muted" stroke-opacity="0.4" />
          <g v-for="b in hourBars" :key="b.key">
            <title>{{ b.title }}</title>
            <rect :x="b.x" :y="H / 2 - b.h" :width="b.w" :height="b.h" fill="var(--cg-blue)" />
          </g>
        </svg>
      </figure>
    </div>
    <template v-if="data" #footer>
      <span class="text-[13px] text-muted">
        {{ t('traffic.privacy') }}
        <template v-if="data.collected_at"> {{ t('traffic.collectedAt', { at: fmtDateTime(data.collected_at, locale) }) }}</template>
      </span>
    </template>
  </PanelSection>

  <div v-if="data" class="grid items-start gap-6 lg:grid-cols-2">
    <PanelSection :title="t('traffic.topPages')">
      <p v-if="!data.top_pages.length" class="m-0 text-sm text-muted">{{ t('traffic.none') }}</p>
      <table v-else class="w-full text-sm">
        <thead>
          <tr class="text-left text-[13px] text-muted">
            <th class="pb-2 font-normal">{{ t('traffic.path') }}</th>
            <th class="pb-2 text-right font-normal">{{ t('traffic.pageviews') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in data.top_pages" :key="p.path" class="border-t border-default">
            <td class="max-w-0 truncate py-1.5 pe-3 font-mono text-xs" :title="p.path">{{ p.path }}</td>
            <td class="py-1.5 text-right tabular-nums">{{ fmtNumber(p.count, 0, locale) }}</td>
          </tr>
        </tbody>
      </table>
    </PanelSection>
    <PanelSection :title="t('traffic.notFound')" :description="t('traffic.notFoundHint')">
      <p v-if="!data.top_not_found.length" class="m-0 text-sm text-muted">{{ t('traffic.none') }}</p>
      <table v-else class="w-full text-sm">
        <thead>
          <tr class="text-left text-[13px] text-muted">
            <th class="pb-2 font-normal">{{ t('traffic.path') }}</th>
            <th class="pb-2 text-right font-normal">{{ t('traffic.hits') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in data.top_not_found" :key="p.path" class="border-t border-default">
            <td class="max-w-0 truncate py-1.5 pe-3 font-mono text-xs" :title="p.path">{{ p.path }}</td>
            <td class="py-1.5 text-right tabular-nums">{{ fmtNumber(p.count, 0, locale) }}</td>
          </tr>
        </tbody>
      </table>
    </PanelSection>
  </div>
</template>
