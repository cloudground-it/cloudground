<script setup lang="ts">
/*
 * Quarry's sign-in: a database user's own name and password. MariaDB
 * checks them, and Quarry then shows exactly what that user is granted.
 * Someone signed in to the panel opens Quarry from a site's Database tab
 * instead (pages/quarry/handoff.vue).
 */
import type { StudioSession } from '~/composables/useStudio'

definePageMeta({ layout: 'auth' })

const { t } = useI18n()
const api = useApi()
const { session, ended, load, dbLink } = useStudio()
useHead({ title: () => t('studio.signInTitle') })

const state = reactive({ user: '', password: '', database: '' })
const loading = ref(false)
const error = ref<string | null>(null)
const checking = ref(true)

function home(s: StudioSession) {
  const db = s.database || s.databases?.[0] || ''
  return db ? dbLink(db) : `${STUDIO_HOME}/db`
}

onMounted(async () => {
  const s = session.value ?? await load()
  checking.value = false
  if (s) await navigateTo(home(s), { replace: true })
})

async function submit() {
  loading.value = true
  error.value = null
  ended.value = ''
  try {
    const s = await api.post<StudioSession>(`${STUDIO_API}/login`, {
      user: state.user.trim(), password: state.password, database: state.database.trim()
    }, { skipAuthRedirect: true })
    session.value = s
    state.password = ''
    await navigateTo(home(s))
  } catch (e) {
    error.value = isApiError(e) && e.status === 401 ? t('studio.invalid') : errorMessage(e)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="space-y-6">
    <div class="flex flex-col gap-3">
      <PageHeader size="md" :title="t('studio.signInTitle')" :accent="t('studio.signInAccent')" />
      <p class="text-sm text-muted">
        {{ t('studio.signInHint') }}
      </p>
    </div>

    <UAlert v-if="ended" color="warning" variant="subtle" icon="i-cg-clock" :title="t('studio.ended')" :description="ended" />
    <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />

    <UForm :state="state" class="space-y-5 border-t border-default pt-6" :disabled="checking" @submit="submit">
      <UFormField :label="t('studio.dbUser')" name="user" required>
        <UInput v-model="state.user" autocomplete="username" autocapitalize="off" spellcheck="false" class="w-full font-mono" autofocus />
      </UFormField>
      <UFormField :label="t('common.password')" name="password" required>
        <UInput v-model="state.password" type="password" autocomplete="current-password" class="w-full" />
      </UFormField>
      <UFormField :label="t('studio.database')" :hint="t('common.optional')" name="database">
        <UInput v-model="state.database" autocapitalize="off" spellcheck="false" class="w-full font-mono" />
      </UFormField>
      <UButton color="primary" variant="solid" type="submit" size="lg" icon="i-cg-login" :label="t('auth.signIn')" block :loading="loading" :disabled="!state.user || !state.password" />
    </UForm>

    <p class="m-0 border-t border-default pt-4 text-[13px] leading-snug text-muted">
      {{ t('studio.fromPanel') }}
      <NuxtLink to="/" class="text-highlighted underline underline-offset-2">
        {{ t('studio.toPanel') }}
      </NuxtLink>
    </p>
  </div>
</template>
