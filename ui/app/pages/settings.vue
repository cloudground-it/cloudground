<script setup lang="ts">
import type { DestinationKind } from '~/utils/backupDestination'

const { t } = useI18n()
const api = useApi()
const toast = useToast()
const { isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
useHead({ title: () => t('nav.settings') })

const saved = ref<Record<string, string>>({})
const form = ref<Record<string, string>>({})
const panelDomain = ref('')

async function load() {
  saved.value = await api.get<Record<string, string>>('/api/settings')
  form.value = { ...saved.value }
  panelDomain.value = saved.value.panel_domain ?? ''
  readDestination()
}

/** Saves only the given keys, and only those that changed: secrets come back masked. */
async function save(keys: string[]) {
  const body: Record<string, string> = {}
  for (const k of keys) {
    if ((form.value[k] ?? '') !== (saved.value[k] ?? '')) body[k] = form.value[k] ?? ''
  }
  if (!Object.keys(body).length) {
    toast.add({ title: t('settings.nothingChanged'), color: 'neutral' })
    return
  }
  await run(async () => {
    saved.value = await api.put<Record<string, string>>('/api/settings', body)
    form.value = { ...saved.value }
    readDestination()
    generated.value = ''
  }, t('common.saved'))
}

// ---- off-site backups: a destination and the fields it needs ----
// The presets fill in the endpoint; what is saved is the same repository
// string and region the API has always taken (utils/backupDestination).
const BACKUP_KEYS = ['backup_repository', 'backup_password', 'backup_s3_access_key', 'backup_s3_secret_key', 'backup_s3_region', 'backup_b2_account_id', 'backup_b2_account_key']

// Destinations typed by hand (issue 23): B2's native API takes its own
// credentials; SFTP signs in with the panel's key, made here.
const advancedRepo = computed(() => dest.value.kind === 'advanced' ? (form.value.backup_repository ?? '').trim() : '')
const isSFTP = computed(() => advancedRepo.value.startsWith('sftp:'))
const isB2 = computed(() => advancedRepo.value.startsWith('b2:'))
const sshKey = ref('')
async function loadSSHKey() {
  if (!isAdmin.value) return
  try {
    sshKey.value = (await api.get<{ public_key: string }>('/api/settings/backup-ssh-key')).public_key
  } catch {
    sshKey.value = ''
  }
}
watch(isSFTP, (on) => { if (on && !sshKey.value) void loadSSHKey() })
onMounted(() => { if (isSFTP.value) void loadSSHKey() })
async function makeSSHKey() {
  if (sshKey.value && !await confirm({ title: t('ux.backup.sshNewTitle'), description: t('ux.backup.sshNewHint'), danger: true, confirmLabel: t('ux.backup.sshNew') })) return
  await run(async () => {
    sshKey.value = (await api.post<{ public_key: string }>('/api/settings/backup-ssh-key', {})).public_key
  }, t('ux.backup.sshMade'))
}
const dest = ref(blankDestination())
function readDestination() {
  dest.value = destinationOf(saved.value.backup_repository ?? '', saved.value.backup_s3_region ?? '')
  // Nothing configured yet: the field starts empty, not on a provider.
  if (!saved.value.backup_repository) dest.value.kind = 'r2'
}
await load().catch(e => toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error' }))

const kindItems = computed(() => DESTINATION_KINDS.map(k => ({ value: k, label: t(`ux.backup.dest.${k}`) })))
const preset = computed(() => dest.value.kind in DESTINATION_PRESETS ? DESTINATION_PRESETS[dest.value.kind as keyof typeof DESTINATION_PRESETS] : null)
const s3 = computed(() => isS3Destination(dest.value.kind))
const repository = computed(() => repositoryOf(dest.value))
// The form's repository and region follow the destination, except in
// advanced mode, where the string is typed as it is.
watch([repository, () => dest.value.kind, () => dest.value.region], () => {
  if (dest.value.kind === 'advanced') return
  form.value.backup_repository = repository.value
  if (repository.value) form.value.backup_s3_region = regionOf(dest.value)
})
function onKind(k: DestinationKind) {
  // Advanced starts from what the other fields made, so nothing is lost.
  const raw = repository.value || saved.value.backup_repository || ''
  dest.value = { ...dest.value, kind: k, raw: k === 'advanced' ? raw : dest.value.raw }
}

// The repository password: generated once, kept by the person (it is the
// only key to the backups), never shown again after saving.
const generated = ref('')
const { copy: copyText } = useClipboard({ legacy: true })
const hasPassword = computed(() => !!saved.value.backup_password)
function generatePassword() {
  generated.value = newRepositoryPassword()
  form.value.backup_password = generated.value
}
function downloadPassword() {
  const text = `${t('settings.repoPassword')}: ${generated.value}\n${t('settings.backups')}: ${repository.value || form.value.backup_repository || ''}\n`
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
  const a = document.createElement('a')
  a.href = url
  a.download = 'cloudground-backup-password.txt'
  a.click()
  URL.revokeObjectURL(url)
}
const backupDirty = computed(() => BACKUP_KEYS.some(k => (form.value[k] ?? '') !== (saved.value[k] ?? '')))

// Who must have two-factor (issue 156): nobody, administrators, every account.
const require2fa = computed({
  get: () => form.value.require_2fa || 'off',
  set: (v: string) => { form.value.require_2fa = v }
})
const require2faItems = computed(() => (['off', 'admins', 'all'] as const).map(v => ({ label: t({ off: 'settings.require2faOff', admins: 'settings.require2faAdmins', all: 'settings.require2faAll' }[v]), value: v as string })))
// The remote MCP server (issue 70): off by default; off revokes every
// assistant's access.
const mcpOn = computed({
  get: () => form.value.mcp === '1',
  set: (v: boolean) => { form.value.mcp = v ? '1' : '' }
})
const vulnCheck = computed({
  get: () => form.value.vulnerability_check !== '0',
  set: (v: boolean) => { form.value.vulnerability_check = v ? '' : '0' }
})
const malwareScan = computed({
  get: () => form.value.malware_scan === '1',
  set: (v: boolean) => { form.value.malware_scan = v ? '1' : '' }
})
const updateCheck = computed({
  get: () => form.value.update_check !== '0',
  set: (v: boolean) => { form.value.update_check = v ? '' : '0' }
})
/** The release channel the update check and the self-update follow. */
const updateChannel = computed({
  get: () => form.value.update_channel || 'stable',
  set: (v: string) => { form.value.update_channel = v }
})
const channels = computed(() => [
  { label: t('settings.updateChannelStable'), value: 'stable' },
  { label: t('settings.updateChannelBeta'), value: 'beta' }
])

// The mail relay (#77): sites' mail() and the alerts' email go through it.
const MAIL_KEYS = ['mail_host', 'mail_port', 'mail_security', 'mail_username', 'mail_password', 'mail_from', 'alert_emails']
const mailSecurity = computed({
  get: () => form.value.mail_security || 'starttls',
  set: (v: string) => { form.value.mail_security = v }
})
const securityItems = computed(() => (['starttls', 'tls', 'none']).map(v => ({ value: v, label: t(`mail.security.${v}`) })))
const testTo = ref('')
async function testMail() {
  await run(() => api.post('/api/settings/mail/test', { to: testTo.value }), t('mail.testSent'))
}

// The page's index (PageToc), in the order of the sections.
const toc = computed(() => [
  { id: 'backups', label: t('settings.backups') },
  { id: 'panel-domain', label: t('settings.panelDomain') },
  { id: 'certificates', label: t('settings.certificates') },
  { id: 'dbservers', label: t('dbservers.title') },
  { id: 'mail', label: t('mail.title') },
  { id: 'notifications', label: t('settings.notifications') },
  { id: 'access', label: t('settings.access') },
  { id: 'updates', label: t('settings.updates') }
])

async function testRepo() {
  await run(async () => {
    const r = await api.post<{ snapshots: number }>('/api/backups/test')
    toast.add({ title: t('settings.repoOk', { n: fmtNumber(r.snapshots) }), color: 'success' })
  })
}
async function panelCert() {
  if (await run(() => api.post('/api/panel/certificate', { domain: panelDomain.value, email: form.value.acme_email }), t('settings.panelCertStarted'))) await load()
}
async function selfUpdate() {
  if (await confirm({ title: t('settings.updateTitle'), description: t('settings.updateHint'), confirmLabel: t('dashboard.updateNow') })) {
    await run(() => api.post('/api/panel/update', {}), t('dashboard.updateStarted'))
  }
}
</script>

<template>
  <UDashboardPanel id="settings">
    <template #header>
      <PageNavbar :title="t('nav.settings')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.settings')" :description="t('ux.pages.settings')" />
        <div class="grid gap-6 lg:grid-cols-[minmax(0,1fr)_13rem] lg:items-start">
          <PageToc :items="toc" class="lg:order-last" />
          <div class="flex min-w-0 flex-col gap-6">

            <PanelSection id="backups" :title="t('settings.backups')" :description="t('settings.backupsHint')" icon="i-cg-backup">
              <div class="flex flex-col gap-5">
                <UFormField :label="t('ux.backup.destination')" :help="t(`ux.backup.destHelp.${dest.kind}`)">
                  <USelect :model-value="dest.kind" :items="kindItems" class="w-full sm:w-96" :disabled="!isAdmin" @update:model-value="v => onKind(v as DestinationKind)" />
                </UFormField>

                <UAlert v-if="dest.kind === 'local'" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('ux.backup.localWarning')" />

                <div v-if="dest.kind === 'local'" class="grid gap-4">
                  <UFormField :label="t('ux.backup.folder')" :help="t('ux.backup.folderHelp')" required>
                    <UInput v-model="dest.path" class="w-full font-mono" placeholder="/var/backups/cloudground" :disabled="!isAdmin" />
                  </UFormField>
                </div>

                <UFormField v-else-if="dest.kind === 'advanced'" :label="t('settings.repository')" :help="t('ux.backup.advancedHelp')">
                  <UInput v-model="form.backup_repository" class="w-full font-mono" placeholder="sftp:user@host:/srv/restic" :disabled="!isAdmin" />
                </UFormField>

                <div v-else class="grid gap-4 sm:grid-cols-2">
                  <UFormField v-if="preset?.field === 'account'" :label="t('ux.backup.account')" :help="t('ux.backup.accountHelp')" required>
                    <UInput v-model="dest.account" class="w-full font-mono" :placeholder="preset.example" autocomplete="off" :disabled="!isAdmin" />
                  </UFormField>
                  <UFormField v-else-if="preset" :label="t('settings.region')" :help="t('ux.backup.regionHelp', { example: preset.example })" required>
                    <UInput v-model="dest.region" class="w-full font-mono" :placeholder="preset.example" autocomplete="off" :disabled="!isAdmin" />
                  </UFormField>
                  <template v-else>
                    <UFormField :label="t('ux.backup.endpoint')" :help="t('ux.backup.endpointHelp')" required>
                      <UInput v-model="dest.endpoint" class="w-full font-mono" placeholder="https://minio.example.com" autocomplete="off" :disabled="!isAdmin" />
                    </UFormField>
                    <UFormField :label="t('settings.region')" :hint="t('common.optional')" :help="t('ux.backup.regionOther')">
                      <UInput v-model="dest.region" class="w-full font-mono" placeholder="us-east-1" autocomplete="off" :disabled="!isAdmin" />
                    </UFormField>
                  </template>
                  <UFormField :label="t('ux.backup.bucket')" :help="t('ux.backup.bucketHelp')" required>
                    <UInput v-model="dest.bucket" class="w-full font-mono" placeholder="my-backups" autocomplete="off" :disabled="!isAdmin" />
                  </UFormField>
                  <UFormField :label="t('ux.backup.prefix')" :hint="t('common.optional')" :help="t('ux.backup.prefixHelp')">
                    <UInput v-model="dest.prefix" class="w-full font-mono" placeholder="cloudground" autocomplete="off" :disabled="!isAdmin" />
                  </UFormField>
                </div>

                <div v-if="isB2" class="grid gap-4 sm:grid-cols-2">
                  <UFormField :label="t('ux.backup.b2Id')" :help="t('ux.backup.b2Help')">
                    <UInput v-model="form.backup_b2_account_id" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                  </UFormField>
                  <UFormField :label="t('ux.backup.b2Key')">
                    <UInput v-model="form.backup_b2_account_key" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                  </UFormField>
                </div>

                <div v-if="isSFTP" class="flex flex-col gap-2">
                  <p class="text-sm text-highlighted">
                    {{ t('ux.backup.sshTitle') }}
                  </p>
                  <p class="text-xs text-muted">
                    {{ t('ux.backup.sshHint') }}
                  </p>
                  <div v-if="sshKey" class="flex min-w-0 items-center gap-2">
                    <code class="mono min-w-0 flex-1 text-xs [overflow-wrap:anywhere] text-highlighted">{{ sshKey }}</code>
                    <CopyButton :value="sshKey" />
                  </div>
                  <UButton v-if="isAdmin" class="self-start" color="neutral" variant="outline" icon="i-cg-key" :label="sshKey ? t('ux.backup.sshNew') : t('ux.backup.sshMake')" @click="makeSSHKey" />
                </div>

                <div v-if="s3 || (dest.kind === 'advanced' && !isSFTP && !isB2)" class="grid gap-4 sm:grid-cols-2">
                  <UFormField :label="t('ux.backup.accessKey')" :help="t('ux.backup.keysHelp')">
                    <UInput v-model="form.backup_s3_access_key" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                  </UFormField>
                  <UFormField :label="t('ux.backup.secretKey')">
                    <UInput v-model="form.backup_s3_secret_key" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                  </UFormField>
                  <UFormField v-if="dest.kind === 'advanced'" :label="t('settings.region')" :hint="t('common.optional')">
                    <UInput v-model="form.backup_s3_region" placeholder="auto" class="w-full font-mono" :disabled="!isAdmin" />
                  </UFormField>
                </div>

                <USeparator />

                <UFormField :label="t('settings.repoPassword')" :help="hasPassword && !generated ? t('ux.backup.passwordSet') : t('settings.repoPasswordHint')">
                  <UFieldGroup class="w-full">
                    <UInput v-model="form.backup_password" :type="generated ? 'text' : 'password'" autocomplete="off" class="min-w-0 flex-1 font-mono" :disabled="!isAdmin" />
                    <UButton v-if="isAdmin && !hasPassword" color="neutral" variant="outline" icon="i-cg-key" :label="t('ux.backup.generate')" @click="generatePassword" />
                  </UFieldGroup>
                </UFormField>
                <UAlert
                  v-if="generated"
                  color="warning"
                  variant="subtle"
                  icon="i-cg-alert-triangle"
                  :title="t('ux.backup.keepTitle')"
                  :description="t('ux.backup.keepPassword')"
                  :actions="[
                    { label: t('common.copy'), icon: 'i-cg-copy', color: 'neutral', variant: 'outline', size: 'sm', onClick: () => copyText(generated) },
                    { label: t('ux.backup.download'), icon: 'i-cg-download', color: 'neutral', variant: 'outline', size: 'sm', onClick: downloadPassword }
                  ]"
                />

                <UFormField v-if="dest.kind !== 'advanced'" :label="t('ux.backup.result')" :help="t('ux.backup.resultHelp')">
                  <code class="mono block w-full bg-elevated px-3 py-2.5 text-[13px] [overflow-wrap:anywhere] text-highlighted">{{ repository || '—' }}</code>
                </UFormField>
              </div>
              <template v-if="isAdmin" #footer>
                <UButton :label="t('common.save')" :loading="busy" :disabled="!backupDirty" @click="save(BACKUP_KEYS)" />
                <UButton color="neutral" variant="outline" icon="i-cg-signal" :label="t('settings.testRepo')" :disabled="busy || backupDirty || !saved.backup_repository" @click="testRepo" />
                <span class="text-[13px] text-muted">{{ backupDirty ? t('ux.backup.saveFirst') : t('ux.backup.testHelp') }}</span>
              </template>
            </PanelSection>

            <PanelSection id="panel-domain" :title="t('settings.panelDomain')" :description="t('settings.panelDomainHint')" icon="i-cg-sites">
              <UAlert v-if="saved.panel_domain" color="success" variant="subtle" icon="i-cg-check-circle" :description="t('settings.attached', { domain: saved.panel_domain })" />
              <UFormField :label="t('site.domain')" :help="!saved.acme_email ? t('settings.needAcme') : undefined">
                <UInput v-model="panelDomain" placeholder="panel.example.com" class="w-full sm:w-96" :disabled="!isAdmin" />
              </UFormField>
              <template v-if="isAdmin" #footer>
                <!-- The full label does not fit a phone: a shorter one there. -->
                <UButton icon="i-cg-lock" :aria-label="saved.panel_domain ? t('settings.movePanel') : t('settings.securePanel')" :disabled="!panelDomain || !saved.acme_email || panelDomain === saved.panel_domain" :loading="busy" @click="panelCert">
                  <span class="sm:hidden">{{ saved.panel_domain ? t('ux.settings.moveShort') : t('ux.settings.secureShort') }}</span>
                  <span class="hidden sm:inline">{{ saved.panel_domain ? t('settings.movePanel') : t('settings.securePanel') }}</span>
                </UButton>
              </template>
            </PanelSection>

            <PanelSection id="certificates" :title="t('settings.certificates')" icon="i-cg-certificate">
              <div class="grid gap-4 sm:grid-cols-2">
                <UFormField :label="t('settings.acmeEmail')" :help="t('settings.acmeHint')">
                  <UInput v-model="form.acme_email" type="email" placeholder="ops@example.com" class="w-full" :disabled="!isAdmin" />
                </UFormField>
                <UFormField :label="t('settings.cloudflare')" :help="t('settings.cloudflareHint')">
                  <UInput v-model="form.cloudflare_api_token" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
                <UFormField :label="t('settings.cloudflarePurge')" :help="t('settings.cloudflarePurgeHint')">
                  <UInput v-model="form.cloudflare_purge_token" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
              </div>
              <template v-if="isAdmin" #footer>
                <UButton :label="t('common.save')" :loading="busy" @click="save(['acme_email', 'cloudflare_api_token', 'cloudflare_purge_token'])" />
              </template>
            </PanelSection>

            <DatabaseServers />

            <PanelSection id="mail" :title="t('mail.title')" :description="t('mail.serverHint')" icon="i-cg-bell">
              <div class="grid gap-4 sm:grid-cols-2">
                <UFormField :label="t('mail.host')">
                  <UInput v-model="form.mail_host" placeholder="smtp.example.com" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
                <div class="grid grid-cols-2 gap-4">
                  <UFormField :label="t('mail.securityLabel')">
                    <USelect v-model="mailSecurity" :items="securityItems" class="w-full" :disabled="!isAdmin" />
                  </UFormField>
                  <UFormField :label="t('mail.port')" :help="t('mail.portHelp')">
                    <UInput v-model="form.mail_port" inputmode="numeric" class="w-full font-mono" :disabled="!isAdmin" />
                  </UFormField>
                </div>
                <UFormField :label="t('mail.username')">
                  <UInput v-model="form.mail_username" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
                <UFormField :label="t('mail.password')">
                  <UInput v-model="form.mail_password" type="password" autocomplete="off" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
                <UFormField :label="t('mail.from')" :help="t('mail.fromHelp')">
                  <UInput v-model="form.mail_from" placeholder="noreply@example.com" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
                <UFormField :label="t('mail.alertEmails')" :help="t('mail.alertEmailsHelp')">
                  <UInput v-model="form.alert_emails" placeholder="ops@example.com" class="w-full font-mono" :disabled="!isAdmin" />
                </UFormField>
              </div>
              <template v-if="isAdmin" #footer>
                <UButton :label="t('common.save')" :loading="busy" @click="save(MAIL_KEYS)" />
                <UInput v-model="testTo" placeholder="test@example.com" class="w-full font-mono sm:w-64" />
                <UButton color="neutral" variant="outline" :label="t('mail.test')" :disabled="busy || !testTo || !saved.mail_host" @click="testMail" />
              </template>
            </PanelSection>

            <PanelSection id="notifications" :title="t('settings.notifications')" :description="t('settings.notificationsHint')" icon="i-cg-bell">
              <UFormField :label="t('settings.webhook')" :help="t('settings.webhookHint')">
                <UInput v-model="form.notify_webhook" placeholder="https://hooks.slack.com/services/…" class="w-full font-mono" :disabled="!isAdmin" />
              </UFormField>
              <USwitch v-model="vulnCheck" :label="t('settings.vulnCheck')" :description="t('settings.vulnCheckHint')" :disabled="!isAdmin" />
              <USwitch v-model="malwareScan" :label="t('settings.malwareScan')" :description="t('settings.malwareScanHint')" :disabled="!isAdmin" />
              <template v-if="isAdmin" #footer>
                <UButton :label="t('common.save')" :loading="busy" @click="save(['notify_webhook', 'vulnerability_check', 'malware_scan'])" />
              </template>
            </PanelSection>

            <PanelSection id="access" :title="t('settings.access')" icon="i-cg-lock">
              <UFormField :label="t('settings.require2fa')" :help="t('settings.require2faHint')">
                <URadioGroup v-model="require2fa" :items="require2faItems" :disabled="!isAdmin" />
              </UFormField>
              <USwitch v-model="mcpOn" :label="t('settings.mcp')" :description="t('settings.mcpHint')" :disabled="!isAdmin" />
              <UFormField :label="t('settings.probeTarget')" :help="t('settings.probeTargetHint')">
                <UInput v-model="form.probe_target" placeholder="https://127.0.0.1" class="w-full sm:w-96 font-mono" :disabled="!isAdmin" />
              </UFormField>
              <template v-if="isAdmin" #footer>
                <UButton :label="t('common.save')" :loading="busy" @click="save(['require_2fa', 'mcp', 'probe_target'])" />
              </template>
            </PanelSection>

            <PanelSection id="updates" :title="t('settings.updates')" :description="t('settings.updatesHint')" icon="i-cg-download">
              <USwitch v-model="updateCheck" :label="t('settings.updateCheck')" :disabled="!isAdmin" />
              <UFormField :label="t('settings.updateChannel')" :help="t('settings.updateChannelHint')">
                <USelect v-model="updateChannel" :items="channels" class="w-full sm:w-80" :disabled="!isAdmin" />
              </UFormField>
              <template v-if="isAdmin" #footer>
                <UButton :label="t('common.save')" :loading="busy" @click="save(['update_check', 'update_channel'])" />
                <UButton color="neutral" variant="outline" icon="i-cg-refresh" :label="t('dashboard.updateNow')" :disabled="busy" @click="selfUpdate" />
              </template>
            </PanelSection>
          </div>
        </div>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
