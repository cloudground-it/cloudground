<script setup lang="ts">
/**
 * The top bar of every panel page, on UDashboardNavbar (the height of the
 * sidebar's header): the breadcrumb, the search that opens the ⌘K palette,
 * the theme switch and the alerts. A page's own actions are in its header,
 * not here. `title` is the last crumb; the section before it follows the
 * route unless `section` is given.
 */
const props = defineProps<{ title: string, section?: string, sectionTo?: string }>()
const { t } = useI18n()
const route = useRoute()
const colorMode = useColorMode()

const crumb = computed(() => {
  if (props.section) return { label: props.section, to: props.sectionTo }
  const p = route.path
  if (p === '/' || p.startsWith('/sites')) return { label: t('nav.sites'), to: p === '/sites' ? undefined : '/sites' }
  if (p.startsWith('/security')) return { label: t('cg.shell.account') }
  return { label: t('nav.server') }
})

const dark = computed(() => colorMode.value === 'dark')
function toggleTheme() {
  colorMode.preference = dark.value ? 'light' : 'dark'
}
</script>

<template>
  <UDashboardNavbar :toggle="{ color: 'neutral', variant: 'ghost', class: 'lg:hidden' }">
    <template #left>
      <nav :aria-label="t('cg.shell.breadcrumb')" class="flex min-w-0 items-center gap-2 text-sm">
        <NuxtLink v-if="crumb.to" :to="crumb.to" class="hidden shrink-0 text-muted hover:text-highlighted sm:inline">{{ crumb.label }}</NuxtLink>
        <span v-else class="hidden shrink-0 text-muted sm:inline">{{ crumb.label }}</span>
        <UIcon name="i-cg-chevron-right" class="hidden size-3.5 shrink-0 text-muted sm:block" aria-hidden="true" />
        <span class="truncate font-medium text-highlighted" aria-current="page">{{ title }}</span>
      </nav>
    </template>

    <template #right>
      <UDashboardSearchButton
        :label="t('cg.shell.search')"
        color="neutral"
        variant="outline"
        class="hidden w-80 md:flex"
        :ui="{ base: 'min-h-9 justify-start text-sm font-normal text-muted', label: 'truncate', trailing: 'ms-auto' }"
      />
      <UDashboardSearchButton
        :label="t('cg.shell.search')"
        color="neutral"
        variant="ghost"
        collapsed
        class="md:hidden"
        :aria-label="t('cg.shell.search')"
      />
      <slot name="right" />
      <UButton
        color="neutral"
        variant="ghost"
        square
        :icon="dark ? 'i-cg-sun' : 'i-cg-moon'"
        :aria-label="dark ? t('cg.shell.themeLight') : t('cg.shell.themeDark')"
        @click="toggleTheme"
      />
      <AlertsButton />
    </template>
  </UDashboardNavbar>
</template>
