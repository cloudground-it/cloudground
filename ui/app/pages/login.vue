<script setup lang="ts">
definePageMeta({ layout: 'auth' })

const { t } = useI18n()
const api = useApi()
const { fetchMe, redirectTarget } = useAuth()
useHead({ title: () => t('auth.signIn') })

const state = reactive({ email: '', password: '', totp: '', recovery: '' })
/** After the password: the second factors the account can answer with. */
const factorStep = ref(false)
const factors = ref<string[]>([])
const passkeyOptions = ref<Record<string, unknown> | null>(null)
const method = ref<'code' | 'recovery'>('code')
const loading = ref(false)
const passkeyBusy = ref(false)
const error = ref<string | null>(null)
const needsSetup = ref(false)
const canPasskey = ref(false)

onMounted(async () => {
  canPasskey.value = passkeysSupported()
  try {
    const b = await api.get<{ setup_complete: boolean }>('/api/bootstrap', { skipAuthRedirect: true })
    needsSetup.value = !b.setup_complete
  } catch {
    // Assume a normal sign-in when the probe fails.
  }
})

const hasTotp = computed(() => factors.value.includes('totp'))
const hasRecovery = computed(() => factors.value.includes('recovery_code'))
const hasPasskey = computed(() => canPasskey.value && !!passkeyOptions.value)

async function signedIn() {
  // fetchMe learns whether the 2FA requirement holds the account: the
  // route guard then takes it straight to its setup.
  await fetchMe()
  await navigateTo(redirectTarget())
}

/** A refusal of the sign-in, as the page shows it. */
function refused(e: unknown) {
  if (isApiError(e) && e.status === 401 && e.data?.totp_required) {
    const first = !factorStep.value
    factors.value = Array.isArray(e.data.factors) ? e.data.factors : ['totp']
    passkeyOptions.value = (e.data.passkey as Record<string, unknown> | undefined) ?? null
    if (first) method.value = hasTotp.value ? 'code' : 'recovery'
    error.value = first ? null : e.message
    factorStep.value = true
    state.totp = ''
    state.recovery = ''
  } else if (isApiError(e) && e.status === 403 && e.data?.password_off) {
    error.value = t('auth.passwordOff')
  } else if (isApiError(e) && e.status === 429) {
    error.value = e.message
  } else {
    error.value = isApiError(e) && e.status === 401 ? t('auth.invalid') : errorMessage(e)
  }
}

async function submit() {
  loading.value = true
  error.value = null
  const body: Record<string, unknown> = { email: state.email, password: state.password }
  if (factorStep.value && method.value === 'code') body.totp = state.totp
  if (factorStep.value && method.value === 'recovery') body.recovery_code = state.recovery
  try {
    await api.post('/api/login', body, { skipAuthRedirect: true })
    await signedIn()
  } catch (e) {
    refused(e)
  } finally {
    loading.value = false
  }
}

/** The password was right: the account's passkey is the second factor. */
async function secondFactorPasskey() {
  if (!passkeyOptions.value) return
  passkeyBusy.value = true
  error.value = null
  try {
    const assertion = await getPasskey(passkeyOptions.value)
    await api.post('/api/login', { email: state.email, password: state.password, passkey: assertion }, { skipAuthRedirect: true })
    await signedIn()
  } catch (e) {
    if (passkeyCancelled(e)) error.value = t('auth.passkeyCancelled')
    else refused(e)
  } finally {
    passkeyBusy.value = false
  }
}

/** Signing in with a passkey alone: no e-mail, no password. */
async function passkeySignIn() {
  passkeyBusy.value = true
  error.value = null
  try {
    const options = await api.post<Record<string, unknown>>('/api/login/passkey/begin', {}, { skipAuthRedirect: true })
    const assertion = await getPasskey(options)
    await api.post('/api/login/passkey', { credential: assertion }, { skipAuthRedirect: true })
    await signedIn()
  } catch (e) {
    if (passkeyCancelled(e)) error.value = t('auth.passkeyCancelled')
    else if (isApiError(e) && e.status === 401) error.value = t('auth.passkeyFailed')
    else error.value = errorMessage(e)
  } finally {
    passkeyBusy.value = false
  }
}

function back() {
  factorStep.value = false
  error.value = null
}

const factorReady = computed(() => method.value === 'code' ? state.totp.trim().length === 6 : state.recovery.trim().length >= 16)
</script>

<template>
  <div class="space-y-6">
    <div class="flex flex-col gap-3">
      <PageHeader
        size="md"
        :title="t(`cg.pages.${factorStep ? 'totp' : 'login'}.title`)"
        :accent="t(`cg.pages.${factorStep ? 'totp' : 'login'}.accent`)"
      />
      <p class="text-sm text-muted">
        <template v-if="!factorStep">
          {{ t('auth.signInHint') }}
        </template>
        <template v-else-if="method === 'recovery'">
          {{ t('auth.recoveryHint') }}
        </template>
        <template v-else-if="hasTotp">
          {{ t('auth.twoFactorHint') }}
        </template>
        <template v-else>
          {{ t('auth.passkeyHint') }}
        </template>
      </p>
    </div>

    <UAlert v-if="needsSetup" color="info" variant="subtle" icon="i-cg-info" :description="t('auth.setupPending')" />
    <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />

    <UForm :state="state" class="space-y-5 border-t border-default pt-6" @submit="submit">
      <template v-if="!factorStep">
        <UFormField :label="t('common.email')" name="email" required>
          <UInput v-model="state.email" type="email" autocomplete="username webauthn" class="w-full" autofocus />
        </UFormField>
        <UFormField :label="t('common.password')" name="password" required>
          <UInput v-model="state.password" type="password" autocomplete="current-password" class="w-full" />
        </UFormField>
        <UButton color="primary" variant="solid" type="submit" size="lg" :label="t('auth.signIn')" block :loading="loading" />
        <template v-if="canPasskey">
          <USeparator :label="t('auth.or')" />
          <UButton color="neutral" variant="outline" size="lg" icon="i-cg-key" :label="t('auth.signInPasskey')" block :loading="passkeyBusy" @click="passkeySignIn" />
        </template>
      </template>

      <template v-else>
        <UAlert v-if="!hasTotp && !hasRecovery && !hasPasskey" color="warning" variant="subtle" icon="i-cg-alert-circle" :description="t('auth.passkeyElsewhere')" />
        <UButton v-if="hasPasskey" color="primary" :variant="hasTotp && method === 'code' ? 'outline' : 'solid'" size="lg" icon="i-cg-key" :label="t('auth.usePasskey')" block :loading="passkeyBusy" @click="secondFactorPasskey" />
        <template v-if="method === 'code' && hasTotp">
          <UFormField :label="t('auth.code')" name="totp" required>
            <UInput v-model="state.totp" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000" class="w-full" :ui="{ base: 'text-center font-mono text-lg tracking-[0.4em]' }" autofocus />
          </UFormField>
          <UButton color="primary" variant="solid" type="submit" size="lg" :label="t('auth.verify')" block :loading="loading" :disabled="!factorReady" />
        </template>
        <template v-else-if="method === 'recovery' && hasRecovery">
          <UFormField :label="t('auth.recoveryCode')" name="recovery" required>
            <UInput v-model="state.recovery" autocomplete="off" placeholder="xxxx-xxxx-xxxx-xxxx" class="w-full" :ui="{ base: 'text-center font-mono text-lg' }" autofocus />
          </UFormField>
          <UButton color="primary" variant="solid" type="submit" size="lg" :label="t('auth.verify')" block :loading="loading" :disabled="!factorReady" />
        </template>
        <div class="flex flex-col items-center gap-1">
          <UButton v-if="hasTotp && method !== 'code'" color="neutral" variant="link" :label="t('auth.useCode')" @click="method = 'code'; error = null" />
          <UButton v-if="hasRecovery && method !== 'recovery'" color="neutral" variant="link" :label="t('auth.useRecoveryCode')" @click="method = 'recovery'; error = null" />
          <UButton color="neutral" variant="link" :label="t('common.back')" @click="back" />
        </div>
      </template>
    </UForm>
  </div>
</template>
