<script setup lang="ts">
import type { Profile } from '~/types'

const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site, refresh } = useCurrentSite()

const policy = reactive({ profile: site.value.profile as Profile, cache_enabled: site.value.config.cache_enabled, cache_ttl_sec: site.value.config.cache_ttl_sec || 0 })
const purgeURL = ref('')
const profileItems = computed(() => (['balanced', 'commerce', 'maximum'] as const).map(v => ({ label: `${t(`site.profile.${v}`)} — ${t(`site.profileHint.${v}`)}`, value: v })))

// Development mode (#80): the cache is bypassed for a while; the panel ends it
// by itself, and this counts down to it.
const devLengths = [15, 30, 60]
const now = ref(Date.now())
const devUntil = computed(() => (site.value.config.dev_mode_until ? Date.parse(site.value.config.dev_mode_until) : 0))
const devLeft = computed(() => (devUntil.value ? Math.max(0, Math.ceil((devUntil.value - now.value) / 1000)) : 0))
const devClock = computed(() => `${Math.floor(devLeft.value / 60)}:${String(devLeft.value % 60).padStart(2, '0')}`)
const canDev = computed(() => site.value.config.cache_enabled && site.value.type !== 'static' && site.value.type !== 'proxy')
let tick: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  tick = setInterval(() => {
    now.value = Date.now()
    // The panel ends the mode within a minute of its time: look again until it has.
    if (devUntil.value && devLeft.value === 0 && now.value % 10000 < 1000) void refresh()
  }, 1000)
})
onBeforeUnmount(() => clearInterval(tick))
function setDevMode(minutes: number) {
  void run(() => api.put(`/api/sites/${site.value.id}/dev-mode`, { minutes }), minutes ? t('cache.devStarted') : t('cache.devEnded')).then(refresh)
}

function apply() {
  void run(() => api.patch(`/api/sites/${site.value.id}`, { ...policy, cache_ttl_sec: Number(policy.cache_ttl_sec) || 0 }), t('common.applied')).then(refresh)
}
function purgeOne() {
  void run(() => api.post(`/api/sites/${site.value.id}/purge`, { urls: [purgeURL.value] }), t('cache.purgedOne')).then((ok) => {
    if (ok) purgeURL.value = ''
  })
}
function purgeAll() {
  void run(() => api.post(`/api/sites/${site.value.id}/purge`), t('cache.purgedAll'))
}
</script>

<template>
  <div class="grid items-start gap-6 lg:grid-cols-2">
    <PanelSection :title="t('cache.policy')" :description="t('cache.policyHint')">
      <div class="flex flex-col gap-4">
        <UFormField :label="t('site.profileLabel')">
          <USelect v-model="policy.profile" :items="profileItems" class="w-full" :disabled="!canManage" />
        </UFormField>
        <USwitch v-model="policy.cache_enabled" :label="t('cache.pageCache')" :disabled="!canManage" />
        <UFormField :label="t('cache.ttl')" :help="t('cache.ttlHint')">
          <UInputNumber v-model="policy.cache_ttl_sec" :min="0" :step="60" class="w-48" :disabled="!canManage" />
        </UFormField>
      </div>
      <template v-if="canManage" #footer>
        <UButton :label="t('common.apply')" :loading="busy" @click="apply" />
      </template>
    </PanelSection>

    <PanelSection :title="t('cache.dev')" :description="t('cache.devHint')">
      <p v-if="!canDev" class="text-sm text-muted">
        {{ t('cache.devNone') }}
      </p>
      <div v-else-if="devUntil" class="flex flex-col gap-1" role="status">
        <span class="font-mono text-2xl tabular-nums">{{ devLeft ? devClock : '…' }}</span>
        <span class="text-sm text-muted">{{ devLeft ? t('cache.devOn', { time: new Date(devUntil).toLocaleTimeString() }) : t('cache.devEnding') }}</span>
      </div>
      <p v-else class="text-sm text-muted">
        {{ t('cache.devOff') }}
      </p>
      <template v-if="canManage && canDev" #footer>
        <div class="flex flex-wrap gap-2">
          <UButton
            v-for="m in devLengths"
            :key="m"
            color="neutral"
            variant="outline"
            :label="t('cache.devFor', { n: m })"
            :disabled="busy"
            @click="setDevMode(m)"
          />
          <UButton v-if="devUntil" icon="i-cg-purge" :label="t('cache.devEnd')" :loading="busy" @click="setDevMode(0)" />
        </div>
      </template>
    </PanelSection>

    <PanelSection :title="t('cache.purge')" :description="t('cache.purgeHint')">
      <UFormField :label="t('cache.purgeOne')">
        <UFieldGroup class="w-full">
          <UInput v-model="purgeURL" :placeholder="`https://${site.domain}/blog/post/`" class="min-w-0 flex-1" />
          <UButton color="neutral" variant="outline" :label="t('cache.purgeButton')" :disabled="!canManage || !purgeURL" :loading="busy" @click="purgeOne" />
        </UFieldGroup>
      </UFormField>
      <template #footer>
        <UButton icon="i-cg-purge" :label="t('cache.purgeAll')" :disabled="!canManage" @click="purgeAll" />
      </template>
    </PanelSection>
  </div>
</template>
