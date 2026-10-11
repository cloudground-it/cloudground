<script setup lang="ts">
/**
 * A page's title block: a HUD eyebrow, then the wide display title with one
 * serif-italic accent word (`accent`), actions to the right.
 */
withDefaults(defineProps<{ eyebrow?: string, title: string, accent?: string, size?: 'lg' | 'md' }>(), { size: 'lg' })
</script>

<template>
  <div class="flex flex-wrap items-end justify-between gap-x-10 gap-y-4">
    <div class="flex min-w-0 flex-col gap-2.5">
      <span v-if="eyebrow" class="hud text-muted">{{ eyebrow }}</span>
      <div class="flex flex-wrap items-center gap-x-[18px] gap-y-3">
        <h1
          class="disp m-0 text-highlighted [overflow-wrap:anywhere]"
          :class="size === 'lg' ? 'text-[clamp(36px,4vw,56px)] leading-[0.95]' : 'text-[clamp(30px,3.4vw,48px)] leading-none'"
        >
          {{ title }}<template v-if="accent">
            {{ ' ' }}<span class="ser text-cg-blue">{{ accent }}</span>
          </template>
        </h1>
        <slot name="badge" />
      </div>
    </div>
    <div v-if="$slots.default" class="flex flex-wrap items-center gap-2.5">
      <slot />
    </div>
  </div>
</template>
