<script setup lang="ts">
/**
 * Where a function answers and who may call it: its public addresses
 * (internal/functions SPEC §1.2), its own domains with their DNS and
 * certificate (PUT …/domains, GET …/domains/status; a name another site or
 * function holds is a 409, shown on the field), public or token access
 * with a new token on demand (POST …/token), and its CORS origins.
 */
import type { FnAuth, FnOwnDomainsStatus, FnView } from '~/utils/functions'

const { t } = useI18n()
const api = useApi()
const toast = useToast()
const { isAdmin } = useAuth()
const confirm = useConfirm()
const { fn, domain, base, refresh } = useCurrentFunction()

// ---- own domains ----
const domains = ref<string[]>([])
const { data: dstatus, refresh: refreshStatus } = useLoad(() => fn.value.domains.length ? api.get<FnOwnDomainsStatus>(`${base.value}/domains/status`) : Promise.resolve(null))
const checking = ref(false)
async function checkAgain() {
  checking.value = true
  await refreshStatus()
  checking.value = false
}
const domainsChanged = computed(() => JSON.stringify(domains.value.map(d => d.trim().toLowerCase()).filter(Boolean)) !== JSON.stringify(fn.value.domains))
const domainsError = ref('')
const savingDomains = ref(false)
async function saveDomains() {
  savingDomains.value = true
  domainsError.value = ''
  try {
    await api.put(`${base.value}/domains`, { domains: domains.value.map(d => d.trim().toLowerCase()).filter(Boolean) })
    toast.add({ title: t('fn.addr.domainsSaved'), color: 'success', icon: 'i-cg-check-circle' })
    await refresh()
    await refreshStatus()
  } catch (e) {
    if (isApiError(e) && (e.status === 409 || e.status === 400)) domainsError.value = t('address.refused', { why: e.message })
    else toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    savingDomains.value = false
  }
}

// ---- access ----
const access = reactive({ auth: 'public' as FnAuth, cors: '' })
function load() {
  domains.value = [...fn.value.domains]
  access.auth = fn.value.auth
  access.cors = (fn.value.cors_origins ?? []).join('\n')
}
watch(fn, load, { immediate: true })
const corsList = computed(() => access.cors.split(/[\s,]+/).map(s => s.trim()).filter(Boolean))
const accessChanged = computed(() => access.auth !== fn.value.auth || JSON.stringify(corsList.value) !== JSON.stringify(fn.value.cors_origins ?? []))
const authItems = computed(() => (['public', 'token'] as FnAuth[]).map(v => ({ value: v, label: t(`fn.auth.${v}`), description: t(`fn.auth.${v}Hint`) })))
const token = ref('')
const savingAccess = ref(false)
const accessError = ref('')
async function saveAccess() {
  savingAccess.value = true
  accessError.value = ''
  try {
    const v = await api.put<FnView>(base.value, { auth: access.auth, cors_origins: corsList.value })
    if (v.token) token.value = v.token
    toast.add({ title: t('fn.addr.accessSaved'), color: 'success', icon: 'i-cg-check-circle' })
    await refresh()
  } catch (e) {
    if (isApiError(e) && e.status === 400) accessError.value = t('address.refused', { why: e.message })
    else toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    savingAccess.value = false
  }
}
async function newToken() {
  const ok = await confirm({ title: t('fn.addr.newTokenTitle'), description: t('fn.addr.newTokenHint'), confirmLabel: t('fn.addr.newToken'), danger: fn.value.has_token })
  if (!ok) return
  try {
    token.value = (await api.post<{ token: string }>(`${base.value}/token`, {})).token
    await refresh()
  } catch (e) {
    toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  }
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <PanelSection :title="t('fn.addr.urlsTitle')" :description="t('fn.addr.urlsHint')" icon="i-cg-link">
      <ul v-if="fn.urls.length" class="m-0 flex list-none flex-col border border-default p-0" data-testid="fn-urls">
        <li v-for="u in fn.urls" :key="u" class="flex min-w-0 items-center gap-2 border-b border-default px-3 py-2 last:border-b-0">
          <code class="mono min-w-0 flex-1 text-[13px] [overflow-wrap:anywhere] text-highlighted">{{ u }}</code>
          <CopyButton :value="u" />
          <UButton size="xs" color="neutral" variant="ghost" square icon="i-cg-external-link" :to="u" target="_blank" :aria-label="t('fn.open')" />
        </li>
      </ul>
      <p v-else class="m-0 text-sm text-muted">{{ t('fn.addr.noUrls') }}</p>
      <UAlert
        v-if="!domain"
        color="info"
        variant="subtle"
        icon="i-cg-info"
        :title="t('fn.noDomain.title')"
        :description="t('fn.noDomain.hint')"
        :actions="[{ label: t('fn.noDomain.action'), to: '/settings#functions', color: 'neutral', variant: 'outline', size: 'sm' }]"
      />
    </PanelSection>

    <PanelSection :title="t('fn.addr.domainsTitle')" :description="t('fn.addr.domainsHint')" icon="i-cg-sites">
      <UFormField :label="t('fn.addr.domains')" :error="domainsError || undefined">
        <div class="flex max-w-xl flex-col gap-2">
          <div v-for="(_, i) in domains" :key="i" class="flex gap-2">
            <UInput v-model="domains[i]" placeholder="api.example.com" class="min-w-0 flex-1 font-mono" :disabled="!isAdmin" :aria-label="t('fn.addr.domains')" @update:model-value="domainsError = ''" />
            <UButton v-if="isAdmin" color="neutral" variant="ghost" square icon="i-cg-trash" :aria-label="t('common.remove')" @click="domains.splice(i, 1)" />
          </div>
          <p v-if="!domains.length" class="m-0 text-sm text-muted">{{ t('fn.addr.noDomains') }}</p>
          <UButton v-if="isAdmin && domains.length < 10" color="neutral" variant="outline" size="sm" icon="i-cg-plus" :label="t('fn.addr.addDomain')" class="self-start" @click="domains.push('')" />
        </div>
      </UFormField>
      <FnDnsStatus v-if="dstatus?.domains.length" :checks="dstatus.domains.map(d => d.dns)" :certificate="dstatus.certificate" :loading="checking" class="max-w-[48rem]" @again="checkAgain" />
      <template v-if="isAdmin" #footer>
        <UButton :label="t('common.save')" :loading="savingDomains" :disabled="!domainsChanged" @click="saveDomains" />
        <span class="text-[13px] text-muted">{{ t('fn.addr.domainsSaveHint') }}</span>
      </template>
    </PanelSection>

    <PanelSection :title="t('fn.addr.accessTitle')" :description="t('fn.addr.accessHint')" icon="i-cg-lock">
      <FnTokenOnce v-if="token" :token="token" @done="token = ''" />
      <URadioGroup
        v-model="access.auth"
        :legend="t('fn.create.access')"
        :items="authItems"
        variant="card"
        orientation="horizontal"
        :disabled="!isAdmin"
        :ui="{ legend: 'mb-2 text-sm font-medium text-highlighted', fieldset: 'flex-wrap gap-2', item: 'min-w-48 flex-1 max-w-sm' }"
      />
      <p v-if="fn.auth === 'token'" class="m-0 flex items-center gap-2 text-[13px] text-muted">
        <UIcon name="i-cg-key" class="size-4" aria-hidden="true" />
        {{ fn.has_token ? t('fn.addr.hasToken') : t('fn.addr.noToken') }}
      </p>
      <UFormField :label="t('fn.addr.cors')" :help="t('fn.addr.corsHelp')" :error="accessError || undefined">
        <UTextarea v-model="access.cors" :rows="3" placeholder="https://www.example.com" class="w-full max-w-xl" :ui="{ base: 'font-mono text-[13px]' }" spellcheck="false" :disabled="!isAdmin" />
      </UFormField>
      <template v-if="isAdmin" #footer>
        <UButton :label="t('common.save')" :loading="savingAccess" :disabled="!accessChanged" @click="saveAccess" />
        <UButton color="neutral" variant="outline" icon="i-cg-key" :label="t('fn.addr.newToken')" @click="newToken" />
      </template>
    </PanelSection>
  </div>
</template>
