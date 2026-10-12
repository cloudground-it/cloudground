<script setup lang="ts">
/*
 * Quarry's frame, built as the panel's: the sidebar (its header the
 * "cloudground quarry" wordmark, at the top bar's height; its body the
 * tree of the databases the session may open, scrolling on its own) and
 * a panel whose top bar holds the breadcrumb (site › database › table),
 * the theme switch, the session's user menu and sign-out. The page
 * scrolls in the panel's body; a table's sticky header sticks to its top.
 */
import type { DropdownMenuItem } from '@nuxt/ui'
import StudioTree from '~/components/dbstudio/StudioTree.vue'

const { t } = useI18n()
const colorMode = useColorMode()
const dark = computed(() => colorMode.value === 'dark')
const { session, logout, dbLink } = useStudio()
const route = useRoute()
const open = ref(false)
const home = STUDIO_HOME
watch(() => route.fullPath, () => { open.value = false })
const modeLabel = computed(() => session.value?.mode === 'login' ? t('studio.modeLogin') : session.value?.read_only ? t('studio.modePanelRead') : t('studio.modePanel'))

const db = computed(() => typeof route.params.db === 'string' ? route.params.db : '')
const table = computed(() => typeof route.params.table === 'string' ? route.params.table : '')

const userItems = computed<DropdownMenuItem[][]>(() => [
  [{ label: session.value?.user ?? '', icon: session.value?.mode === 'login' ? 'i-cg-database' : 'i-cg-user', type: 'label' }, { label: modeLabel.value, type: 'label' }],
  ...(session.value?.mode === 'panel' ? [[{ label: t('studio.toPanel'), icon: 'i-cg-arrow-left', to: '/' }]] : []),
  [{ label: t('studio.signOut'), icon: 'i-cg-logout', onSelect: () => void logout() }]
])
const initial = computed(() => (session.value?.user ?? '?').slice(0, 1).toUpperCase())
</script>

<template>
  <UDashboardGroup unit="rem" storage-key="quarry">
    <UDashboardSidebar
      id="quarry"
      v-model:open="open"
      :default-size="18"
      :min-size="18"
      :max-size="18"
      toggle-side="right"
      :ui="{ root: 'border-default', content: 'w-[19rem] max-w-[85vw] bg-elevated', toggle: 'me-3' }"
    >
      <template #header>
        <NuxtLink :to="home" class="flex h-full min-w-0 flex-1 items-center text-highlighted" :aria-label="t('studio.home')">
          <AppLogo wordmark product="quarry" />
        </NuxtLink>
      </template>

      <template #default>
        <StudioTree />
      </template>
    </UDashboardSidebar>

    <!-- No padding above the content: a table's sticky header sticks to the
         body's top edge (--studio-head 0), not below the padding. -->
    <UDashboardPanel id="quarry-main" class="[--studio-head:0px]" :ui="{ body: 'pt-0 sm:pt-0' }">
      <template #header>
        <UDashboardNavbar :toggle="{ color: 'neutral', variant: 'ghost', class: 'lg:hidden' }">
          <template #left>
            <nav :aria-label="t('cg.shell.breadcrumb')" class="flex min-w-0 items-center gap-2 text-[15px]">
              <span v-if="session" class="hidden shrink-0 text-muted sm:inline" :title="session.site.domain">{{ session.site.domain }}</span>
              <template v-if="db">
                <UIcon name="i-cg-chevron-right" class="hidden size-3.5 shrink-0 text-dimmed sm:block" aria-hidden="true" />
                <NuxtLink v-if="table" :to="dbLink(db)" class="mono hidden min-w-0 truncate text-[13px] text-muted hover:text-highlighted sm:inline">{{ db }}</NuxtLink>
                <span v-else class="mono min-w-0 truncate text-[13px] font-semibold text-highlighted" aria-current="page">{{ db }}</span>
              </template>
              <template v-if="table">
                <UIcon name="i-cg-chevron-right" class="hidden size-3.5 shrink-0 text-dimmed sm:block" aria-hidden="true" />
                <span class="mono min-w-0 truncate text-[13px] font-semibold text-highlighted" aria-current="page">{{ table }}</span>
              </template>
            </nav>
          </template>

          <template #right>
            <UBadge v-if="session" color="neutral" variant="outline" class="hidden md:inline-flex" :label="modeLabel" :title="t('studio.modeHint')" />
            <UButton
              color="neutral"
              variant="ghost"
              square
              :icon="dark ? 'i-cg-sun' : 'i-cg-moon'"
              :aria-label="dark ? t('cg.shell.themeLight') : t('cg.shell.themeDark')"
              @click="colorMode.preference = dark ? 'light' : 'dark'"
            />
            <UDropdownMenu v-if="session" :items="userItems" :content="{ align: 'end' }">
              <UButton color="neutral" variant="ghost" class="gap-2 px-1.5 text-sm font-normal" :aria-label="t('cg.shell.accountMenu', { name: session.user })">
                <span class="grid size-7 shrink-0 place-items-center rounded-[3px] bg-inverted text-xs font-semibold text-inverted" aria-hidden="true">{{ initial }}</span>
                <span class="mono hidden max-w-56 truncate text-[13px] text-default xl:inline">{{ session.user }}</span>
              </UButton>
            </UDropdownMenu>
            <UButton v-if="session" color="neutral" variant="ghost" square icon="i-cg-logout" :aria-label="t('studio.signOut')" :title="t('studio.signOut')" @click="logout" />
          </template>
        </UDashboardNavbar>
      </template>

      <template #body>
        <UContainer class="flex flex-col gap-6 pt-6 sm:pt-8">
          <slot />
        </UContainer>
      </template>
    </UDashboardPanel>
  </UDashboardGroup>
</template>
