<script setup lang="ts">
import type { AuditEvent } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
useHead({ title: () => t('nav.audit') })

const { data: events, pending } = useLoad(() => api.get<AuditEvent[]>('/api/audit'))
const q = ref('')
const filtered = computed(() => {
  const needle = q.value.trim().toLowerCase()
  return (events.value ?? []).filter(e => !needle || `${e.actor} ${e.action} ${e.subject} ${e.detail}`.toLowerCase().includes(needle))
})
const columns = computed(() => [
  { id: 'when', header: t('audit.when') },
  { id: 'actor', header: t('audit.actor') },
  { id: 'action', header: t('audit.action') },
  { id: 'subject', header: t('audit.subject'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'detail', header: t('audit.detail'), meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } }
])
function tone(action: string) {
  if (/delete|disable|block|fail|revoke/.test(action)) return 'error' as const
  if (/create|enable|login/.test(action)) return 'success' as const
  return 'neutral' as const
}
</script>

<template>
  <UDashboardPanel id="audit">
    <template #header>
      <PageNavbar :title="t('nav.audit')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.audit')" :description="t('ux.pages.audit')" />
        <PanelSection flush :label="t('nav.audit')">
          <template #toolbar>
            <div class="flex flex-wrap items-center justify-between gap-3">
              <span class="text-sm text-muted">{{ t('ux.audit.count', events?.length ?? 0) }}</span>
              <UInput v-model="q" icon="i-cg-search" type="search" :placeholder="t('audit.search')" :aria-label="t('audit.search')" class="w-72 max-w-full" />
            </div>
          </template>
          <USkeleton v-if="pending && !events" class="m-4 h-64" />
          <UEmpty v-else-if="!filtered.length" icon="i-cg-history" :title="t('audit.none')" />
          <UTable v-else :data="filtered" :columns="columns">
            <template #when-cell="{ row: { original: e } }">
              <span class="text-[13px] whitespace-nowrap text-muted" :title="fmtDateTime(e.created_at, locale)">{{ fmtAgo(e.created_at, locale) }}</span>
            </template>
            <template #actor-cell="{ row: { original: e } }">
              {{ e.actor }}
            </template>
            <template #action-cell="{ row: { original: e } }">
              <UBadge :color="tone(e.action)" variant="subtle" class="font-mono normal-case tracking-normal" :label="e.action" />
            </template>
            <template #subject-cell="{ row: { original: e } }">
              {{ e.subject }}
            </template>
            <template #detail-cell="{ row: { original: e } }">
              <span class="block max-w-80 truncate text-muted" :title="e.detail">{{ e.detail }}</span>
            </template>
          </UTable>
        </PanelSection>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
