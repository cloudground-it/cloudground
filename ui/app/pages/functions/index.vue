<script setup lang="ts">
/**
 * The server's functions (issue 143, v0.9.0): Node.js or Python code that runs
 * on a request or on its schedule, at the functions domain or its own
 * (GET /api/functions; internal/api SPEC §17).
 */
import type { FnList } from '~/utils/functions'

const { t, locale } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
useHead({ title: () => t('fn.title') })

const { data, error, refresh } = useLoad(() => api.get<FnList>('/api/functions'))
useIntervalFn(() => void inBackground(refresh), 30000)
const fns = computed(() => data.value?.functions ?? null)

const columns = computed(() => [
  { id: 'name', header: t('fn.col.name') },
  { id: 'runtime', header: t('fn.col.runtime'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'urls', header: t('fn.col.addresses') },
  { id: 'auth', header: t('fn.col.auth'), meta: { class: { th: 'hidden sm:table-cell', td: 'hidden sm:table-cell' } } },
  { id: 'schedule', header: t('fn.col.schedule'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
  { id: 'last', header: t('fn.col.last') }
])
const shortURL = (u: string) => u.replace(/^https?:\/\//, '')
</script>

<template>
  <UDashboardPanel id="functions">
    <template #header>
      <PageNavbar :title="t('fn.title')" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('fn.title')" :description="t('fn.intro')">
          <template v-if="isAdmin" #links>
            <UButton v-bind="fns?.length ? { color: 'primary', variant: 'solid' } : { color: 'neutral', variant: 'outline' }" icon="i-cg-plus" :label="t('fn.new')" to="/functions/new" />
          </template>
        </UPageHeader>

        <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />

        <UAlert
          v-if="data && !data.domain"
          color="info"
          variant="subtle"
          icon="i-cg-info"
          :title="t('fn.noDomain.title')"
          :description="t('fn.noDomain.hint')"
          :actions="[{ label: t('fn.noDomain.action'), to: '/settings#functions', color: 'neutral', variant: 'outline', size: 'sm' }]"
          data-testid="fn-no-domain"
        />

        <div v-if="!data && !error" class="flex flex-col gap-2">
          <USkeleton v-for="i in 3" :key="i" class="h-14" />
        </div>

        <PanelSection v-else-if="fns && !fns.length">
          <UEmpty
            icon="i-cg-webhook"
            :title="t('fn.empty.title')"
            :description="t('fn.empty.hint')"
            :actions="isAdmin ? [{ label: t('fn.new'), icon: 'i-cg-plus', color: 'primary', variant: 'solid', to: '/functions/new' }] : []"
          />
        </PanelSection>

        <PanelSection v-else-if="fns" flush :label="t('fn.title')">
          <UTable :data="fns" :columns="columns">
            <template #name-cell="{ row: { original: f } }">
              <span class="flex min-w-0 items-center gap-2.5">
                <UIcon :name="fnRuntimeIcon(f.runtime)" class="size-[18px] shrink-0" aria-hidden="true" />
                <span class="flex min-w-0 flex-col">
                  <NuxtLink :to="`/functions/${f.name}`" class="mono text-[15px] font-semibold break-all text-highlighted decoration-1 underline-offset-[0.2em] hover:text-cg-blue hover:underline">{{ f.name }}</NuxtLink>
                  <span class="text-[13px] text-muted md:hidden">{{ fnRuntimeName(f.runtime) }} {{ f.version }}</span>
                </span>
              </span>
            </template>
            <template #runtime-cell="{ row: { original: f } }">
              <span class="text-muted">{{ fnRuntimeName(f.runtime) }} <span class="mono">{{ f.version }}</span></span>
            </template>
            <template #urls-cell="{ row: { original: f } }">
              <span v-if="f.urls.length" class="flex min-w-0 flex-col gap-0.5">
                <span v-for="u in f.urls.slice(0, 2)" :key="u" class="mono text-[13px] break-all">{{ shortURL(u) }}</span>
                <span v-if="f.urls.length > 2" class="text-[13px] text-muted">{{ t('fn.moreUrls', { n: f.urls.length - 2 }) }}</span>
              </span>
              <span v-else class="text-[13px] text-muted">{{ t('fn.notPublished') }}</span>
            </template>
            <template #auth-cell="{ row: { original: f } }">
              <UBadge :color="f.auth === 'token' ? 'neutral' : 'info'" variant="subtle" :icon="f.auth === 'token' ? 'i-cg-lock' : 'i-cg-unlock'" :label="t(`fn.auth.${f.auth}`)" />
            </template>
            <template #schedule-cell="{ row: { original: f } }">
              <span class="mono text-[13px]" :class="f.schedule ? '' : 'text-muted'">{{ f.schedule || '—' }}</span>
            </template>
            <template #last-cell="{ row: { original: f } }">
              <span v-if="f.last" class="flex items-center gap-2 whitespace-nowrap">
                <UBadge :color="httpStatusColor(f.last.status)" variant="subtle" class="mono" :label="String(f.last.status || '—')" />
                <span class="text-[13px] text-muted" :title="fmtDateTime(f.last.time, locale)">{{ fmtAgo(f.last.time, locale) }}</span>
              </span>
              <span v-else class="text-[13px] text-muted">{{ t('fn.never') }}</span>
            </template>
          </UTable>
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
