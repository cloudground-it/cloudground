<script setup lang="ts">
/**
 * The consent page of the remote MCP server (api SPEC §16.4): an assistant
 * asked to connect; the person decides what it may do and confirms it is
 * them. Reading is always granted; writing per area and per site.
 */
const { t } = useI18n()
const api = useApi()
const route = useRoute()
const proof = useProof()
useHead({ title: () => t('assistants.connectTitle') })

interface ConnectRequest {
  client_name: string
  redirect_host: string
  requested_scope: string
  areas: string[]
  sites: { id: number, domain: string }[]
  two_factor: boolean
}
const id = computed(() => String(route.query.request ?? ''))
const { data: req, error: loadError } = useLoad(() => api.get<ConnectRequest>(`/api/mcp/requests/${encodeURIComponent(id.value)}`))

const areas = reactive<Record<string, boolean>>({})
const sites = ref<number[]>([])
const siteItems = computed(() => (req.value?.sites ?? []).map(s => ({ label: s.domain, value: s.id })))
const anyArea = computed(() => Object.values(areas).some(Boolean))
const busy = ref(false)
const error = ref<string | null>(null)

async function allow() {
  const p = await proof({ title: t('assistants.allowTitle', { name: req.value?.client_name ?? '' }), confirmLabel: t('assistants.allow') })
  if (!p) return
  const write: Record<string, number[]> = {}
  if (sites.value.length) {
    for (const [area, on] of Object.entries(areas)) if (on) write[area] = sites.value
  }
  busy.value = true
  error.value = null
  try {
    const r = await api.post<{ redirect: string }>(`/api/mcp/requests/${encodeURIComponent(id.value)}/approve`, { write, ...p })
    window.location.href = r.redirect
  } catch (e) {
    error.value = errorMessage(e)
    busy.value = false
  }
}

async function deny() {
  busy.value = true
  try {
    const r = await api.post<{ redirect: string }>(`/api/mcp/requests/${encodeURIComponent(id.value)}/deny`, {})
    window.location.href = r.redirect
  } catch (e) {
    error.value = errorMessage(e)
    busy.value = false
  }
}
</script>

<template>
  <UDashboardPanel id="assistants-connect">
    <template #header>
      <PageNavbar :title="t('assistants.connectTitle')" />
    </template>
    <template #body>
      <UContainer class="flex max-w-2xl flex-col gap-6">
        <UAlert v-if="loadError" color="warning" variant="subtle" icon="i-cg-alert-circle" :title="t('assistants.requestGone')" :description="loadError" />
        <template v-else-if="req">
          <UPageHeader :title="t('assistants.connectHeading', { name: req.client_name })" :description="t('assistants.connectHint', { host: req.redirect_host })" />
          <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />
          <UAlert v-if="!req.two_factor" color="warning" variant="subtle" icon="i-cg-lock" :title="t('assistants.needTwoFactor')">
            <template #description>
              <ULink to="/security" class="underline">
                {{ t('nav.security') }}
              </ULink>
            </template>
          </UAlert>

          <PanelSection :title="t('assistants.readTitle')" :description="t('assistants.readHint')" />

          <PanelSection :title="t('assistants.writeTitle')" :description="req.sites.length ? t('assistants.writeHint') : t('assistants.writeNone')">
            <template v-if="req.sites.length">
              <div class="flex flex-col gap-2">
                <UCheckbox v-for="a in req.areas" :key="a" v-model="areas[a]" :label="t(`assistants.area_${a}`)" :description="t(`assistants.areaHint_${a}`)" />
              </div>
              <UFormField v-if="anyArea" :label="t('assistants.onSites')">
                <USelectMenu v-model="sites" :items="siteItems" value-key="value" multiple class="w-full" :placeholder="t('users.pickSites')" />
              </UFormField>
              <p class="text-xs text-muted">
                {{ t('assistants.destructiveHint') }}
              </p>
            </template>
          </PanelSection>

          <div class="flex flex-wrap justify-end gap-2">
            <UButton color="neutral" variant="outline" :label="t('assistants.deny')" :disabled="busy" @click="deny" />
            <UButton color="primary" :label="t('assistants.allow')" :loading="busy" :disabled="!req.two_factor || (anyArea && !sites.length)" @click="allow" />
          </div>
        </template>
      </UContainer>
    </template>
  </UDashboardPanel>
</template>
