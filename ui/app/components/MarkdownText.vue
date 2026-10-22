<script setup lang="ts">
import { markdownOf } from '~/utils/markdown'

/**
 * A changelog entry's Markdown, drawn from the tree utils/markdown.ts reads
 * (no HTML is ever built from it). Headings start at the level below the
 * entry's own title.
 */
const props = defineProps<{ source: string }>()
const blocks = computed(() => markdownOf(props.source))
</script>

<template>
  <div class="flex flex-col gap-3 text-sm leading-relaxed text-default">
    <template v-for="(b, i) in blocks" :key="i">
      <h4 v-if="b.t === 'h'" class="m-0 mt-1 text-sm font-semibold text-highlighted">
        <MarkdownInline :runs="b.inline" />
      </h4>
      <ul v-else-if="b.t === 'ul'" class="m-0 flex list-disc flex-col gap-1.5 ps-5">
        <li v-for="(item, j) in b.items" :key="j">
          <MarkdownInline :runs="item" />
        </li>
      </ul>
      <p v-else class="m-0">
        <MarkdownInline :runs="b.inline" />
      </p>
    </template>
  </div>
</template>
