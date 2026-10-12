<script setup lang="ts">
/**
 * The CloudGround mark, "Messa a terra" (docs/media/logo.svg, v3): the
 * electrical ground symbol with a cloud at the top of its wire, one 6/64
 * stroke in --mark (the logo's blue, lighter on dark). Inlined, so the
 * interface never waits on a request for its logo. `wordmark` adds the
 * name, "CloudGround", in Plex Sans 600: a 32px mark 8px from the name,
 * as the site's navbar sets them. `product` names a product of the family
 * after it, one word with a dot ("CloudGround .quarry").
 */
withDefaults(defineProps<{ wordmark?: boolean, product?: string }>(), { wordmark: false, product: '' })
</script>

<template>
  <span class="inline-flex min-w-0 items-center gap-2">
    <svg viewBox="0 0 64 64" class="size-8 shrink-0 text-cg-mark" role="img" aria-hidden="true">
      <path d="M20.5 28.5 H43.5 A7 7 0 1 0 41.62 14.76 A10 10 0 0 0 22.16 15.72 A6.5 6.5 0 1 0 20.5 28.5 Z M32 28.5 V38 M12 38 H52 M20 47.5 H44 M28 57 H36" fill="none" stroke="currentColor" stroke-width="6" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
    <span v-if="wordmark" class="truncate text-lg leading-none font-semibold tracking-[-0.015em] text-highlighted">CloudGround<span v-if="product" class="font-normal text-muted">.{{ product }}</span></span>
  </span>
</template>
