<script setup lang="ts">
/**
 * Where a site answers and what it serves: its aliases and proxy upstream
 * (an administrator's), and the folder of the release nginx serves. All
 * three are PATCH /api/sites/{id} fields; a refusal is shown on its field.
 */
const { t } = useI18n()
const api = useApi()
const toast = useToast()
const { canManage, isAdmin } = useAuth()
const { site, refresh } = useCurrentSite()

const stored = () => ({
  aliases: [...(site.value.aliases ?? [])],
  public_root: site.value.config.public_root ?? '',
  proxy_upstream: site.value.config.proxy_upstream ?? ''
})
const form = reactive(stored())
watch(() => [site.value.aliases, site.value.config], () => Object.assign(form, stored()), { deep: true })

const isProxy = computed(() => site.value.type === 'proxy')
const showAliases = computed(() => !site.value.staging_of)
const changes = computed(() => {
  const now = stored()
  const body: Record<string, unknown> = {}
  if (isAdmin.value && showAliases.value && JSON.stringify(form.aliases) !== JSON.stringify(now.aliases)) body.aliases = form.aliases
  if (!isProxy.value && form.public_root.trim() !== now.public_root) body.public_root = form.public_root.trim()
  if (isAdmin.value && isProxy.value && form.proxy_upstream.trim() !== now.proxy_upstream) body.proxy_upstream = form.proxy_upstream.trim()
  return body
})
const errors = ref<Record<string, string>>({})
const busy = ref(false)

// The API's warning about a custom certificate, in the interface's language.
const CUSTOM_CERT = 'this site serves a custom certificate'
async function save() {
  busy.value = true
  errors.value = {}
  try {
    const r = await api.patch<{ warnings?: string[] }>(`/api/sites/${site.value.id}`, changes.value)
    toast.add({ title: t('common.applied'), color: 'success', icon: 'i-cg-check-circle' })
    for (const w of r.warnings ?? []) {
      toast.add({ title: w.startsWith(CUSTOM_CERT) ? t('address.customCertWarning') : w, color: 'warning', icon: 'i-cg-alert-triangle' })
    }
    await refresh()
  } catch (e) {
    const fields = isApiError(e) ? e.data?.fields as Record<string, string> | undefined : undefined
    if (fields && Object.keys(fields).length) errors.value = fields
    else toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
  } finally {
    busy.value = false
  }
}
const fieldError = (name: string) => errors.value[name] ? t('address.refused', { why: errors.value[name] }) : undefined
</script>

<template>
  <PanelSection :title="t('address.title')" :description="t('address.hint')">
    <div class="flex max-w-2xl flex-col gap-4">
      <UFormField v-if="showAliases" :label="t('address.aliases')" :help="isAdmin ? t('address.aliasesHint') : t('address.adminOnly')" :error="fieldError('aliases')">
        <UInputTags v-model="form.aliases" :placeholder="`www.${site.domain}`" class="w-full font-mono" :disabled="!isAdmin" />
      </UFormField>
      <UFormField v-if="!isProxy" :label="t('address.publicRoot')" :help="t('address.publicRootHint')" :error="fieldError('public_root')">
        <UFieldGroup class="w-full max-w-md">
          <UBadge color="neutral" variant="outline" size="lg" class="mono shrink-0 tracking-normal normal-case" label="current/" />
          <UInput v-model="form.public_root" placeholder="public" class="w-full font-mono" :disabled="!canManage" />
        </UFieldGroup>
      </UFormField>
      <UFormField v-else :label="t('site.upstream')" :help="isAdmin ? t('address.upstreamHint') : t('address.adminOnly')" :error="fieldError('proxy_upstream')">
        <UInput v-model="form.proxy_upstream" placeholder="http://127.0.0.1:3000" class="w-full max-w-md font-mono" :disabled="!isAdmin" />
      </UFormField>
    </div>
    <template v-if="canManage" #footer>
      <UButton :label="t('common.apply')" :loading="busy" :disabled="!Object.keys(changes).length" @click="save" />
    </template>
  </PanelSection>
</template>
