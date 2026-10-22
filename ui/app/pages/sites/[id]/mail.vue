<script setup lang="ts">
/**
 * A site's mail (#77): PHP's mail() and wp_mail() go through the panel's
 * mail gate to a relay, the server's or the site's own, from the site's
 * sender (GET/PUT /api/sites/{id}/mail). The checklist reads the sender
 * domain's SPF, DMARC and DKIM records; the relay's provider says what
 * they must hold.
 */
interface RelayView { host: string, port: number, security: string, username: string, from: string, password_set: boolean }
interface SiteMail { off: boolean, from: string, dkim_selector: string, relay: RelayView | null, effective: 'site' | 'server' | 'none', sender: string }
interface RecordCheck { found: boolean, record?: string }
interface MailChecks { domain: string, spf: RecordCheck, dmarc: RecordCheck, dkim: { selector: string, found: boolean } }

const { t } = useI18n()
const api = useApi()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const { site } = useCurrentSite()
const base = computed(() => `/api/sites/${site.value.id}/mail`)

const { data: mail, refresh } = useLoad(() => api.get<SiteMail>(base.value))
const { data: checks, refresh: recheck } = useLoad(() => api.get<MailChecks>(`${base.value}/checks`))

const form = reactive({ on: true, from: '', selector: '', own: false, host: '', port: '', security: 'starttls', username: '', password: '', relayFrom: '' })
function load() {
  const m = mail.value
  if (!m) return
  Object.assign(form, {
    on: !m.off, from: m.from, selector: m.dkim_selector, own: !!m.relay,
    host: m.relay?.host ?? '', port: m.relay?.port ? String(m.relay.port) : '', security: m.relay?.security || 'starttls',
    username: m.relay?.username ?? '', password: m.relay?.password_set ? '••••••••' : '', relayFrom: m.relay?.from ?? ''
  })
}
watch(mail, load, { immediate: true })
const securityItems = computed(() => (['starttls', 'tls', 'none'] as const).map(v => ({ value: v, label: t(`mail.security.${v}`) })))

async function save() {
  const body: Record<string, unknown> = { off: !form.on, from: form.from, dkim_selector: form.selector }
  body.relay = form.own
    ? { host: form.host, port: Number(form.port) || 0, security: form.security, username: form.username, from: form.relayFrom, password: form.password }
    : null
  if (await run(() => api.put(base.value, body), t('common.saved'))) {
    await refresh()
    await recheck()
  }
}

const testTo = ref('')
const test = () => run(() => api.post(`${base.value}/test`, { to: testTo.value }), t('mail.testSent'))

const status = computed(() => {
  const m = mail.value
  if (!m) return ''
  if (site.value.staging_of) return t('mail.stagingNote')
  if (m.off) return t('mail.statusOff')
  if (m.effective === 'none') return t('mail.statusNone')
  return t(m.effective === 'site' ? 'mail.statusSite' : 'mail.statusServer', { sender: m.sender })
})
</script>

<template>
  <PanelSection :title="t('mail.title')" :description="t('mail.siteHint')">
    <UAlert :color="mail?.effective === 'none' || site.staging_of ? 'warning' : 'neutral'" variant="subtle" :title="status" />
    <template v-if="!site.staging_of">
      <USwitch v-model="form.on" :label="t('mail.send')" :disabled="!isAdmin" />
      <div class="grid gap-4 sm:grid-cols-2">
        <UFormField :label="t('mail.siteFrom')" :help="t('mail.siteFromHelp')">
          <UInput v-model="form.from" :placeholder="`noreply@${site.domain}`" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('mail.dkimSelector')" :help="t('mail.dkimSelectorHelp')">
          <UInput v-model="form.selector" placeholder="s1" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
      </div>
      <USwitch v-model="form.own" :label="t('mail.ownRelay')" :description="t('mail.ownRelayHint')" :disabled="!isAdmin" />
      <div v-if="form.own" class="grid gap-4 sm:grid-cols-2">
        <UFormField :label="t('mail.host')">
          <UInput v-model="form.host" placeholder="smtp.example.com" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
        <div class="grid grid-cols-2 gap-4">
          <UFormField :label="t('mail.securityLabel')">
            <USelect v-model="form.security" :items="securityItems" class="w-full" :disabled="!isAdmin" />
          </UFormField>
          <UFormField :label="t('mail.port')" :help="t('mail.portHelp')">
            <UInput v-model="form.port" inputmode="numeric" class="w-full font-mono" :disabled="!isAdmin" />
          </UFormField>
        </div>
        <UFormField :label="t('mail.username')">
          <UInput v-model="form.username" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('mail.password')">
          <UInput v-model="form.password" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
        <UFormField :label="t('mail.from')" :help="t('mail.relayFromHelp')">
          <UInput v-model="form.relayFrom" class="w-full font-mono" :disabled="!isAdmin" />
        </UFormField>
      </div>
    </template>
    <template v-if="!site.staging_of && (isAdmin || canManage)" #footer>
      <UButton v-if="isAdmin" :label="t('common.save')" :loading="busy" @click="save" />
      <UInput v-model="testTo" placeholder="test@example.com" class="w-full font-mono sm:w-64" />
      <UButton color="neutral" variant="outline" :label="t('mail.test')" :disabled="busy || !testTo || mail?.effective === 'none'" @click="test" />
    </template>
  </PanelSection>

  <PanelSection v-if="checks && !site.staging_of" :title="t('mail.checklist')" :description="t('mail.checklistHint', { domain: checks.domain })">
    <ul class="m-0 flex list-none flex-col gap-3 p-0">
      <li class="flex flex-col gap-1">
        <span class="flex items-center gap-2">
          <UBadge :color="checks.spf.found ? 'success' : 'warning'" variant="subtle" :label="checks.spf.found ? t('mail.found') : t('mail.missing')" />
          <span class="font-semibold">SPF</span>
        </span>
        <code v-if="checks.spf.record" class="mono break-all text-[13px] text-muted">{{ checks.spf.record }}</code>
        <span v-else class="text-[13px] text-muted">{{ t('mail.spfHint', { domain: checks.domain }) }}</span>
      </li>
      <li class="flex flex-col gap-1">
        <span class="flex items-center gap-2">
          <UBadge :color="checks.dkim.found ? 'success' : 'warning'" variant="subtle" :label="checks.dkim.found ? t('mail.found') : t('mail.missing')" />
          <span class="font-semibold">DKIM</span>
        </span>
        <span class="text-[13px] text-muted">{{ checks.dkim.selector ? t('mail.dkimHint', { name: `${checks.dkim.selector}._domainkey.${checks.domain}` }) : t('mail.dkimNoSelector') }}</span>
      </li>
      <li class="flex flex-col gap-1">
        <span class="flex items-center gap-2">
          <UBadge :color="checks.dmarc.found ? 'success' : 'warning'" variant="subtle" :label="checks.dmarc.found ? t('mail.found') : t('mail.missing')" />
          <span class="font-semibold">DMARC</span>
        </span>
        <code v-if="checks.dmarc.record" class="mono break-all text-[13px] text-muted">{{ checks.dmarc.record }}</code>
        <span v-else class="text-[13px] text-muted">{{ t('mail.dmarcHint', { domain: checks.domain }) }}</span>
      </li>
    </ul>
    <template #footer>
      <UButton color="neutral" variant="outline" icon="i-cg-refresh" :label="t('mail.recheck')" @click="recheck" />
    </template>
  </PanelSection>
</template>
