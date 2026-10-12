<script setup lang="ts">
definePageMeta({ layout: 'auth' })

const { t } = useI18n()
const api = useApi()
const { fetchMe, redirectTarget } = useAuth()
useHead({ title: () => t('auth.signIn') })

const state = reactive({ email: '', password: '', totp: '' })
const needTotp = ref(false)
const loading = ref(false)
const error = ref<string | null>(null)
const needsSetup = ref(false)

onMounted(async () => {
  try {
    const b = await api.get<{ setup_complete: boolean }>('/api/bootstrap', { skipAuthRedirect: true })
    needsSetup.value = !b.setup_complete
  } catch {
    // Assume a normal sign-in when the probe fails.
  }
})

async function submit() {
  loading.value = true
  error.value = null
  try {
    await api.post('/api/login', { email: state.email, password: state.password, totp: state.totp }, { skipAuthRedirect: true })
    await fetchMe()
    await navigateTo(redirectTarget())
  } catch (e) {
    if (isApiError(e) && e.status === 401 && e.data?.totp_required) {
      error.value = needTotp.value ? e.message : null
      needTotp.value = true
      state.totp = ''
    } else if (isApiError(e) && e.status === 429) {
      error.value = e.message
    } else {
      error.value = isApiError(e) && e.status === 401 ? t('auth.invalid') : errorMessage(e)
    }
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="space-y-6">
    <div class="flex flex-col gap-3">
      <PageHeader
        size="md"
        :title="t(`cg.pages.${needTotp ? 'totp' : 'login'}.title`)"
        :accent="t(`cg.pages.${needTotp ? 'totp' : 'login'}.accent`)"
      />
      <p class="text-sm text-muted">
        {{ needTotp ? t('auth.twoFactorHint') : t('auth.signInHint') }}
      </p>
    </div>

    <UAlert v-if="needsSetup" color="info" variant="subtle" icon="i-cg-info" :description="t('auth.setupPending')" />
    <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />

    <UForm :state="state" class="space-y-5 border-t border-default pt-6" @submit="submit">
      <template v-if="!needTotp">
        <UFormField :label="t('common.email')" name="email" required>
          <UInput v-model="state.email" type="email" autocomplete="username" class="w-full" autofocus />
        </UFormField>
        <UFormField :label="t('common.password')" name="password" required>
          <UInput v-model="state.password" type="password" autocomplete="current-password" class="w-full" />
        </UFormField>
      </template>
      <UFormField v-else :label="t('auth.code')" name="totp" required>
        <UInput v-model="state.totp" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000" class="w-full" :ui="{ base: 'text-center font-mono text-lg tracking-[0.4em]' }" autofocus />
      </UFormField>
      <UButton color="primary" variant="solid" type="submit" size="lg" :label="needTotp ? t('auth.verify') : t('auth.signIn')" block :loading="loading" />
      <UButton v-if="needTotp" color="neutral" variant="link" block :label="t('common.back')" @click="needTotp = false; error = null" />
    </UForm>
  </div>
</template>
