<script setup lang="ts">
/*
 * The way in from the panel: the Database tab asks the API for a one-time
 * token and opens this page with it in the fragment (never sent to a
 * server, never in a Referer). The page exchanges it, with the panel's own
 * session, for a Quarry session, then forgets it.
 */
import type { StudioSession } from '~/composables/useStudio'

definePageMeta({ layout: 'auth' })

const { t } = useI18n()
const home = STUDIO_HOME
const api = useApi()
const { session, dbLink } = useStudio()
useHead({ title: () => t('studio.opening') })
const error = ref<string | null>(null)

onMounted(async () => {
  const token = window.location.hash.replace(/^#/, '')
  // Out of the address bar and the history at once: it works once anyway.
  history.replaceState(null, '', window.location.pathname)
  if (!token) {
    error.value = t('studio.handoffMissing')
    return
  }
  try {
    const s = await api.post<StudioSession>(`${STUDIO_API}/handoff`, { token }, { skipAuthRedirect: true })
    session.value = s
    await navigateTo(s.database ? dbLink(s.database) : `${STUDIO_HOME}/db`, { replace: true })
  } catch (e) {
    error.value = errorMessage(e)
  }
})
</script>

<template>
  <div class="space-y-6">
    <PageHeader size="md" :title="t('studio.opening')" :accent="t('studio.openingAccent')" />
    <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="t('studio.handoffFailed')" :description="error" />
    <div v-else class="flex items-center gap-3 text-sm text-muted">
      <UIcon name="i-cg-loader" class="size-4 animate-spin" />
      {{ t('studio.openingHint') }}
    </div>
    <div v-if="error" class="flex flex-wrap gap-2">
      <UButton to="/" color="neutral" variant="outline" icon="i-cg-arrow-left" :label="t('studio.toPanel')" />
      <UButton color="primary" variant="solid" :to="home" icon="i-cg-login" :label="t('studio.signInTitle')" />
    </div>
  </div>
</template>
