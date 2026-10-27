<script setup lang="ts">
import type { Passkey } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { user, fetchMe, isAdmin, enrolRequired } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const toast = useToast()
useHead({ title: () => t('nav.security') })

const profile = reactive({ first_name: user.value?.first_name ?? '', last_name: user.value?.last_name ?? '' })
async function saveProfile() {
  if (await run(() => api.put('/api/account/profile', profile), t('security.profileSaved'))) await fetchMe()
}

const pw = reactive({ current: '', next: '', confirm: '' })
const pwReady = computed(() => pw.current && pw.next.length >= 12 && pw.next === pw.confirm)
async function changePassword() {
  if (await run(() => api.post('/api/account/password', { current_password: pw.current, new_password: pw.next }), t('security.passwordChanged'))) {
    Object.assign(pw, { current: '', next: '', confirm: '' })
  }
}

const proof = useProof()
const enrol = reactive({ confirmCode: '' })
const pending = ref<{ secret: string, qr_data: string } | null>(null)
/** Recovery codes just made: shown once, here. */
const freshCodes = ref<string[] | null>(null)

async function refreshAll() {
  await fetchMe()
  await refreshPasskeys()
  await refreshSessions()
}

async function begin() {
  const p = await proof({ title: user.value?.totp_enabled ? t('security.replace') : t('security.setup') })
  if (!p) return
  await run(async () => {
    pending.value = await api.post('/api/account/2fa/begin', p)
  })
}
async function confirmTotp() {
  if (await run(async () => {
    const r = await api.post<{ recovery_codes?: string[] }>('/api/account/2fa/confirm', { code: enrol.confirmCode })
    if (r.recovery_codes) freshCodes.value = r.recovery_codes
  }, t('security.totpEnabled'))) {
    pending.value = null
    enrol.confirmCode = ''
    await refreshAll()
    await refreshTokens()
  }
}
async function disable() {
  if (!await confirm({ title: t('security.disableTitle'), description: t('security.disableHint'), danger: true, confirmLabel: t('security.disable') })) return
  const p = await proof({ title: t('security.disableTitle'), danger: true, confirmLabel: t('security.disable') })
  if (!p) return
  if (await run(() => api.post('/api/account/2fa/disable', p), t('security.totpDisabled'))) await refreshAll()
}

// Passkeys (issue 156): a second factor, or the whole sign-in.
const passkeyHere = ref(false)
onMounted(() => {
  const host = window.location.hostname
  passkeyHere.value = passkeysSupported() && !/^[\d.]+$/.test(host) && !host.includes(':')
})
const { data: passkeys, refresh: refreshPasskeys } = useLoad(() => api.get<Passkey[]>('/api/account/passkeys'))
const newPasskeyName = ref('')
onMounted(() => { newPasskeyName.value = suggestedPasskeyName() })
const addingPasskey = ref(false)
async function addPasskey() {
  const name = newPasskeyName.value.trim()
  if (!name) return
  const p = await proof({ title: t('security.passkeyAdd') })
  if (!p) return
  addingPasskey.value = true
  try {
    const options = await api.post<Record<string, unknown>>('/api/account/passkeys/begin', p)
    const credential = await createPasskey(options)
    const r = await api.post<{ recovery_codes?: string[] }>('/api/account/passkeys/finish', { name, credential })
    if (r.recovery_codes) freshCodes.value = r.recovery_codes
    toast.add({ title: t('security.passkeyAdded'), color: 'success', icon: 'i-cg-check-circle' })
    await refreshAll()
    await refreshTokens()
  } catch (e) {
    if (!passkeyCancelled(e)) toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    addingPasskey.value = false
  }
}
async function removePasskey(pk: Passkey) {
  if (!await confirm({ title: t('security.passkeyRemoveTitle', { name: pk.name }), description: t('security.passkeyRemoveHint'), danger: true, confirmLabel: t('security.remove') })) return
  const p = await proof({ title: t('security.passkeyRemoveTitle', { name: pk.name }), danger: true, confirmLabel: t('security.remove') })
  if (!p) return
  if (await run(() => api.post(`/api/account/passkeys/${pk.id}/remove`, p), t('security.passkeyRemoved'))) await refreshAll()
}

// Password sign-in: off leaves the passkeys as the only way in.
async function setPasswordSignIn(enabled: boolean) {
  const p = await proof({
    title: enabled ? t('security.passwordSignInOnTitle') : t('security.passwordSignInOffTitle'),
    description: enabled ? undefined : t('security.passwordSignInOffHint'),
    passkeyOnly: !enabled
  })
  if (!p) return
  if (await run(() => api.put('/api/account/password-signin', { enabled, ...p }), enabled ? t('security.passwordSignInOn') : t('security.passwordSignInOff'))) await refreshAll()
}

// Recovery codes: made with the first factor, and again on demand.
async function newCodes() {
  const p = await proof({ title: t('security.codesNew') })
  if (!p) return
  await run(async () => {
    const r = await api.post<{ recovery_codes: string[] }>('/api/account/recovery-codes', p)
    freshCodes.value = r.recovery_codes
  }, t('security.codesMade'))
  await fetchMe()
}
function downloadCodes() {
  if (!freshCodes.value) return
  const text = `CloudGround — ${user.value?.email ?? ''}\n${t('security.codesFile')}\n\n${freshCodes.value.join('\n')}\n`
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
  const a = document.createElement('a')
  a.href = url
  a.download = 'cloudground-recovery-codes.txt'
  a.click()
  URL.revokeObjectURL(url)
}
const twoFactorOn = computed(() => !!user.value && (user.value.totp_enabled || user.value.passkeys > 0))

const { data: sessions, refresh: refreshSessions } = useLoad(() => api.get<{ id: string, current: boolean, created_at: string, expires_at: string, last_seen: string, browser: string, os: string, ip: string }[]>('/api/account/sessions'))
function device(s: { browser: string, os: string }) {
  if (s.browser && s.os) return t('security.on', { browser: s.browser, os: s.os })
  return s.browser || s.os || t('security.unknownDevice')
}
async function revoke(id: string) {
  await run(() => api.del(`/api/account/sessions/${id}`), t('security.revoked'))
  await refreshSessions()
}

// API tokens for cgctl and scripts: an administrator's, from this session
// only. The secret is shown once, when it is made.
interface APIToken { id: number, email: string, name: string, hint: string, created_at: string, expires_at?: string, last_used_at?: string }
const tokens = ref<APIToken[] | null>(null)
async function refreshTokens() {
  if (!isAdmin.value || enrolRequired.value) return
  try {
    tokens.value = await api.get<APIToken[]>('/api/tokens')
  } catch {
    tokens.value = []
  }
}
onMounted(refreshTokens)
const newToken = reactive({ name: '', expires: 90 })
const expiryItems = computed(() => [30, 90, 365, 0].map(d => ({ label: d ? t('security.tokenDays', { n: fmtNumber(d) }) : t('security.tokenNever'), value: d })))
const createdSecret = ref<string | null>(null)
async function createToken() {
  await run(async () => {
    const r = await api.post<{ token: string }>('/api/tokens', { name: newToken.name.trim(), expires_in_days: newToken.expires })
    createdSecret.value = r.token
    newToken.name = ''
  }, t('security.tokenCreated'))
  await refreshTokens()
}
async function revokeToken(tok: APIToken) {
  if (await confirm({ title: t('security.tokenRevokeTitle', { name: tok.name }), description: t('security.tokenRevokeHint'), danger: true, confirmLabel: t('security.revoke') })) {
    await run(() => api.del(`/api/tokens/${tok.id}`), t('security.tokenRevoked'))
    await refreshTokens()
  }
}
// The page's index (PageToc): the sections this person sees.
const toc = computed(() => [
  ...(enrolRequired.value ? [] : [{ id: 'profile', label: t('security.profile') }, { id: 'password', label: t('security.password') }]),
  { id: 'two-step', label: t('security.totp') },
  { id: 'passkeys', label: t('security.passkeys') },
  ...(twoFactorOn.value ? [{ id: 'recovery-codes', label: t('security.codes') }] : []),
  { id: 'sessions', label: t('security.sessions') },
  ...(isAdmin.value && !enrolRequired.value ? [{ id: 'api-tokens', label: t('security.tokens') }] : [])
])
</script>

<template>
  <UDashboardPanel id="security">
    <template #header>
      <PageNavbar :title="t('nav.security')" />
    </template>
    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('nav.security')" :description="t('ux.pages.security', { email: user?.email ?? '' })" />
        <div class="grid gap-6 lg:grid-cols-[minmax(0,1fr)_13rem] lg:items-start">
          <PageToc :items="toc" class="lg:order-last" />
          <div class="flex min-w-0 flex-col gap-6">
            <UAlert v-if="enrolRequired" color="warning" variant="subtle" icon="i-cg-alert-circle" :title="t('security.enrolRequired')" :description="t('security.enrolRequiredHint')" />
            <PanelSection id="profile" v-if="!enrolRequired" :title="t('security.profile')" :description="t('security.profileHint')">
              <UForm :state="profile" class="space-y-4" @submit="saveProfile">
                <div class="grid gap-4 sm:grid-cols-2">
                  <UFormField :label="t('users.firstName')">
                    <UInput v-model="profile.first_name" class="w-full" autocomplete="given-name" />
                  </UFormField>
                  <UFormField :label="t('users.lastName')">
                    <UInput v-model="profile.last_name" class="w-full" autocomplete="family-name" />
                  </UFormField>
                </div>
                <UButton type="submit" :label="t('common.save')" :loading="busy" />
              </UForm>
            </PanelSection>

            <PanelSection id="password" v-if="!enrolRequired" :title="t('security.password')" :description="t('security.passwordHint')">
              <UForm :state="pw" class="space-y-4" @submit="changePassword">
                <UFormField :label="t('security.current')">
                  <UInput v-model="pw.current" type="password" autocomplete="current-password" class="w-full" />
                </UFormField>
                <div class="grid gap-4 sm:grid-cols-2">
                  <UFormField :label="t('security.new')" :hint="t('auth.min12')" :error="pw.next && pw.next.length < 12 ? t('auth.passwordLength') : undefined">
                    <UInput v-model="pw.next" type="password" autocomplete="new-password" class="w-full" />
                  </UFormField>
                  <UFormField :label="t('auth.confirmPassword')" :error="pw.confirm && pw.next !== pw.confirm ? t('auth.passwordMismatch') : undefined">
                    <UInput v-model="pw.confirm" type="password" autocomplete="new-password" class="w-full" />
                  </UFormField>
                </div>
                <UButton type="submit" :label="t('security.updatePassword')" :disabled="!pwReady" :loading="busy" />
              </UForm>
            </PanelSection>

            <UAlert v-if="freshCodes" color="success" variant="subtle" icon="i-cg-key" :title="t('security.codesOnce')">
              <template #description>
                <div class="mt-1 flex w-full flex-col gap-3">
                  <p>{{ t('security.codesOnceHint') }}</p>
                  <ul class="m-0 grid list-none grid-cols-2 gap-x-6 gap-y-1 p-0 font-mono text-sm text-highlighted sm:grid-cols-2">
                    <li v-for="c in freshCodes" :key="c">
                      {{ c }}
                    </li>
                  </ul>
                  <div class="flex flex-wrap items-center gap-2">
                    <CopyButton :value="freshCodes.join('\n')" />
                    <UButton size="xs" color="neutral" variant="outline" icon="i-cg-download" :label="t('security.codesDownload')" @click="downloadCodes" />
                    <UButton size="xs" color="neutral" variant="outline" :label="t('security.codesStored')" @click="freshCodes = null" />
                  </div>
                </div>
              </template>
            </UAlert>

            <PanelSection id="two-step" :title="t('security.totp')" :description="t('security.totpHint')">
              <template #actions>
                <UBadge :color="user?.totp_enabled ? 'success' : 'warning'" variant="subtle" size="lg" :label="user?.totp_enabled ? t('common.on') : t('common.off')" />
              </template>

              <template v-if="pending">
                <p class="text-sm text-muted">
                  {{ t('security.scan') }}
                </p>
                <div class="flex flex-col items-center gap-2">
                  <img :src="pending.qr_data" :alt="t('security.qr')" class="size-48 bg-(--cg-qr-ground) p-2">
                  <code class="font-mono text-xs break-all text-muted">{{ pending.secret }}</code>
                </div>
                <div class="flex items-end gap-2">
                  <UFormField :label="t('auth.code')" class="flex-1">
                    <UInput v-model="enrol.confirmCode" inputmode="numeric" maxlength="6" placeholder="000000" autocomplete="one-time-code" class="w-full font-mono" />
                  </UFormField>
                  <UButton :label="t('security.verifyEnable')" :disabled="enrol.confirmCode.length !== 6" :loading="busy" @click="confirmTotp" />
                </div>
              </template>

              <div v-else class="flex flex-wrap gap-2">
                <UButton :label="user?.totp_enabled ? t('security.replace') : t('security.setup')" :loading="busy" @click="begin" />
                <UButton v-if="user?.totp_enabled" color="error" variant="outline" :label="t('security.disable')" @click="disable" />
              </div>
            </PanelSection>

            <PanelSection id="passkeys" :title="t('security.passkeys')" :description="t('security.passkeysHint')">
              <template #actions>
                <UBadge :color="(user?.passkeys ?? 0) > 0 ? 'success' : 'neutral'" variant="subtle" size="lg" :label="String(user?.passkeys ?? 0)" />
              </template>
              <UAlert v-if="!passkeyHere" color="info" variant="subtle" icon="i-cg-info" :description="t('security.passkeysElsewhere')" />
              <ul v-if="passkeys?.length" class="m-0 list-none divide-y divide-default border-y border-default p-0">
                <li v-for="pk in passkeys" :key="pk.id" class="flex items-center gap-3 py-2.5">
                  <UIcon name="i-cg-key" class="size-5 shrink-0 text-muted" />
                  <div class="min-w-0 flex-1">
                    <p class="flex flex-wrap items-center gap-2 text-sm text-highlighted">
                      {{ pk.name }}
                      <UBadge v-if="pk.backed_up" size="sm" color="neutral" variant="subtle" :label="t('security.passkeySynced')" />
                    </p>
                    <p class="text-xs text-muted">
                      {{ pk.rp_id }} · {{ t('security.tokenMade', { when: fmtAgo(pk.created_at, locale) }) }} · {{ pk.last_used_at ? t('security.lastSeen', { when: fmtAgo(pk.last_used_at, locale) }) : t('security.tokenUnused') }}
                    </p>
                  </div>
                  <UButton size="sm" color="error" variant="outline" :label="t('security.remove')" :disabled="!user?.password_signin && passkeys.length === 1" @click="removePasskey(pk)" />
                </li>
              </ul>
              <form v-if="passkeyHere" class="flex flex-wrap items-end gap-3" @submit.prevent="addPasskey">
                <UFormField :label="t('security.passkeyName')" class="min-w-48 flex-[1_1_240px]">
                  <UInput v-model="newPasskeyName" maxlength="64" class="w-full" autocomplete="off" />
                </UFormField>
                <UButton type="submit" icon="i-cg-plus" :label="t('security.passkeyAdd')" :disabled="!newPasskeyName.trim()" :loading="addingPasskey" />
              </form>
              <template v-if="(user?.passkeys ?? 0) > 0 && !enrolRequired">
                <USeparator />
                <div class="flex flex-wrap items-center justify-between gap-3">
                  <div class="min-w-0 flex-1">
                    <p class="text-sm text-highlighted">
                      {{ t('security.passwordSignIn') }}
                    </p>
                    <p class="text-xs text-muted">
                      {{ user?.password_signin ? t('security.passwordSignInOnHint') : t('security.passwordSignInOffNow') }}
                    </p>
                  </div>
                  <UButton :color="user?.password_signin ? 'error' : 'primary'" variant="outline" :label="user?.password_signin ? t('security.passwordSignInTurnOff') : t('security.passwordSignInTurnOn')" @click="setPasswordSignIn(!user?.password_signin)" />
                </div>
              </template>
            </PanelSection>

            <PanelSection v-if="twoFactorOn" id="recovery-codes" :title="t('security.codes')" :description="t('security.codesHint')">
              <template #actions>
                <UBadge :color="(user?.recovery_codes_left ?? 0) > 2 ? 'success' : 'warning'" variant="subtle" size="lg" :label="t('security.codesLeft', { n: fmtNumber(user?.recovery_codes_left ?? 0) })" />
              </template>
              <UButton class="self-start" :label="t('security.codesNew')" :loading="busy" @click="newCodes" />
            </PanelSection>

            <PanelSection id="sessions" :title="t('security.sessions')" flush>
              <ul class="m-0 list-none divide-y divide-default p-0">
                <li v-for="s in sessions ?? []" :key="s.id" class="flex items-center gap-3 px-4 py-3 sm:px-5">
                  <UIcon :name="s.os === 'iOS' || s.os === 'Android' ? 'i-cg-browser' : 'i-cg-browser'" class="size-5 shrink-0 text-muted" />
                  <div class="min-w-0 flex-1">
                    <p class="flex flex-wrap items-center gap-2 text-sm text-highlighted">
                      {{ device(s) }}
                      <UBadge v-if="s.current" size="sm" color="primary" variant="subtle" :label="t('security.thisDevice')" />
                    </p>
                    <p class="text-xs text-muted">
                      <template v-if="s.ip">
                        {{ s.ip }} ·
                      </template>{{ t('security.started', { when: fmtAgo(s.created_at, locale) }) }}<template v-if="s.last_seen && !s.current">
                        · {{ t('security.lastSeen', { when: fmtAgo(s.last_seen, locale) }) }}
                      </template>
                    </p>
                  </div>
                  <UButton v-if="!s.current && !enrolRequired" size="sm" color="error" variant="outline" :label="t('security.revoke')" @click="revoke(s.id)" />
                </li>
              </ul>
            </PanelSection>

            <PanelSection id="api-tokens" v-if="isAdmin && !enrolRequired" :title="t('security.tokens')" :description="t('security.tokensHint')">
              <UAlert v-if="createdSecret" color="success" variant="subtle" icon="i-cg-key" :title="t('security.tokenOnce')">
                <template #description>
                  <span class="mt-1 flex min-w-0 items-center gap-2">
                    <code class="mono min-w-0 flex-1 text-xs [overflow-wrap:anywhere] text-highlighted">{{ createdSecret }}</code>
                    <CopyButton :value="createdSecret" />
                    <UButton size="xs" color="neutral" variant="outline" :label="t('security.tokenStored')" @click="createdSecret = null" />
                  </span>
                </template>
              </UAlert>
              <UForm :state="newToken" class="flex flex-wrap items-end gap-3" @submit="createToken">
                <UFormField :label="t('security.tokenName')" class="min-w-48 flex-[1_1_240px]">
                  <UInput v-model="newToken.name" maxlength="64" placeholder="ci-deploy" class="w-full" autocomplete="off" />
                </UFormField>
                <UFormField :label="t('security.tokenExpires')" class="w-44">
                  <USelect v-model="newToken.expires" :items="expiryItems" class="w-full" />
                </UFormField>
                <UButton type="submit" icon="i-cg-plus" :label="t('security.tokenCreate')" :disabled="!newToken.name.trim()" :loading="busy" />
              </UForm>
              <p v-if="tokens && !tokens.length" class="text-sm text-muted">
                {{ t('security.tokensNone') }}
              </p>
              <ul v-else-if="tokens" class="m-0 list-none divide-y divide-default border-y border-default p-0">
                <li v-for="tok in tokens" :key="tok.id" class="flex items-center gap-3 py-2.5">
                  <UIcon name="i-cg-key" class="size-5 shrink-0 text-muted" />
                  <div class="min-w-0 flex-1">
                    <p class="flex flex-wrap items-center gap-2 text-sm text-highlighted">
                      {{ tok.name }}
                      <code class="mono text-xs text-muted">{{ tok.hint }}…</code>
                    </p>
                    <p class="mono text-xs [overflow-wrap:anywhere] text-muted">
                      {{ tok.email }} · {{ t('security.tokenMade', { when: fmtAgo(tok.created_at, locale) }) }} · {{ tok.expires_at ? t('security.tokenUntil', { when: fmtDateTime(tok.expires_at, locale) }) : t('security.tokenNever') }} · {{ tok.last_used_at ? t('security.lastSeen', { when: fmtAgo(tok.last_used_at, locale) }) : t('security.tokenUnused') }}
                    </p>
                  </div>
                  <UButton size="sm" color="error" variant="outline" :label="t('security.revoke')" @click="revokeToken(tok)" />
                </li>
              </ul>
            </PanelSection>
          </div>
        </div>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
