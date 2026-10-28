<script setup lang="ts">
/**
 * One function: its header and tabs (Code, Addresses, Settings, Runs,
 * Releases), each a child page reading the function through
 * useCurrentFunction (GET /api/functions/{name}).
 */
import type { FnList, FnView } from '~/utils/functions'

const { t } = useI18n()
const api = useApi()
const route = useRoute()

const name = computed(() => String(route.params.name))
const base = computed(() => `/api/functions/${name.value}`)
const fn = ref<FnView | null>(null)
const domain = ref('')
const loadError = ref<string | null>(null)

async function refresh() {
  try {
    const [f, list] = await Promise.all([api.get<FnView>(base.value), api.get<FnList>('/api/functions')])
    fn.value = f
    domain.value = list.domain
    loadError.value = null
  } catch (e) {
    if (isApiError(e) && e.status === 404) await navigateTo('/functions')
    else loadError.value = errorMessage(e)
  }
}
await refresh()
watch(name, refresh)

provide(currentFunctionKey, { fn: fn as Ref<FnView>, domain, base, refresh })
useHead({ title: () => fn.value?.name ?? t('fn.title') })

const root = computed(() => `/functions/${name.value}`)
const tabs = computed(() => [
  { label: t('fn.tabs.code'), icon: 'i-cg-code', to: root.value, exact: true },
  { label: t('fn.tabs.addresses'), icon: 'i-cg-link', to: `${root.value}/addresses` },
  { label: t('fn.tabs.settings'), icon: 'i-cg-sliders', to: `${root.value}/settings` },
  { label: t('fn.tabs.runs'), icon: 'i-cg-activity', to: `${root.value}/runs` },
  { label: t('fn.tabs.releases'), icon: 'i-cg-release', to: `${root.value}/releases` }
].map(l => ({ label: l.label, icon: l.icon, to: l.to, active: l.exact ? route.path === l.to : route.path.startsWith(l.to) })))

const meta = computed(() => {
  const f = fn.value
  if (!f) return ''
  const parts = [`${fnRuntimeName(f.runtime)} ${f.version}`, `${f.memory_mb} MB`, `${f.timeout_sec} s`]
  if (f.schedule) parts.push(f.schedule)
  return parts.join(' · ')
})
</script>

<template>
  <UDashboardPanel id="function">
    <template #header>
      <PageNavbar :title="fn?.name ?? name" :section="t('fn.title')" section-to="/functions" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader v-if="fn" :description="meta" :ui="{ description: 'mono' }">
          <template #title>
            <UIcon :name="fnRuntimeIcon(fn.runtime)" class="size-7 shrink-0" aria-hidden="true" />
            <span class="mono min-w-0">{{ fn.name }}</span>
            <UBadge :color="fn.auth === 'token' ? 'neutral' : 'info'" variant="subtle" size="lg" :icon="fn.auth === 'token' ? 'i-cg-lock' : 'i-cg-unlock'" :label="t(`fn.auth.${fn.auth}`)" />
          </template>
          <template #links>
            <UButton v-if="fn.urls[0]" :to="fn.urls[0]" target="_blank" color="neutral" variant="outline" trailing-icon="i-cg-external-link" :label="t('fn.open')" :ui="{ label: 'hidden sm:inline' }" :aria-label="t('fn.open')" />
          </template>
        </UPageHeader>

        <TabBar v-if="fn" :items="tabs" :label="t('fn.tabs.label')" />

        <UAlert v-if="loadError" color="error" variant="subtle" icon="i-cg-alert-circle" :title="loadError" />
        <NuxtPage v-if="fn" />
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
