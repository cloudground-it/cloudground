<script setup lang="ts">
import type { Proof } from '~/types'

/**
 * Confirming it is you before a change to how the account signs in (api
 * SPEC §2.4): the password while it signs in, then one current factor when
 * the account has two-factor — a code, a passkey or a recovery code.
 */
const props = defineProps<{
  title: string
  description?: string
  confirmLabel?: string
  danger?: boolean
  /** The account's password signs in, so it is asked for. */
  needsPassword: boolean
  /** The factors the account has; none means the password alone. */
  totp?: boolean
  passkey?: boolean
  recovery?: boolean
  /** Only a passkey that checks who holds it will do (password sign-in off). */
  passkeyOnly?: boolean
}>()
const emit = defineEmits<{ close: [Proof | null] }>()
const { t } = useI18n()
const api = useApi()

const password = ref('')
const code = ref('')
const recoveryCode = ref('')
const method = ref<'code' | 'recovery'>(props.totp ? 'code' : 'recovery')
const error = ref<string | null>(null)
const waiting = ref(false)

const hasFactor = computed(() => props.totp || props.passkey || props.recovery)
const passwordOk = computed(() => !props.needsPassword || password.value.length > 0)
const typedOk = computed(() => {
  if (!passwordOk.value || props.passkeyOnly) return false
  if (!hasFactor.value) return true
  if (method.value === 'code' && props.totp) return code.value.trim().length === 6
  if (method.value === 'recovery' && props.recovery) return recoveryCode.value.trim().length >= 16
  return false
})

function submitTyped() {
  if (!typedOk.value) return
  const proof: Proof = {}
  if (props.needsPassword) proof.password = password.value
  if (hasFactor.value && method.value === 'code') proof.code = code.value.trim()
  if (hasFactor.value && method.value === 'recovery') proof.recovery_code = recoveryCode.value.trim()
  emit('close', proof)
}

async function usePasskey() {
  if (!passwordOk.value) return
  error.value = null
  waiting.value = true
  try {
    const options = await api.post<Record<string, unknown>>('/api/account/passkeys/challenge', {})
    const assertion = await getPasskey(options)
    const proof: Proof = { passkey: assertion }
    if (props.needsPassword) proof.password = password.value
    emit('close', proof)
  } catch (e) {
    if (!passkeyCancelled(e)) error.value = errorMessage(e)
  } finally {
    waiting.value = false
  }
}
</script>

<template>
  <UModal :title="title" :description="description" :close="false" :dismissible="false">
    <template #body>
      <form class="flex flex-col gap-4" @submit.prevent="submitTyped">
        <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />
        <UFormField v-if="needsPassword" :label="t('common.password')">
          <UInput v-model="password" type="password" autocomplete="current-password" class="w-full" autofocus />
        </UFormField>
        <template v-if="passkeyOnly || !needsPassword">
          <p class="text-sm text-muted">
            {{ t('proof.passkeyOnly') }}
          </p>
        </template>
        <template v-else-if="hasFactor">
          <p class="text-sm text-muted">
            {{ t('proof.factorHint') }}
          </p>
          <UFormField v-if="method === 'code' && totp" :label="t('auth.code')">
            <UInput v-model="code" inputmode="numeric" maxlength="6" placeholder="000000" autocomplete="one-time-code" class="w-full font-mono" />
          </UFormField>
          <UFormField v-if="method === 'recovery' && recovery" :label="t('auth.recoveryCode')">
            <UInput v-model="recoveryCode" autocomplete="off" placeholder="xxxx-xxxx-xxxx-xxxx" class="w-full font-mono" />
          </UFormField>
          <div class="flex flex-wrap gap-x-4 gap-y-1">
            <UButton v-if="totp && method !== 'code'" color="neutral" variant="link" class="px-0" :label="t('auth.useCode')" @click="method = 'code'" />
            <UButton v-if="recovery && method !== 'recovery'" color="neutral" variant="link" class="px-0" :label="t('auth.useRecoveryCode')" @click="method = 'recovery'" />
          </div>
        </template>
        <button type="submit" class="hidden" />
      </form>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="emit('close', null)" />
        <UButton v-if="passkey || passkeyOnly || !needsPassword" color="neutral" variant="outline" icon="i-cg-key" :label="t('auth.usePasskey')" :loading="waiting" :disabled="!passwordOk" @click="usePasskey" />
        <UButton v-if="!passkeyOnly && needsPassword" variant="solid" :color="danger ? 'error' : 'primary'" :label="confirmLabel || t('common.confirm')" :disabled="!typedOk" @click="submitTyped" />
      </div>
    </template>
  </UModal>
</template>
