<script setup lang="ts">
definePageMeta({ layout: 'auth' })

const { t } = useI18n()
const api = useApi()
const route = useRoute()
const { fetchMe } = useAuth()
useHead({ title: () => t('auth.setup') })

const state = reactive({ first_name: '', last_name: '', email: '', password: '', confirm: '' })
const loading = ref(false)
const error = ref<string | null>(null)

async function submit() {
  error.value = null
  if (state.password.length < 12) {
    error.value = t('auth.passwordLength')
    return
  }
  if (state.password !== state.confirm) {
    error.value = t('auth.passwordMismatch')
    return
  }
  loading.value = true
  try {
    await api.post('/api/setup', { email: state.email, password: state.password, token: String(route.params.token) }, { skipAuthRedirect: true })
    if (state.first_name || state.last_name) {
      await api.put('/api/account/profile', { first_name: state.first_name, last_name: state.last_name }).catch(() => {})
    }
    await fetchMe()
    await navigateTo('/security')
  } catch (e) {
    error.value = errorMessage(e)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="space-y-6">
    <div class="flex flex-col gap-3">
      <PageHeader size="md" :title="t('cg.pages.setup.title')" :accent="t('cg.pages.setup.accent')" />
      <p class="text-sm text-muted">
        {{ t('auth.setupHint') }}
      </p>
    </div>
    <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />
    <UForm :state="state" class="space-y-5 border-t border-default pt-6" @submit="submit">
      <div class="grid gap-4 sm:grid-cols-2">
        <UFormField :label="t('auth.firstName')">
          <UInput v-model="state.first_name" autocomplete="given-name" class="w-full" />
        </UFormField>
        <UFormField :label="t('auth.lastName')">
          <UInput v-model="state.last_name" autocomplete="family-name" class="w-full" />
        </UFormField>
      </div>
      <UFormField :label="t('common.email')" required>
        <UInput v-model="state.email" type="email" autocomplete="username" class="w-full" autofocus />
      </UFormField>
      <UFormField :label="t('common.password')" :hint="t('auth.min12')" required>
        <UInput v-model="state.password" type="password" autocomplete="new-password" class="w-full" />
      </UFormField>
      <UFormField :label="t('auth.confirmPassword')" required>
        <UInput v-model="state.confirm" type="password" autocomplete="new-password" class="w-full" />
      </UFormField>
      <UButton color="primary" variant="solid" type="submit" size="lg" :label="t('auth.createAccount')" block :loading="loading" />
    </UForm>
  </div>
</template>
