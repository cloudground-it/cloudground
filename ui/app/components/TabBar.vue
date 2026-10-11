<script setup lang="ts">
/**
 * A page's tab bar (a site's tools, a Quarry table's tabs): links with an
 * icon and a sans label, the active one underlined in blue. It never draws
 * a scrollbar: when it does not fit it scrolls sideways (touch, wheel,
 * keyboard focus) and keeps the active tab in view. `more` puts the less
 * used tabs under one menu at the end.
 */
import type { TabBarItem } from '~/types/tabbar'

const props = defineProps<{ items: TabBarItem[], more?: TabBarItem[], label: string }>()
const { t } = useI18n()
const route = useRoute()

const activeMore = computed(() => props.more?.find(m => m.active))

const scroller = ref<HTMLElement | null>(null)
function reveal() {
  const box = scroller.value
  const el = box?.querySelector<HTMLElement>('[aria-current="page"]')
  if (!box || !el) return
  const left = el.getBoundingClientRect().left - box.getBoundingClientRect().left + box.scrollLeft
  if (left < box.scrollLeft || left + el.offsetWidth > box.scrollLeft + box.clientWidth) box.scrollLeft = Math.max(0, left - 16)
}
watch(() => route.fullPath, () => nextTick(reveal))
onMounted(() => nextTick(reveal))
</script>

<template>
  <nav :aria-label="label" class="flex items-stretch border-b border-default">
    <div ref="scroller" class="scrollbar-none min-w-0 flex-1 overflow-x-auto overflow-y-hidden">
      <UNavigationMenu
        :items="items"
        variant="link"
        color="neutral"
        highlight
        highlight-color="secondary"
        :ui="{ root: 'w-max', item: 'py-1.5', link: 'px-3 py-2.5 after:-bottom-1.5' }"
      />
    </div>
    <UDropdownMenu v-if="more?.length" :items="more" :content="{ align: 'end' }">
      <UButton
        color="neutral"
        variant="ghost"
        :icon="activeMore?.icon ?? 'i-cg-more-horizontal'"
        trailing-icon="i-cg-chevron-down"
        :label="activeMore?.label ?? t('tabs.more')"
        :aria-current="activeMore ? 'page' : undefined"
        class="relative my-1.5 shrink-0 font-sans text-sm font-medium tracking-normal normal-case after:absolute after:inset-x-2 after:-bottom-1.5 after:h-0.5"
        :class="activeMore ? 'text-highlighted after:bg-cg-blue' : 'text-muted'"
      />
    </UDropdownMenu>
  </nav>
</template>
