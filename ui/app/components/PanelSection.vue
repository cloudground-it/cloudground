<script setup lang="ts">
/**
 * One section of a page: a panel (UPageCard, outline) framed by a 1px rule,
 * with a ruled header strip — the title, one line about it, the section's
 * own actions on the right — and its content below. `flush` lets a table
 * or a list run edge to edge under the strip. The one layout every page
 * uses for a group of related things (DESIGN.md, .panel).
 */
const props = defineProps<{ title?: string, description?: string, flush?: boolean, icon?: string, label?: string }>()
// `toolbar` replaces the header row (a list's filters); `label` names a
// section that shows no title.
const slots = defineSlots<{ default?: () => unknown, actions?: () => unknown, footer?: () => unknown, title?: () => unknown, toolbar?: () => unknown }>()
const id = useId()
const hasHeader = computed(() => !!(props.title || slots.title || props.description || slots.actions))
</script>

<template>
  <UPageCard
    as="section"
    variant="outline"
    class="scroll-mt-6"
    :aria-labelledby="!slots.toolbar && (title || slots.title) ? id : undefined"
    :aria-label="label"
    :ui="{ container: 'p-0 sm:p-0 gap-y-0', header: 'px-4 sm:px-5 py-3 border-b border-default' }"
  >
    <template v-if="slots.toolbar" #header>
      <slot name="toolbar" />
    </template>
    <template v-else-if="hasHeader" #header>
      <div class="flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
        <div class="flex min-w-0 flex-[1_1_10rem] items-start gap-2.5">
          <UIcon v-if="icon" :name="icon" class="mt-0.5 size-[18px] shrink-0 text-cg-blue" aria-hidden="true" />
          <div class="min-w-0">
            <h2 v-if="title || slots.title" :id="id" class="m-0 text-base leading-snug font-semibold text-highlighted">
              <slot name="title">{{ title }}</slot>
            </h2>
            <p v-if="description" class="mt-0.5 mb-0 max-w-[72ch] text-[13px] text-pretty text-muted">
              {{ description }}
            </p>
          </div>
        </div>
        <div v-if="slots.actions" class="flex flex-wrap items-center gap-2">
          <slot name="actions" />
        </div>
      </div>
    </template>
    <div v-if="flush" class="min-w-0">
      <slot />
    </div>
    <div v-else class="flex min-w-0 flex-col gap-4 p-4 sm:p-5">
      <slot />
    </div>
    <!-- After the content (UPageCard's own footer comes before it). -->
    <div v-if="slots.footer" class="flex flex-wrap items-center gap-2 border-t border-default px-4 py-3 sm:px-5">
      <slot name="footer" />
    </div>
  </UPageCard>
</template>
