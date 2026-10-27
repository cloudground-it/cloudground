<script setup lang="ts">
/**
 * A site behind Cloudflare (#22): the visitor's address from CF-Connecting-IP,
 * trusted from Cloudflare's own addresses only, and the zone whose cache a purge
 * of the site also empties (GET and PUT /api/sites/{id}/cloudflare, live once saved).
 */
interface CloudflareSettings { real_ip: boolean, zone_id: string, purge_ready: boolean, ranges_updated_at?: string }

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/cloudflare`)
const { data, refresh } = useLoad(() => api.get<CloudflareSettings>(base.value))
const form = reactive({ realIP: false, zone: '' })
watch(data, (c) => {
  if (c) Object.assign(form, { realIP: c.real_ip, zone: c.zone_id })
}, { immediate: true })

const zoneOK = computed(() => /^[0-9a-f]{32}$/i.test(form.zone.trim()) || form.zone.trim() === '')
const ranges = computed(() => data.value?.ranges_updated_at
  ? t('cloudflare.rangesUpdated', { date: new Date(data.value.ranges_updated_at).toLocaleDateString(locale.value) })
  : t('cloudflare.rangesShipped'))

async function save() {
  if (await run(() => api.put(base.value, { real_ip: form.realIP, zone_id: form.zone.trim() }), t('cloudflare.saved'))) await refresh()
}
</script>

<template>
  <PanelSection :title="t('cloudflare.title')" :description="t('cloudflare.hint')">
    <template #actions>
      <UBadge v-if="data?.purge_ready" color="success" variant="subtle" :label="t('cloudflare.purgeReady')" />
    </template>
    <div class="flex flex-col gap-4">
      <UFormField :help="t('cloudflare.realIPHelp')">
        <USwitch v-model="form.realIP" :label="t('cloudflare.realIP')" :disabled="!canManage" />
      </UFormField>
      <p class="text-xs text-muted">
        {{ ranges }}
      </p>
      <UFormField :label="t('cloudflare.zone')" :help="t('cloudflare.zoneHelp')" :error="zoneOK ? undefined : t('cloudflare.zoneBad')">
        <UInput v-model="form.zone" class="w-full font-mono" maxlength="40" autocomplete="off" spellcheck="false" :disabled="!canManage" />
      </UFormField>
      <UAlert v-if="data?.zone_id && !data.purge_ready" color="neutral" variant="subtle" icon="i-cg-info" :description="t('cloudflare.purgeNeedsToken')" />
    </div>
    <template v-if="canManage" #footer>
      <UButton :label="t('common.save')" :disabled="!zoneOK" :loading="busy" @click="save" />
    </template>
  </PanelSection>
</template>
