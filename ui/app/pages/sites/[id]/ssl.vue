<script setup lang="ts">
import type { CertificateStatus } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site, refresh: refreshSite } = useCurrentSite()

const { data: cert, refresh } = useLive(() => api.get<CertificateStatus>(`/api/sites/${site.value.id}/certificate`), { on: { types: ['cert.changed', 'task.finished'], sites: () => site.value.id }, fallbackMs: 30000 })
const custom = reactive({ fullchain: '', key: '' })

const issue = (wildcard: boolean) => run(() => api.post(`/api/sites/${site.value.id}/certificate`, { wildcard }), t('ssl.requested'))

async function setHSTS(on: boolean) {
  if (on) {
    const ok = await confirm({ title: t('ssl.hstsTitle'), description: t('ssl.hstsWarning'), confirmLabel: t('ssl.hstsEnable') })
    if (!ok) return
  }
  await run(() => api.patch(`/api/sites/${site.value.id}`, { hsts: on }), on ? t('ssl.hstsOn') : t('ssl.hstsOff'))
  await Promise.all([refresh(), refreshSite()])
}
async function uploadCustom() {
  const ok = await run(() => api.put(`/api/sites/${site.value.id}/certificate/custom`, custom), t('ssl.customInstalled'))
  if (ok) {
    custom.fullchain = ''
    custom.key = ''
    await refresh()
  }
}
async function removeCustom() {
  const ok = await confirm({ title: t('ssl.customRemoveTitle'), description: t('ssl.customRemoveHint'), confirmLabel: t('common.remove'), danger: true })
  if (!ok) return
  // Going back to Let's Encrypt requests the certificate for the site's
  // names as they are now (aliases added meanwhile were never issued for);
  // without an ACME e-mail nothing can be requested, and the API says so.
  let res: { task?: unknown, warning?: string } = {}
  if (await run(async () => { res = await api.del(`/api/sites/${site.value.id}/certificate/custom`) }, t('ssl.customRemoved'))) {
    if (res.warning) useToast().add({ title: t('ssl.noAcmeEmail'), color: 'warning' })
    await refresh()
  }
}
</script>

<template>
  <PanelSection :title="t('ssl.current')" icon="i-cg-certificate">
    <template v-if="cert?.exists" #actions>
      <UBadge :color="(cert.days_left ?? 0) < 14 ? 'error' : (cert.days_left ?? 0) < 30 ? 'warning' : 'success'" variant="subtle" size="lg" :label="t('ssl.expiresIn', { n: cert.days_left })" />
    </template>
    <SummaryList
      v-if="cert?.exists"
      :rows="[{ k: t('ssl.issuer'), v: cert.issuer || '—' }, { k: t('ssl.expires'), v: fmtDateTime(cert.not_after, locale) }]"
    />
    <p v-else class="m-0 text-sm text-muted">
      {{ t('ssl.none') }}
    </p>
    <UAlert v-if="cert?.last_renewal_error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('ssl.renewalFailed')" :description="cert.last_renewal_error" />
  </PanelSection>

  <div class="grid items-start gap-6 lg:grid-cols-2">
    <PanelSection title="Let's Encrypt" :description="t('ssl.leHint')">
      <template v-if="site.config.wildcard_cert" #actions>
        <UBadge color="info" variant="subtle" :label="t('ssl.wildcardActive')" />
      </template>
      <p class="m-0 text-[13px] text-muted">
        {{ t('ssl.wildcardHint', { domain: site.domain }) }}
      </p>
      <template #footer>
        <UButton icon="i-cg-refresh" :label="t('ssl.issue')" :disabled="!canManage" :loading="busy" @click="issue(false)" />
        <UButton color="neutral" variant="outline" :label="t('ssl.wildcard')" :disabled="!canManage" :loading="busy" @click="issue(true)" />
      </template>
    </PanelSection>

    <PanelSection title="HSTS" :description="t('ssl.hstsHint')">
      <USwitch :model-value="!!cert?.hsts" :label="cert?.hsts ? t('common.on') : t('common.off')" :disabled="!canManage || busy || (!cert?.exists && !cert?.hsts)" @update:model-value="setHSTS" />
    </PanelSection>
  </div>

  <PanelSection :title="t('ssl.custom')" :description="t('ssl.customHint')">
    <div class="grid gap-4 lg:grid-cols-2">
      <UFormField :label="t('ssl.fullchain')">
        <UTextarea v-model="custom.fullchain" :rows="6" class="w-full font-mono text-xs" placeholder="-----BEGIN CERTIFICATE-----" />
      </UFormField>
      <UFormField :label="t('ssl.privateKey')">
        <UTextarea v-model="custom.key" :rows="6" class="w-full font-mono text-xs" placeholder="-----BEGIN PRIVATE KEY-----" />
      </UFormField>
    </div>
    <template #footer>
      <UButton :label="t('ssl.install')" icon="i-cg-upload" :disabled="!canManage || !custom.fullchain || !custom.key" :loading="busy" @click="uploadCustom" />
      <UButton color="neutral" variant="outline" :label="t('ssl.customRemove')" :disabled="!canManage" @click="removeCustom" />
    </template>
  </PanelSection>
</template>
