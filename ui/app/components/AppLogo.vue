<script setup lang="ts">
/**
 * The CloudGround mark (docs/media/logo.svg, v2): the cloud in ink over two
 * bars, the upper one blue and the lower one the brand orange, with no tile.
 * Drawn from the tokens, so it follows the colour mode; inlined so the
 * interface never waits on a request for its logo. `wordmark` adds the
 * lowercase wordmark, "cloudground", in Plex Sans; `product` names a
 * product of the family after it, one word with a dot ("cloudground
 * .quarry"). Running text keeps the capitalised names.
 */
withDefaults(defineProps<{ wordmark?: boolean, product?: string }>(), { wordmark: false, product: '' })
</script>

<template>
  <span class="inline-flex min-w-0 items-center gap-2.5">
    <svg viewBox="0 0 48 48" class="size-7 shrink-0" role="img" aria-hidden="true">
      <path fill="var(--cg-ink)" d="M14 26A8 8 0 1 1 15.843 10.215A10 10 0 0 1 33.87 14.391A6 6 0 1 1 36 26Z" />
      <rect x="14" y="30" width="28" height="4.6" rx="1" fill="var(--cg-blue)" />
      <rect x="6" y="38" width="28" height="4.6" rx="1" fill="var(--cg-logo-orange)" />
    </svg>
    <span v-if="wordmark" class="truncate text-[1.0625rem] leading-none font-semibold tracking-[-0.01em] text-highlighted">cloudground<span v-if="product" class="font-normal text-muted">.{{ product }}</span></span>
  </span>
</template>
