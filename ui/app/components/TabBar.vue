<script setup lang="ts">
/**
 * A page's tab bar (a site's tools, a Quarry table's tabs): links with an
 * icon and a sans label, in the one tab style (main.css, cg-tab): the
 * active one in ink over a 2px --primary line. It never draws
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
  <!-- The divider is an inset shadow, painted under the tabs (cg-tab-list):
       the active tab's line lies on it. As a border it sat below the
       scroller, which clips anything drawn past its own box. -->
  <nav :aria-label="label" class="cg-tab-list flex items-stretch">
    <div ref="scroller" class="scrollbar-none min-w-0 flex-1 overflow-x-auto overflow-y-hidden">
      <ul class="m-0 flex w-max list-none p-0">
        <li v-for="it in items" :key="it.label">
          <NuxtLink
            :to="it.to"
            :replace="it.replace"
            :aria-current="it.active ? 'page' : undefined"
            class="cg-tab"
            :class="{ 'cg-tab-on': it.active }"
          >
            <UIcon :name="it.icon" class="size-4 shrink-0" aria-hidden="true" />
            {{ it.label }}
          </NuxtLink>
        </li>
      </ul>
    </div>
    <UDropdownMenu v-if="more?.length" :items="more" :content="{ align: 'end' }">
      <button
        type="button"
        class="cg-tab shrink-0 cursor-pointer"
        :class="{ 'cg-tab-on': activeMore }"
        :aria-current="activeMore ? 'page' : undefined"
      >
        <UIcon :name="activeMore?.icon ?? 'i-cg-more-horizontal'" class="size-4 shrink-0" aria-hidden="true" />
        {{ activeMore?.label ?? t('tabs.more') }}
        <UIcon name="i-cg-chevron-down" class="size-4 shrink-0" aria-hidden="true" />
      </button>
    </UDropdownMenu>
  </nav>
</template>
