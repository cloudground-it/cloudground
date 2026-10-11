<script setup lang="ts">
/** A section title (display, 22px) with an optional HUD link or actions to the right. */
defineProps<{ title: string, linkLabel?: string, to?: string, as?: string }>()
</script>

<template>
  <div class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
    <component :is="as || 'h2'" class="disp m-0 text-[22px] leading-tight text-highlighted [font-stretch:108%]">
      {{ title }}
    </component>
    <slot>
      <NuxtLink v-if="to && linkLabel" :to="to" class="hud text-cg-blue hover:underline">{{ linkLabel }} →</NuxtLink>
    </slot>
  </div>
</template>
