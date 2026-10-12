<script setup lang="ts">
import type { CommandPaletteGroup, CommandPaletteItem } from '@nuxt/ui'
import type { Site, UpdateStatus } from '~/types'

const { t } = useI18n()
const route = useRoute()
const api = useApi()
const { user, isAdmin } = useAuth()
const { sites, sitesVersion, version } = useShell()
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
        { label: t('nav.blocklist'), icon: 'i-cg-shield-check', to: '/blocklist' },
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
        items: [{ label: t('site.new'), icon: 'i-cg-plus', to: '/sites/new' }]
      }]
    : [])
])
</script>

<template>
  <UDashboardGroup unit="rem">
    <UDashboardSidebar
      id="default"
      v-model:open="open"
      :default-size="15"
      :min-size="15"
      :max-size="15"
      toggle-side="right"
      :ui="{ root: 'border-default', content: 'w-[16rem] max-w-[85vw] bg-elevated', toggle: 'me-3' }"
    >
      <template #header>
        <NuxtLink to="/" class="flex h-full flex-1 items-center gap-2.5 text-highlighted" :aria-label="t('cg.shell.home')">
          <AppLogo wordmark />
        </NuxtLink>
      </template>

      <template #default>
        <div class="mx-3 mt-1 mb-3 flex flex-col border border-default bg-default px-2.5 py-2 text-[13px] leading-snug">
          <span class="sr-only">{{ t('nav.server') }}</span>
          <span class="mono truncate text-sm font-medium text-highlighted" :title="host">{{ host || '—' }}</span>
          <span v-if="version" class="text-muted">CloudGround <span class="mono">{{ version.startsWith('v') ? version : `v${version}` }}</span></span>
        </div>

        <nav :aria-label="t('cg.shell.navigation')" class="flex flex-col px-3 pb-4">
          <template v-for="(g, gi) in groups" :key="g.label">
            <div v-if="gi" class="my-2 h-px bg-(--ui-border)" aria-hidden="true" />
            <h2 class="sr-only">{{ g.label }}</h2>
            <NuxtLink
              v-for="l in g.links"
              :key="l.to"
              :to="l.to"
              :aria-current="isActive(l) ? 'page' : undefined"
              class="mb-px flex min-h-9 items-center gap-2.5 rounded-[3px] px-2.5 py-1.5 text-[15px] transition-colors duration-100"
              :class="isActive(l) ? 'bg-inverted font-medium text-inverted' : 'text-default hover:bg-accented hover:text-highlighted'"
            >
              <UIcon :name="l.icon" class="size-[18px] shrink-0" />
              <span class="min-w-0 flex-1 truncate">{{ l.label }}</span>
              <span v-if="l.count !== undefined" class="mono text-[13px]" :class="isActive(l) ? 'text-inverted' : 'text-muted'">{{ l.count }}</span>
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

    <StreamStatus />
  </UDashboardGroup>
</template>
