<script setup lang="ts">
import type { Inline } from '~/utils/markdown'

/** The runs of one line of Markdown (utils/markdown.ts): text, never HTML. */
defineProps<{ runs: Inline[] }>()
</script>

<template>
  <template v-for="(r, i) in runs" :key="i">
    <code v-if="r.t === 'code'" class="mono rounded-[3px] bg-elevated px-1 py-px text-[13px] text-highlighted">{{ r.v }}</code>
    <strong v-else-if="r.t === 'strong'" class="font-semibold text-highlighted">{{ r.v }}</strong>
    <em v-else-if="r.t === 'em'">{{ r.v }}</em>
    <a v-else-if="r.t === 'link'" :href="r.href" target="_blank" rel="noopener noreferrer" class="text-primary underline underline-offset-2">{{ r.v }}</a>
    <template v-else>{{ r.v }}</template>
  </template>
</template>
