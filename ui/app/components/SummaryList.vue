<script setup lang="ts">
/**
 * Key–value facts (a site's folder, a database's user, a repository): a
 * description list, keys on the left, values on the right, a hairline
 * between rows. Nuxt UI has no description list. `mono` marks values that
 * are technical (paths, users, hosts); `copy` adds a copy button.
 */
import type { SummaryRow } from '~/types/summary'

defineProps<{ rows: SummaryRow[] }>()
const tone = { ok: 'text-cg-ok', warn: 'text-cg-warn', err: 'text-cg-err', muted: 'text-muted' }
</script>

<template>
  <dl class="m-0 grid grid-cols-1 sm:grid-cols-[minmax(8rem,max-content)_1fr]">
    <template v-for="r in rows" :key="r.k">
      <dt class="border-default pt-2.5 text-[13px] text-muted sm:border-b sm:py-2.5 sm:pe-6 [&:nth-last-child(2)]:border-b-0">
        {{ r.k }}
      </dt>
      <dd
        class="m-0 flex min-w-0 items-center gap-1.5 border-b border-default pb-2.5 text-sm [overflow-wrap:anywhere] sm:py-2.5 last:border-b-0"
        :class="[r.mono ? 'mono text-[13px]' : '', r.tone ? tone[r.tone] : 'text-highlighted']"
      >
        <span class="min-w-0">{{ r.v }}</span>
        <CopyButton v-if="r.copy && r.v" :value="r.v" />
      </dd>
    </template>
  </dl>
</template>
