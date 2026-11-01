<script setup lang="ts">
import type { CertificateStatus } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site, refresh: refreshSite } = useCurrentSite()

const { data: cert, refresh } = useLive(() => api.get<CertificateStatus>(`/api/sites/${site.value.id}/certificate`), { on: { types: ['cert.changed', 'task.finished'], sites: () => site.value.id }, fallbackMs: 30000 })
const { tasks } = useTasks()
// The newest issue or renewal of this site: running now, or how it ended.
const lastTask = computed(() => tasks.value.find(x => x.site_id === site.value.id && (x.kind === 'certificate.issue' || x.kind === 'certificate.renew')))
const issuing = computed(() => lastTask.value?.status === 'pending' || lastTask.value?.status === 'running')
const failure = computed(() => lastTask.value?.status === 'failed' && lastTask.value.error ? lastTask.value.error : cert.value?.last_renewal_error)
const wildcard = computed(() => !!site.value.config.wildcard_cert || !!cert.value?.names?.some(n => n.startsWith('*.')))
const daysLeft = computed(() => cert.value?.days_left ?? 0)
// One plain line on top: what the site has, or is getting, or why not.
const status = computed(() => {
  if (!cert.value) return null
  if (issuing.value) return { color: 'info' as const, icon: 'i-cg-loader', title: t('ssl.issuing'), text: t('ssl.issuingHint') }
  if (!cert.value.exists) {
    return failure.value
      ? { color: 'error' as const, icon: 'i-cg-alert-circle', title: t('ssl.failed'), text: `${failure.value.replace(/[.\s]+$/, '')}. ${t('ssl.failedHint')}` }
      : { color: 'neutral' as const, icon: 'i-cg-certificate', title: t('ssl.notIssued'), text: t('ssl.notIssuedHint') }
  }
  if (daysLeft.value < 30) {
    return { color: daysLeft.value < 14 ? 'error' as const : 'warning' as const, icon: 'i-cg-alert-triangle', title: t('ssl.expiringSoon'), text: t('ssl.expiringHint', { n: daysLeft.value }) }
  }
  const kind = cert.value.custom ? 'Custom' : wildcard.value ? 'Wildcard' : 'Le'
  return { color: 'success' as const, icon: 'i-cg-check-circle', title: t('ssl.active'), text: t(`ssl.active${kind}`) }
})
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
    <UAlert v-if="status" :color="status.color" variant="subtle" :icon="status.icon" :title="status.title" :description="status.text" :ui="{ icon: issuing ? 'animate-spin' : '' }" />
    <UProgress v-if="issuing" :model-value="lastTask?.progress" size="2xs" />
    <SummaryList
      v-if="cert?.exists"
      :rows="[
        { k: t('ssl.issuer'), v: cert.issuer || '—' },
        { k: t('ssl.expires'), v: fmtDateTime(cert.not_after, locale) },
        ...(cert.names?.length ? [{ k: t('ssl.names'), v: cert.names.join(', '), mono: true }] : [])
      ]"
    />
    <UAlert v-if="cert?.exists && !issuing && failure" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('ssl.renewalFailed')" :description="failure" />
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
        <UButton icon="i-cg-refresh" :label="cert?.exists ? t('ssl.renew') : t('ssl.issue')" :disabled="!canManage || issuing" :loading="busy" @click="issue(false)" />
        <UButton color="neutral" variant="outline" :label="t('ssl.wildcard')" :disabled="!canManage || issuing" :loading="busy" @click="issue(true)" />
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
