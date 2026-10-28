<script setup lang="ts">
/**
 * Settings → Functions: the functions domain, where every function answers
 * at https://<domain>/<name>, its DNS and its certificate
 * (GET /api/functions/domain/status, PUT /api/functions/domain;
 * internal/api SPEC §17.5). A name another site or function holds is a 409,
 * shown on the field.
 */
import type { FnDomainStatus } from '~/utils/functions'

const { t } = useI18n()
const api = useApi()
const toast = useToast()
const { isAdmin } = useAuth()

const { data: status, refresh, pending } = useLoad(() => api.get<FnDomainStatus>('/api/functions/domain/status'))
const domain = ref('')
watch(status, (s) => { if (s) domain.value = s.domain }, { immediate: true })
const checking = ref(false)
async function again() {
  checking.value = true
  await refresh()
  checking.value = false
}

const busy = ref(false)
const refused = ref('')
const changed = computed(() => !!status.value && domain.value.trim().toLowerCase() !== status.value.domain)
async function save() {
  busy.value = true
  refused.value = ''
  try {
    status.value = await api.put<FnDomainStatus>('/api/functions/domain', { domain: domain.value.trim().toLowerCase() })
    toast.add({ title: status.value.domain ? t('fn.server.domainSaved') : t('fn.server.domainCleared'), color: 'success', icon: 'i-cg-check-circle' })
  } catch (e) {
    if (isApiError(e) && (e.status === 409 || e.status === 400)) refused.value = t('address.refused', { why: e.message })
    else toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <PanelSection id="functions" :title="t('fn.title')" :description="t('fn.server.hint')" icon="i-cg-webhook">
    <UFormField :label="t('fn.server.domain')" :help="t('fn.server.domainHelp')" :error="refused || undefined">
      <UInput v-model="domain" placeholder="fn.example.com" class="w-full font-mono sm:w-96" :disabled="!isAdmin || pending" @update:model-value="refused = ''" @keydown.enter="isAdmin && changed && save()" />
    </UFormField>
    <UAlert v-if="status && !status.acme_email" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('fn.server.noAcme')" />
    <FnDnsStatus v-if="status?.domain && status.dns" :checks="[status.dns]" :certificate="status.certificate" :loading="checking" class="max-w-[48rem]" @again="again" />
    <template v-if="isAdmin" #footer>
      <UButton :label="t('common.save')" :loading="busy" :disabled="!changed" @click="save" />
      <NuxtLink to="/functions" class="text-[13px] text-muted hover:text-highlighted">{{ t('fn.server.open') }}</NuxtLink>
    </template>
  </PanelSection>
</template>
