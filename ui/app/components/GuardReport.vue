<script setup lang="ts">
import type { GuardReport } from '~/types'

const props = defineProps<{ report: GuardReport }>()
const { t } = useI18n()

const color = computed(() => ({ pass: 'success', warn: 'warning', block: 'error', inconclusive: 'neutral', unverified: 'warning' } as const)[props.report.verdict] ?? 'neutral')
const pct = (ratio: number) => `${ratio >= 1 ? '+' : ''}${fmtPercent((ratio - 1) * 100, 1)}`
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2 font-medium text-highlighted">
        <UIcon name="i-cg-gauge" class="size-5" />
        Performance Guard
      </div>
      <UBadge :color="color" :label="t(`guard.verdict.${report.verdict}`)" />
    </div>

    <UAlert v-if="report.verdict === 'pass'" color="success" variant="subtle" icon="i-cg-check-circle" :title="t('guard.passed')" />
    <UAlert
      v-for="(r, i) in report.reasons || []"
      :key="i"
      :color="r.startsWith('BLOCK') ? 'error' : 'info'"
      variant="subtle"
      :description="r"
    />

    <div v-if="report.paths?.length" class="overflow-x-auto">
      <table class="cg-table border-t border-(--cg-ink)">
        <thead>
          <tr>
            <th class="py-1.5 pr-3 font-medium">
              {{ t('guard.path') }}
            </th>
            <th class="py-1.5 pr-3 font-medium">
              {{ t('guard.median') }}
            </th>
            <th class="py-1.5 pr-3 font-medium">
              {{ t('guard.interval') }}
            </th>
            <th class="py-1.5 pr-3 font-medium">
              {{ t('guard.delta') }}
            </th>
            <th class="py-1.5 font-medium">
              {{ t('guard.pairs') }}
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in report.paths" :key="p.path">
            <td class="py-1.5 pr-3 font-mono text-xs">
              {{ p.path }}
            </td>
            <td class="py-1.5 pr-3 font-medium" :class="p.ci_low > 1 ? 'text-error' : p.ci_high < 1 ? 'text-success' : ''">
              {{ pct(p.median_ratio) }}
            </td>
            <td class="py-1.5 pr-3 text-muted">
              {{ pct(p.ci_low) }} … {{ pct(p.ci_high) }}
            </td>
            <td class="py-1.5 pr-3 text-muted">
              {{ fmtNumber(p.median_delta_ms, 1) }} ms
            </td>
            <td class="py-1.5 text-muted">
              {{ p.pairs }}<span v-if="p.server_timed" class="ml-1 text-xs">· {{ t('guard.serverTimed') }}</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
