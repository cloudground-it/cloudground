<script setup lang="ts">
import type { WPHardening, WPPlugin } from '~/types'

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
// Automatic updates (#20): the same snapshot, check and rollback as the
// buttons, at the hour chosen. "Off" keeps the rest for when it is on.
const caps = useCapabilities()
const storedSchedule = computed(() => site.value.config.wp_updates ?? {})
const schedForm = reactive({ frequency: 'off' as 'off' | 'daily' | 'weekly', hour: -1, what: 'all' as 'core' | 'plugins' | 'themes' | 'all' })
function loadSchedule() {
  const p = storedSchedule.value
  schedForm.frequency = p.enabled ? (p.schedule || 'weekly') : 'off'
  schedForm.hour = p.hour ?? -1
  schedForm.what = p.what || 'all'
}
loadSchedule()
watch(storedSchedule, loadSchedule, { deep: true })
const schedFrequencyItems = computed(() => (['off', 'daily', 'weekly'] as const).map(v => ({ label: t(`backup.frequency.${v}`), value: v })))
const schedHourItems = computed(() => [
  { label: t('backup.hourAny'), value: -1 },
  ...Array.from({ length: 24 }, (_, h) => ({ label: `${String(h).padStart(2, '0')}:00`, value: h }))
])
const schedWhatItems = computed(() => (['all', 'core', 'plugins', 'themes'] as const).map(v => ({ label: t(`wp.what.${v}`), value: v })))
const scheduleChanged = computed(() => {
  const p = storedSchedule.value
  const enabled = schedForm.frequency !== 'off'
  return enabled !== !!p.enabled || (enabled && (schedForm.frequency !== (p.schedule || 'weekly') || schedForm.hour !== (p.hour ?? -1) || schedForm.what !== (p.what || 'all')))
})
async function saveSchedule() {
  const enabled = schedForm.frequency !== 'off'
  const body: Record<string, unknown> = enabled
    ? { enabled, schedule: schedForm.frequency, hour: schedForm.hour, what: schedForm.what }
    : { enabled }
  if (await run(() => api.put(`/api/sites/${site.value.id}/wp/updates`, body), t('wp.scheduleSaved'))) await refresh()
}

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

// Hardening (#75): each item on its own; the recommended set leaves the
// login path alone, since it changes where people sign in.
const hardening = computed<WPHardening>(() => site.value.config.wp_hardening ?? {})
const hardeningItems = computed(() => [
  { key: 'xmlrpc_off' as const, label: t('wp.hXmlrpc'), hint: t('wp.hXmlrpcHint') },
  { key: 'file_edit_off' as const, label: t('wp.hFileEdit'), hint: t('wp.hFileEditHint') },
  { key: 'hide_version' as const, label: t('wp.hVersion'), hint: t('wp.hVersionHint') },
  { key: 'block_readme' as const, label: t('wp.hReadme'), hint: t('wp.hReadmeHint') }
])
const allowText = ref('')
const loginPath = ref('')
watch(hardening, (h) => {
  allowText.value = (h.xmlrpc_allow ?? []).join('\n')
  loginPath.value = h.login_path ?? ''
}, { immediate: true })
const loginPathOK = computed(() => loginPath.value === '' || /^[a-z0-9][a-z0-9-]{2,63}$/.test(loginPath.value))
const saveHardening = (change: Partial<WPHardening>) => run(() => api.put(`/api/sites/${site.value.id}/wp/hardening`, { ...hardening.value, ...change }), t('wp.hSaved')).then(refresh)
const recommended = () => saveHardening({ xmlrpc_off: true, file_edit_off: true, hide_version: true, block_readme: true })
const saveDetails = () => saveHardening({
  xmlrpc_allow: allowText.value.split(/[\s,]+/).filter(Boolean),
  login_path: loginPath.value
})

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

  <PanelSection :title="t('wp.schedule')" :description="t('wp.safeUpdateHint')">
    <div v-if="!site.staging_of" class="grid gap-4 sm:grid-cols-3">
      <UFormField :label="t('backup.frequencyLabel')">
        <USelect v-model="schedForm.frequency" :items="schedFrequencyItems" class="w-full" :disabled="!canManage" />
      </UFormField>
      <UFormField v-if="schedForm.frequency !== 'off'" :label="t('backup.hour')" :help="t('wp.scheduleHourHelp', { zone: caps?.time_zone || 'UTC' })">
        <USelect v-model="schedForm.hour" :items="schedHourItems" class="w-full" :disabled="!canManage" />
      </UFormField>
      <UFormField v-if="schedForm.frequency !== 'off'" :label="t('wp.scheduleWhat')">
        <USelect v-model="schedForm.what" :items="schedWhatItems" class="w-full" :disabled="!canManage" />
      </UFormField>
    </div>
    <p class="m-0 text-[13px] text-muted">
      {{ site.staging_of ? t('wp.scheduleStaging') : t('wp.safeUpdateRepo') }}
    </p>
    <template v-if="canManage && !site.staging_of" #footer>
      <UButton :label="t('common.save')" :loading="busy" :disabled="!scheduleChanged" @click="saveSchedule" />
    </template>
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

  <SiteWPCleanup />

  <PanelSection :title="t('wp.hardening')" :description="t('wp.hardeningHint')">
    <template #actions>
      <UButton color="neutral" variant="outline" icon="i-cg-lock" :label="t('wp.hRecommended')" :disabled="!canManage || busy" @click="recommended" />
    </template>
    <div class="flex flex-col gap-4">
      <USwitch
        v-for="item in hardeningItems"
        :key="item.key"
        :model-value="!!hardening[item.key]"
        :label="item.label"
        :description="item.hint"
        :disabled="!canManage || busy"
        @update:model-value="v => saveHardening({ [item.key]: v })"
      />
      <UFormField v-if="hardening.xmlrpc_off" :label="t('wp.hAllow')" :help="t('wp.hAllowHelp')">
        <UTextarea v-model="allowText" :rows="3" class="w-full font-mono text-xs" placeholder="192.0.2.10&#10;198.51.100.0/24" :disabled="!canManage" />
      </UFormField>
      <UFormField :label="t('wp.hLoginPath')" :help="t('wp.hLoginPathHelp')" :error="loginPathOK ? undefined : t('wp.hLoginPathBad')">
        <UInput v-model="loginPath" class="w-full font-mono sm:w-80" placeholder="my-door" :disabled="!canManage" />
      </UFormField>
    </div>
    <template #footer>
      <UButton :label="t('common.save')" :disabled="!canManage || busy || !loginPathOK" :loading="busy" @click="saveDetails" />
    </template>
  </PanelSection>

  <PanelSection :title="t('wp.twoFactor')" :description="t('wp.twoFactorHint')">
    <URadioGroup :model-value="twoFactor" :items="twoFactorItems" :disabled="!canManage || busy" @update:model-value="v => setTwoFactor(String(v))" />
    <p class="m-0 text-[13px] text-muted">
      {{ t('wp.tfMagicNote') }}
    </p>
  </PanelSection>
</template>
