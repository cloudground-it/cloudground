<script setup lang="ts">
/**
 * A site's SFTP / SSH tab (artboard PanelSiteSFTP): the chrooted SFTP login,
 * by SSH keys only or with a password (#47), the SSH keys with their type,
 * and the SSH shell an administrator can turn on for the "shell" keys (R-4.5).
 */
type Access = 'sftp' | 'shell'
type SignIn = 'keys' | 'password'
interface SSHKey { type: string, fingerprint: string, label?: string, access?: Access }

const { t } = useI18n()
const api = useApi()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site, refresh } = useCurrentSite()

const password = ref('')
const publicKey = ref('')
const keyAccess = ref<Access>('sftp')
const sshPort = ref(22)
const shellUser = ref('')
const { data: keys, refresh: refreshKeys } = useLoad(async () => {
  const res = await api.get<{ keys: SSHKey[] | null, port?: number, shell_user?: string }>(`/api/sites/${site.value.id}/ssh-keys`)
  if (res.port) sshPort.value = res.port
  if (res.shell_user) shellUser.value = res.shell_user
  return res.keys ?? []
})

const sftpOn = computed(() => site.value.config.sftp_enabled)
const keysOnly = computed(() => !!site.value.config.sftp_keys_only)
// How the login signs in: what it is now, else keys only, the safer choice.
const signIn = ref<SignIn>('keys')
watch([sftpOn, keysOnly], ([on, only]) => { signIn.value = on && !only ? 'password' : 'keys' }, { immediate: true })
const signInItems = computed(() => [
  { value: 'keys' as const, label: t('sftp.modeKeys'), description: t('sftp.modeKeysHint') },
  { value: 'password' as const, label: t('sftp.modePassword'), description: t('sftp.modePasswordHint') }
])
// With no key on the site yet, keys only takes the first one here.
const firstKey = ref('')
const needsKey = computed(() => signIn.value === 'keys' && keys.value !== undefined && !(keys.value ?? []).length)
const submitLabel = computed(() => {
  if (signIn.value === 'keys') return sftpOn.value ? t('sftp.switchKeys') : t('sftp.enableKeys')
  return !sftpOn.value ? t('sftp.enable') : keysOnly.value ? t('sftp.switchPassword') : t('sftp.update')
})
// Nothing to send when the login already takes keys only.
const showSubmit = computed(() => !(signIn.value === 'keys' && sftpOn.value && keysOnly.value))
const canSubmit = computed(() => signIn.value === 'keys'
  ? !needsKey.value || !!firstKey.value.trim()
  : password.value.length >= 12)
const shellOn = computed(() => !!site.value.config.shell_enabled)
// The server's address as the browser reached the panel: the same machine.
const host = computed(() => typeof window !== 'undefined' ? window.location.hostname : site.value.domain)
const portFlag = (flag: string) => sshPort.value === 22 ? '' : `${flag} ${sshPort.value} `
const sftpCommand = computed(() => `sftp ${portFlag('-P')}${site.value.system_user}@${host.value}`)
const shellCommand = computed(() => `ssh ${portFlag('-p')}${shellUser.value}@${host.value}`)

const rows = computed(() => {
  const out: { k: string, v: string, copy?: boolean }[] = [
    { k: 'Host', v: host.value, copy: true },
    { k: t('sftp.port'), v: String(sshPort.value) },
    { k: t('sftp.username'), v: site.value.system_user, copy: true },
    { k: t('sftp.folder'), v: `/srv/sites/${site.value.domain}` },
    { k: t('sftp.command'), v: sftpCommand.value, copy: true }
  ]
  if (shellOn.value && shellUser.value) {
    out.push({ k: t('sftp.shellLogin'), v: shellUser.value, copy: true }, { k: t('sftp.shellCommand'), v: shellCommand.value, copy: true })
  }
  return out
})

async function enable() {
  const byKeys = signIn.value === 'keys'
  const body = byKeys
    ? { enable: true, keys_only: true, ...(firstKey.value.trim() ? { public_key: firstKey.value.trim() } : {}) }
    : { enable: true, keys_only: false, password: password.value }
  if (await run(() => api.post(`/api/sites/${site.value.id}/sftp`, body), byKeys ? t('sftp.keysOnlyOn') : t('sftp.enabled'))) {
    password.value = ''
    firstKey.value = ''
    await refresh()
    await refreshKeys()
  }
}
const disable = () => run(() => api.post(`/api/sites/${site.value.id}/sftp`, { enable: false }), t('sftp.disabled')).then(refresh)

async function addKey() {
  if (await run(() => api.post(`/api/sites/${site.value.id}/ssh-keys`, { public_key: publicKey.value, access: keyAccess.value }), t('sftp.keyAdded'))) {
    publicKey.value = ''
    addingKey.value = false
    await refreshKeys()
  }
}
async function removeKey(k: SSHKey) {
  const name = k.label || k.fingerprint
  if (await confirm({ title: t('sftp.removeKey', { key: name }), danger: true, confirmLabel: t('common.remove') })) {
    await run(() => api.del(`/api/sites/${site.value.id}/ssh-keys/${encodeURIComponent(k.fingerprint)}`), t('sftp.keyRemoved'))
    await refreshKeys()
  }
}

async function setShell(on: boolean) {
  // Name the keys that will get a shell, so the switch is never a surprise.
  const shellKeys = (keys.value ?? []).filter(k => k.access === 'shell').map(k => k.label || k.fingerprint)
  const which = shellKeys.length ? t('sftp.confirmShellKeys', { keys: shellKeys.join(', ') }) : t('sftp.confirmShellNoKeys')
  const ok = await confirm(on
    ? { title: t('sftp.confirmShellOn', { site: site.value.domain }), description: `${t('sftp.shellWarning')} ${which}`, confirmLabel: t('sftp.shellToggle'), danger: true }
    : { title: t('sftp.confirmShellOff'), confirmLabel: t('sftp.disable') })
  if (!ok) return
  if (await run(() => api.post(`/api/sites/${site.value.id}/ssh-shell`, { enable: on }), on ? t('sftp.shellOn') : t('sftp.shellOff'))) {
    await refresh()
    await refreshKeys()
  }
}

// A shell key is an administrator's to give, as the shell is.
const keyTypes = computed(() => [
  { value: 'sftp' as const, label: t('sftp.typeSftp'), disabled: false },
  { value: 'shell' as const, label: t('sftp.typeShell'), disabled: !isAdmin.value }
])
function keyBadge(k: SSHKey) {
  if (k.access !== 'shell') return { label: t('sftp.typeSftp'), color: 'neutral' as const }
  return shellOn.value
    ? { label: t('sftp.typeShell'), color: 'info' as const }
    : { label: t('sftp.typeShellOff'), color: 'warning' as const }
}
// Adding a key happens in a dialog opened from the keys' section.
const addingKey = ref(false)
const keyColumns = computed(() => [
  { id: 'key', header: t('sftp.publicKey') },
  { id: 'type', header: t('sftp.keyType') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <PanelSection :title="t('sftp.title')" :description="t('sftp.hint')">
    <template #actions>
      <UBadge :color="sftpOn ? 'success' : 'neutral'" variant="subtle" size="lg" :label="sftpOn ? (keysOnly ? t('sftp.onKeys') : t('sftp.on')) : t('sftp.off')" />
    </template>
    <div class="grid items-start gap-6 lg:grid-cols-2">
      <div class="flex min-w-0 flex-col gap-2">
        <h3 class="hud m-0 text-muted">
          {{ t('sftp.connection') }}
        </h3>
        <SummaryList :rows="rows.map(r => ({ k: r.k, v: r.v, mono: true, copy: r.copy }))" />
      </div>
      <div v-if="canManage" class="flex min-w-0 flex-col gap-3">
        <URadioGroup
          v-model="signIn"
          :legend="t('sftp.signIn')"
          :items="signInItems"
          variant="card"
          :ui="{ legend: 'hud mb-2 text-muted' }"
        />
        <UFormField v-if="needsKey" :label="t('sftp.publicKey')" :help="t('sftp.firstKeyHint')">
          <UTextarea v-model="firstKey" :rows="2" autoresize class="w-full" :ui="{ base: 'font-mono text-[13px]' }" placeholder="ssh-ed25519 AAAA… developer@laptop" />
        </UFormField>
        <UFormField v-if="signIn === 'password'" :label="t('sftp.password')" :hint="t('auth.min12')" :help="sftpOn ? undefined : t('sftp.offHint')">
          <UInput v-model="password" type="password" autocomplete="new-password" class="w-full font-mono" />
        </UFormField>
        <div class="flex flex-wrap gap-2">
          <UButton v-if="showSubmit" :label="submitLabel" :disabled="!canSubmit" :loading="busy" @click="enable" />
          <UButton v-if="sftpOn" color="error" variant="outline" :label="t('sftp.disable')" :disabled="busy" @click="disable" />
        </div>
      </div>
      <p v-else-if="!sftpOn" class="m-0 text-[13px] text-muted">
        {{ t('sftp.offHint') }}
      </p>
    </div>
  </PanelSection>

  <PanelSection :title="t('sftp.keys')" :description="t('sftp.keysHint')" flush>
    <template v-if="canManage" #actions>
      <UButton icon="i-cg-plus" :label="t('sftp.addKey')" @click="addingKey = true" />
    </template>
    <UEmpty
      v-if="keys && !keys.length"
      icon="i-cg-key"
      :description="t('sftp.noKeys')"
      :actions="canManage ? [{ label: t('sftp.addKey'), icon: 'i-cg-plus', onClick: () => { addingKey = true } }] : []"
    />
    <UTable v-else-if="keys" :data="keys" :columns="keyColumns">
      <template #key-cell="{ row: { original: k } }">
        <span class="flex min-w-0 flex-col gap-0.5 whitespace-normal">
          <span class="font-semibold text-highlighted">{{ k.label || k.type }}</span>
          <span class="mono text-xs [overflow-wrap:anywhere] text-muted">{{ k.fingerprint }}</span>
        </span>
      </template>
      <template #type-cell="{ row: { original: k } }">
        <UBadge :color="keyBadge(k).color" variant="subtle" :label="keyBadge(k).label" />
      </template>
      <template #actions-cell="{ row: { original: k } }">
        <UButton v-if="canManage" size="sm" color="error" variant="ghost" square icon="i-cg-trash" :aria-label="t('sftp.removeKey', { key: k.label || k.fingerprint })" @click="removeKey(k)" />
      </template>
    </UTable>
  </PanelSection>

  <PanelSection :title="t('sftp.shellTitle')" :description="t('sftp.shellHint')">
    <template #actions>
      <UBadge :color="shellOn ? 'success' : 'neutral'" variant="subtle" size="lg" :label="shellOn ? t('sftp.on') : t('sftp.off')" />
    </template>
    <div class="grid items-start gap-6 lg:grid-cols-2">
      <div class="flex min-w-0 flex-col gap-4">
        <USwitch
          :model-value="shellOn"
          :disabled="!isAdmin || busy"
          :label="t('sftp.shellToggle')"
          :description="isAdmin ? (sftpOn ? undefined : t('sftp.shellNeedsAccess')) : t('sftp.shellAdminOnly')"
          @update:model-value="v => setShell(!!v)"
        />
        <UAlert color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('sftp.shellWarning')" />
        <div class="mono overflow-x-auto bg-cg-panel px-3.5 py-3 text-[12.5px] leading-[1.75] whitespace-nowrap text-cg-panel-text" role="img" :aria-label="t('sftp.shellPreviewLabel')">
          <div class="hud pb-1.5 text-[#8A8A92]">
            {{ t('sftp.shellPreview') }}
          </div>
          <div><span class="text-[#7C9BFF]">{{ site.system_user }}@{{ site.domain.split('.')[0] }}</span>:<span class="text-[#5CC98A]">~/current</span>$ wp plugin list --status=active</div>
          <div class="text-[#8A8A92]">
            …
          </div>
        </div>
      </div>
      <div class="flex min-w-0 flex-col gap-2">
        <h3 class="hud m-0 text-muted">
          {{ t('sftp.shellFacts') }}
        </h3>
        <ul class="m-0 flex list-disc flex-col gap-1.5 ps-5 text-sm text-default">
          <li>{{ t('sftp.shellFactTools') }}</li>
          <li>{{ t('sftp.shellFactIsolation') }}</li>
          <li>{{ t('sftp.shellFactEnd') }}</li>
          <li>{{ t('sftp.shellFactRelease') }}</li>
        </ul>
      </div>
    </div>
  </PanelSection>

  <UModal v-model:open="addingKey" :title="t('sftp.addKey')" :description="t('sftp.keysHint')" :ui="{ content: 'sm:max-w-2xl' }">
    <template #body>
      <div class="flex flex-col gap-4">
        <UFormField :label="t('sftp.publicKey')">
          <UTextarea v-model="publicKey" :rows="3" autoresize class="w-full" :ui="{ base: 'font-mono text-[13px]' }" placeholder="ssh-ed25519 AAAA… developer@laptop" autofocus />
        </UFormField>
        <URadioGroup
          v-model="keyAccess"
          :legend="t('sftp.keyType')"
          :items="keyTypes.map(kt => ({ value: kt.value, label: kt.label, disabled: kt.disabled }))"
          orientation="horizontal"
          variant="card"
          :ui="{ legend: 'hud mb-2 text-muted' }"
        />
        <p v-if="!isAdmin" class="m-0 text-[13px] text-muted">
          {{ t('sftp.shellKeyAdminOnly') }}
        </p>
        <UAlert v-if="keyAccess === 'shell' && !shellOn" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('sftp.shellKeyPending')" />
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="addingKey = false" />
        <UButton icon="i-cg-plus" :label="t('sftp.addKey')" :disabled="!publicKey.trim()" :loading="busy" @click="addKey" />
      </div>
    </template>
  </UModal>
</template>
