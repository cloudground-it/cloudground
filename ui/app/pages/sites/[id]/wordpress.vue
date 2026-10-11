<script setup lang="ts">
import type { WPPlugin } from '~/types'

interface CacheStats { backend: string, hits: number, misses: number, entries: number, mem_used: number, mem_total: number, hit_rate_pct: number }

const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site, refresh } = useCurrentSite()

const { data: wp, pending } = useLoad(() => api.get<{ plugins: WPPlugin[] | null, themes: WPPlugin[] | null }>(`/api/sites/${site.value.id}/wp/plugins`))
const { data: stats } = useLoad(() => api.get<CacheStats>(`/api/sites/${site.value.id}/cache-stats`))
const pendingUpdates = computed(() => (wp.value?.plugins ?? []).filter(p => p.update && p.update !== 'none').length)

// wp-admin's one-click login is the site header's "Log in to WordPress".
const update = (what: string) => run(() => api.post(`/api/sites/${site.value.id}/wp/update`, { what }), t('wp.updating', { what }))
const objectCache = (enable: boolean) => run(() => api.patch(`/api/sites/${site.value.id}`, { object_cache: enable }), enable ? t('wp.ocEnabling') : t('wp.ocDisabling')).then(refresh)
const warm = () => run(() => api.post(`/api/sites/${site.value.id}/warm`), t('wp.warming'))

// Two-factor sign-in for the site's own accounts (#74), done by the connector.
const twoFactor = computed(() => site.value.config.wp_two_factor ?? 'off')
const twoFactorItems = computed(() => [
  { value: 'off', label: t('wp.tfOff') },
  { value: 'optional', label: t('wp.tfOptional'), description: t('wp.tfOptionalHint') },
  { value: 'required', label: t('wp.tfRequired'), description: t('wp.tfRequiredHint') }
])
const setTwoFactor = (mode: string) => run(() => api.put(`/api/sites/${site.value.id}/wp/two-factor`, { mode }), t('wp.tfSaved')).then(refresh)

const columns = computed(() => [
  { accessorKey: 'name', header: t('wp.name') },
  { accessorKey: 'status', header: t('wp.status') },
  { accessorKey: 'version', header: t('wp.version') },
  { accessorKey: 'update', header: t('wp.update') }
])
</script>

<template>
  <PanelSection :title="t('wp.updates')" :description="pendingUpdates ? t('wp.updatesPending', { n: pendingUpdates }) : t('wp.upToDate')" flush>
    <template #actions>
      <UButton color="neutral" variant="outline" label="Core" :disabled="!canManage || busy" @click="update('core')" />
      <UButton color="neutral" variant="outline" :label="t('wp.plugins')" :disabled="!canManage || busy" @click="update('plugins')" />
      <UButton icon="i-cg-download" :label="t('wp.updateAll')" :disabled="!canManage || busy" @click="update('all')" />
    </template>
    <UTable :data="wp?.plugins ?? []" :columns="columns" :loading="pending" :empty="t('wp.noPlugins')">
      <template #name-cell="{ row }">
        <span class="mono text-[13px] font-semibold text-highlighted">{{ row.original.name }}</span>
      </template>
      <template #status-cell="{ row }">
        <UBadge :color="row.original.status === 'active' ? 'success' : 'neutral'" variant="subtle" :label="row.original.status" />
      </template>
      <template #version-cell="{ row }">
        <span class="mono text-[13px]">{{ row.original.version }}</span>
      </template>
      <template #update-cell="{ row }">
        <UBadge v-if="row.original.update && row.original.update !== 'none'" color="warning" variant="subtle" :label="row.original.update" />
        <span v-else class="text-dimmed">—</span>
      </template>
    </UTable>
  </PanelSection>

  <PanelSection v-if="wp?.themes?.length" :title="t('wp.themes')" flush>
    <UTable :data="wp.themes" :columns="columns">
      <template #name-cell="{ row }">
        <span class="mono text-[13px] font-semibold text-highlighted">{{ row.original.name }}</span>
      </template>
      <template #status-cell="{ row }">
        <UBadge :color="row.original.status === 'active' ? 'success' : 'neutral'" variant="subtle" :label="row.original.status" />
      </template>
      <template #version-cell="{ row }">
        <span class="mono text-[13px]">{{ row.original.version }}</span>
      </template>
      <template #update-cell="{ row }">
        <UBadge v-if="row.original.update && row.original.update !== 'none'" color="warning" variant="subtle" :label="row.original.update" />
        <span v-else class="text-dimmed">—</span>
      </template>
    </UTable>
  </PanelSection>

  <div class="grid items-start gap-6 lg:grid-cols-2">
    <PanelSection :title="t('wp.objectCache')" :description="t('wp.objectCacheHint')">
      <USwitch :model-value="site.config.object_cache" :label="site.config.object_cache ? t('common.on') : t('common.off')" :disabled="!canManage || busy" @update:model-value="objectCache" />
      <p v-if="stats && stats.backend === 'apcu'" class="m-0 text-[13px] text-muted">
        {{ t('wp.ocStats', { rate: fmtPercent(stats.hit_rate_pct, 1), entries: fmtNumber(stats.entries), mem: fmtBytes(stats.mem_used) }) }}
      </p>
    </PanelSection>
    <PanelSection :title="t('wp.warm')" :description="t('wp.warmHint')">
      <template #footer>
        <UButton color="neutral" variant="outline" icon="i-cg-warm" :label="t('wp.warmNow')" :disabled="!canManage || busy" @click="warm" />
      </template>
    </PanelSection>
  </div>

  <PanelSection :title="t('wp.twoFactor')" :description="t('wp.twoFactorHint')">
    <URadioGroup :model-value="twoFactor" :items="twoFactorItems" :disabled="!canManage || busy" @update:model-value="v => setTwoFactor(String(v))" />
    <p class="m-0 text-[13px] text-muted">
      {{ t('wp.tfMagicNote') }}
    </p>
  </PanelSection>
</template>
