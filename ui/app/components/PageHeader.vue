<script setup lang="ts">
/**
 * The title block of a page outside the panel's frame (sign-in, setup,
 * Quarry's hand-off): the title, set as one sentence with its `accent`
 * words, and actions to the right. No eyebrow above it (DESIGN.md).
 */
withDefaults(defineProps<{ title: string, accent?: string, size?: 'lg' | 'md' }>(), { size: 'lg' })
</script>

<template>
  <div class="flex flex-wrap items-end justify-between gap-x-10 gap-y-4">
    <div class="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-3">
      <h1
        class="m-0 font-semibold tracking-[-0.015em] text-highlighted [overflow-wrap:anywhere]"
        :class="size === 'lg' ? 'text-[2rem] leading-[1.15]' : 'text-[1.625rem] leading-tight'"
      >
        {{ title }}<template v-if="accent">
          {{ ' ' }}{{ accent }}
        </template>
      </h1>
      <slot name="badge" />
    </div>
    <div v-if="$slots.default" class="flex flex-wrap items-center gap-2.5">
      <slot />
    </div>
  </div>
</template>
