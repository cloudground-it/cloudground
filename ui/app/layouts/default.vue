<script setup lang="ts">
import type { CommandPaletteGroup, CommandPaletteItem } from '@nuxt/ui'
import type { Site, UpdateStatus } from '~/types'

const { t } = useI18n()
const route = useRoute()
const api = useApi()
const { user, isAdmin } = useAuth()
const { sites, creating, sitesVersion, version } = useShell()
const open = ref(false)

const isOperator = computed(() => user.value?.role === 'operator')

interface NavLink { label: string, icon: string, to: string, exact?: boolean, count?: number }
const groups = computed<{ label: string, links: NavLink[] }[]>(() => {
  const out = [{
    label: t('nav.sites'),
    links: [
      { label: t('nav.dashboard'), icon: 'i-cg-dashboard', to: '/', exact: true },
      { label: t('nav.sites'), icon: 'i-cg-sites', to: '/sites', count: sites.value?.length },
      ...(isOperator.value ? [] : [{ label: t('nav.logs'), icon: 'i-cg-logs', to: '/logs' }])
    ] as NavLink[]
  }]
  if (!isOperator.value) {
    out.push({
      label: t('nav.server'),
      links: [
        { label: t('nav.services'), icon: 'i-cg-services', to: '/services' },
        { label: t('nav.firewall'), icon: 'i-cg-firewall', to: '/firewall' },
        { label: t('nav.users'), icon: 'i-cg-users', to: '/users' },
        { label: t('nav.audit'), icon: 'i-cg-history', to: '/audit' },
        { label: t('nav.settings'), icon: 'i-cg-settings', to: '/settings' }
      ]
    })
  }
  return out
})
const isActive = (l: NavLink) => l.exact ? route.path === l.to : route.path === l.to || route.path.startsWith(l.to + '/')

// The server, as the browser reached the panel; the version from the update check.
const host = computed(() => import.meta.client ? window.location.hostname : '')

// The signed-in frame owns the app's one event stream.
const events = useEvents()
onMounted(events.start)
onBeforeUnmount(events.stop)

// The site list behind the sidebar count and the search palette: reloaded
// when a site changes, not on a timer.
const { data: shellSites, refresh: loadSites } = useLive(() => api.get<Site[]>('/api/sites'), {
  on: { types: ['site.'] },
  fallbackMs: 30000
})
watch(shellSites, (v) => { if (v) sites.value = v })
onMounted(async () => {
  try {
    version.value = (await api.get<UpdateStatus>('/api/system/update')).current
  } catch {
    // Shown only when known.
  }
})
watch(sitesVersion, loadSites)

// A task that ends says so, wherever one is on the panel.
const toast = useToast()
const { label: taskLabel } = useTaskNames()
events.on((ev) => {
  if (ev.type !== 'task.finished') return
  const d = (ev.data ?? {}) as { kind?: string, status?: string, job_status?: string, error?: string }
  const kind = taskLabel(d.kind ?? '')
  const domain = ev.site_id ? sites.value?.find(s => s.id === ev.site_id)?.domain : undefined
  if (d.job_status === 'canceled') {
    // Asked for: neither a failure nor a success.
    toast.add({ title: t('tasks.toastCanceled', { kind }), description: domain, color: 'neutral', icon: 'i-cg-info' })
  } else if (d.status === 'failed') {
    toast.add({ title: t('tasks.toastFailed', { kind }), description: [domain, d.error].filter(Boolean).join(' · '), color: 'error', icon: 'i-cg-alert-circle' })
  } else {
    toast.add({ title: t('tasks.toastSucceeded', { kind }), description: domain, color: 'success', icon: 'i-cg-check-circle' })
  }
})
watch(() => route.path, () => { open.value = false })

function onCreated() {
  sitesVersion.value++
  if (route.path !== '/sites') void navigateTo('/sites')
}

const searchGroups = computed<CommandPaletteGroup<CommandPaletteItem>[]>(() => [
  {
    id: 'sites',
    label: t('nav.sites'),
    items: (sites.value ?? []).map(s => ({ label: s.domain, suffix: t(`site.type.${s.type}`), icon: siteTypeIcon[s.type], to: `/sites/${s.id}` }))
  },
  {
    id: 'links',
    label: t('nav.goTo'),
    items: [...groups.value.flatMap(g => g.links), { label: t('nav.security'), icon: 'i-cg-lock', to: '/security' }]
      .map(({ label, icon, to }) => ({ label, icon, to }))
  },
  ...(isAdmin.value
    ? [{
        id: 'actions',
        label: t('cg.shell.actions'),
        items: [{ label: t('site.new'), icon: 'i-cg-plus', onSelect: () => { creating.value = true } }]
      }]
    : [])
])
</script>

<template>
  <UDashboardGroup unit="rem">
    <UDashboardSidebar
      id="default"
      v-model:open="open"
      :default-size="15.5"
      :min-size="15.5"
      :max-size="15.5"
      toggle-side="right"
      :ui="{ root: 'border-default', content: 'w-[17rem] max-w-[85vw] bg-elevated', toggle: 'me-3' }"
    >
      <template #header>
        <NuxtLink to="/" class="flex h-full flex-1 items-center gap-2.5 text-highlighted" :aria-label="t('cg.shell.home')">
          <AppLogo wordmark />
        </NuxtLink>
      </template>

      <template #default>
        <div class="flex flex-col gap-1 border-b border-default px-5 py-3.5">
          <span class="hud text-dimmed">{{ t('nav.server') }}</span>
          <span class="mono truncate text-[13px] text-highlighted" :title="host">{{ host || '—' }}</span>
          <span v-if="version" class="mono text-xs text-muted">v{{ version }}</span>
        </div>

        <nav :aria-label="t('cg.shell.navigation')" class="flex flex-col gap-0.5 py-4">
          <template v-for="(g, gi) in groups" :key="g.label">
            <span class="hud px-[22px] pb-2 text-dimmed" :class="gi ? 'pt-[18px]' : ''">{{ g.label }}</span>
            <NuxtLink
              v-for="l in g.links"
              :key="l.to"
              :to="l.to"
              :aria-current="isActive(l) ? 'page' : undefined"
              class="mx-2 flex min-h-10 items-center gap-3 px-3.5 py-2 text-sm transition-colors duration-100"
              :class="isActive(l) ? 'bg-inverted font-semibold text-inverted' : 'text-default hover:bg-accented'"
            >
              <UIcon :name="l.icon" class="size-4 shrink-0" />
              <span class="min-w-0 flex-1 truncate">{{ l.label }}</span>
              <span v-if="l.count !== undefined" class="mono text-xs" :class="isActive(l) ? 'text-inverted/70' : 'text-muted'">{{ l.count }}</span>
            </NuxtLink>
          </template>
        </nav>
      </template>

      <template #footer>
        <UserMenu class="mt-auto w-full border-t border-default" />
      </template>
    </UDashboardSidebar>

    <UDashboardSearch :groups="searchGroups" :placeholder="t('cg.shell.search')" />

    <slot />

    <SiteCreateModal v-if="isAdmin" v-model:open="creating" @created="onCreated" />
    <StreamStatus />
  </UDashboardGroup>
</template>
