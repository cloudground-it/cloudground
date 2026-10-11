<script setup lang="ts">
/**
 * One section of a page: a card (UPageCard, outline) with a header row —
 * the title, one line about it, the section's own actions on the right —
 * and its content below. `flush` lets a table or a list run edge to edge
 * under a ruled header. The one layout every page uses for a group of
 * related things (ui-review layout system).
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
    :ui="flush
      ? { container: 'p-0 sm:p-0 gap-y-0', header: 'px-4 py-4 sm:px-5 border-b border-default' }
      : undefined"
  >
    <template v-if="slots.toolbar" #header>
      <slot name="toolbar" />
    </template>
    <template v-else-if="hasHeader" #header>
      <div class="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div class="flex min-w-0 flex-[1_1_16rem] items-start gap-3">
          <UIcon v-if="icon" :name="icon" class="mt-0.5 size-5 shrink-0 text-cg-blue" aria-hidden="true" />
          <div class="min-w-0">
            <h2 v-if="title || slots.title" :id="id" class="disp m-0 text-lg leading-snug text-highlighted [font-stretch:108%]">
              <slot name="title">{{ title }}</slot>
            </h2>
            <p v-if="description" class="mt-1 mb-0 max-w-[72ch] text-sm text-pretty text-muted">
              {{ description }}
            </p>
          </div>
        </div>
        <div v-if="slots.actions" class="flex flex-wrap items-center gap-2">
          <slot name="actions" />
        </div>
      </div>
    </template>
    <slot />
    <!-- After the content (UPageCard's own footer comes before it). -->
    <div v-if="slots.footer" class="flex flex-wrap items-center gap-2" :class="flush ? 'border-t border-default px-4 py-3 sm:px-5' : ''">
      <slot name="footer" />
    </div>
  </UPageCard>
</template>
