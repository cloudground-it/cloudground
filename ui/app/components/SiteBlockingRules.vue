<script setup lang="ts">
/**
 * Each site's refused visitors (#25), on the Blocking page beside the
 * server's block list: addresses, user agents and countries a site answers
 * 403 (PUT /api/sites/{id}/blocking, live once saved).
 */
import type { Site, SiteBlocking } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { isAdmin } = useAuth()
const { busy, run } = useAction()

const { data: sites, refresh } = useLoad(() => api.get<Site[]>('/api/sites'))
const ruled = computed(() => (sites.value ?? []).filter(s => hasRules(s.config.blocking)))
const unruled = computed(() => (sites.value ?? []).filter(s => !hasRules(s.config.blocking)))
function hasRules(b?: SiteBlocking) {
  return !!(b && ((b.addresses?.length ?? 0) + (b.user_agents?.length ?? 0) + (b.countries?.length ?? 0)))
}

// Presets: crawlers people commonly turn away. Plain substrings of the
// User-Agent, matched in any case.
const presets: Record<string, string[]> = {
  ai: ['GPTBot', 'ChatGPT-User', 'OAI-SearchBot', 'CCBot', 'ClaudeBot', 'anthropic-ai', 'Google-Extended', 'PerplexityBot', 'Bytespider', 'Amazonbot', 'meta-externalagent'],
  seo: ['AhrefsBot', 'SemrushBot', 'MJ12bot', 'DotBot', 'BLEXBot', 'DataForSeoBot', 'serpstatbot']
}

const editing = ref(false)
const form = reactive({ siteId: 0, addresses: '', userAgents: '', countries: [] as string[], onlyCountries: false })
const lines = (s: string) => s.split('\n').map(l => l.trim()).filter(Boolean)
function open(s?: Site) {
  const b = s?.config.blocking ?? {}
  Object.assign(form, {
    siteId: s?.id ?? unruled.value[0]?.id ?? 0,
    addresses: (b.addresses ?? []).join('\n'),
    userAgents: (b.user_agents ?? []).join('\n'),
    countries: [...(b.countries ?? [])],
    onlyCountries: !!b.allow_countries_only
  })
  editing.value = true
}
function addPreset(name: string) {
  const have = new Set(lines(form.userAgents).map(u => u.toLowerCase()))
  form.userAgents = [...lines(form.userAgents), ...presets[name]!.filter(u => !have.has(u.toLowerCase()))].join('\n')
}
const siteItems = computed(() => (sites.value ?? []).map(s => ({ label: s.domain, value: s.id })))
const countryItems = computed(() => COUNTRY_CODES
  .map(c => ({ label: `${countryName(c, locale.value)} (${c})`, value: c }))
  .sort((a, b) => a.label.localeCompare(b.label, locale.value)))
const rulesOK = computed(() => form.siteId > 0 && (!form.onlyCountries || form.countries.length > 0))

async function save(clear = false) {
  const body = clear
    ? {}
    : { addresses: lines(form.addresses), user_agents: lines(form.userAgents), countries: form.countries, allow_countries_only: form.onlyCountries && form.countries.length > 0 }
  const domain = sites.value?.find(s => s.id === form.siteId)?.domain ?? ''
  if (await run(() => api.put(`/api/sites/${form.siteId}/blocking`, body), t('blocklist.site.saved', { site: domain }))) {
    editing.value = false
    await refresh()
  }
}

function summary(b: SiteBlocking): string {
  const parts: string[] = []
  if (b.addresses?.length) parts.push(t('blocklist.site.nAddresses', b.addresses.length))
  if (b.user_agents?.length) parts.push(t('blocklist.site.nAgents', b.user_agents.length))
  if (b.countries?.length) {
    const names = b.countries.map(c => countryName(c, locale.value)).join(', ')
    parts.push(b.allow_countries_only ? t('blocklist.site.onlyCountries', { countries: names }) : t('blocklist.site.countries', { countries: names }))
  }
  return parts.join(' · ')
}
const columns = computed(() => [
  { id: 'site', header: t('blocklist.site.site') },
  { id: 'rules', header: t('blocklist.site.rules') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <PanelSection :title="t('blocklist.site.title')" :description="t('blocklist.site.hint')" flush>
    <template v-if="isAdmin && unruled.length" #actions>
      <UButton icon="i-cg-plus" :label="t('blocklist.site.add')" @click="open()" />
    </template>
    <USkeleton v-if="!sites" class="m-4 h-16" />
    <UEmpty v-else-if="!ruled.length" icon="i-cg-sites" :description="t('blocklist.site.none')" />
    <UTable v-else :data="ruled" :columns="columns">
      <template #site-cell="{ row: { original: s } }">
        <NuxtLink :to="`/sites/${s.id}`" class="mono text-[13px] text-highlighted hover:underline">{{ s.domain }}</NuxtLink>
      </template>
      <template #rules-cell="{ row: { original: s } }">
        <span class="text-[13px]">{{ summary(s.config.blocking ?? {}) }}</span>
      </template>
      <template #actions-cell="{ row: { original: s } }">
        <UButton v-if="isAdmin" size="sm" color="neutral" variant="ghost" icon="i-cg-edit" :label="t('blocklist.site.edit')" @click="open(s)" />
      </template>
    </UTable>
  </PanelSection>

  <UModal v-model:open="editing" :title="t('blocklist.site.dialogTitle')" :description="t('blocklist.site.dialogHint')" :ui="{ content: 'sm:max-w-2xl' }">
    <template #body>
      <div class="flex flex-col gap-4">
        <UFormField :label="t('blocklist.site.site')">
          <USelect v-model="form.siteId" :items="siteItems" class="w-full" />
        </UFormField>
        <UFormField :label="t('blocklist.site.addresses')" :help="t('blocklist.site.addressesHelp')">
          <UTextarea v-model="form.addresses" :rows="3" class="w-full font-mono text-xs" placeholder="203.0.113.0/24" />
        </UFormField>
        <UFormField :label="t('blocklist.site.agents')" :help="t('blocklist.site.agentsHelp')">
          <UTextarea v-model="form.userAgents" :rows="3" class="w-full font-mono text-xs" placeholder="GPTBot" />
          <div class="mt-2 flex flex-wrap gap-2">
            <UButton size="xs" color="neutral" variant="outline" icon="i-cg-plus" :label="t('blocklist.site.presetAI')" @click="addPreset('ai')" />
            <UButton size="xs" color="neutral" variant="outline" icon="i-cg-plus" :label="t('blocklist.site.presetSEO')" @click="addPreset('seo')" />
          </div>
        </UFormField>
        <UFormField :label="t('blocklist.site.countriesLabel')">
          <USelectMenu v-model="form.countries" :items="countryItems" value-key="value" multiple class="w-full" :placeholder="t('blocklist.site.countriesNone')" />
        </UFormField>
        <USwitch v-model="form.onlyCountries" :label="t('blocklist.site.onlyLabel')" :description="t('blocklist.site.onlyHint')" :disabled="!form.countries.length" />
        <p class="m-0 text-[12px] text-muted">
          {{ t('blocklist.site.attribution') }}
          <a href="https://db-ip.com" target="_blank" rel="noopener" class="underline">IP Geolocation by DB-IP</a>
          (<a href="https://creativecommons.org/licenses/by/4.0/" target="_blank" rel="noopener" class="underline">CC BY 4.0</a>)
        </p>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-between gap-2">
        <UButton color="error" variant="ghost" :label="t('blocklist.site.clear')" :disabled="!form.siteId" :loading="busy" @click="save(true)" />
        <div class="flex gap-2">
          <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="editing = false" />
          <UButton icon="i-cg-lock" :label="t('common.save')" :disabled="!rulesOK" :loading="busy" @click="save()" />
        </div>
      </div>
    </template>
  </UModal>
</template>
