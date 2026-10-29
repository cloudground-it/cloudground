<script setup lang="ts">
/**
 * Restore from another server (issue 193): the sites the backup repository
 * holds that this panel has none of (GET /api/backups/foreign), and the
 * restore of one (POST /api/backups/foreign/restore), which creates the
 * site and restores its backup whole. A backup from before 1.0 carries no
 * description of the site: its type and PHP version are asked for.
 */
definePageMeta({
  middleware: [() => {
    if (!useAuth().isAdmin.value) return navigateTo('/sites', { replace: true })
  }]
})

interface ForeignSite {
  domain: string
  hostname: string
  snapshot: string
  time: string
  snapshots: number
  manifest: boolean
}

const { t, locale } = useI18n()
const api = useApi()
const { busy, run } = useAction()
const caps = useCapabilities()
useHead({ title: () => t('backup.foreign.title') })

const { data, error, pending, refresh } = useLoad(() => api.get<{ sites: ForeignSite[] }>('/api/backups/foreign'))
const sites = computed(() => data.value?.sites ?? [])

const target = ref<ForeignSite | null>(null)
const typed = ref('')
const type = ref('wordpress')
const php = ref('')
const TYPES = ['wordpress', 'woocommerce', 'static', 'php', 'laravel', 'node', 'python'] as const
const typeItems = computed(() => TYPES.map(v => ({ value: v, label: t(`site.type.${v}`) })))
const needsPHP = computed(() => !['static', 'node', 'python'].includes(type.value))
const phpItems = computed(() => (caps.value?.php_versions ?? []).map(v => ({ value: v, label: `PHP ${v}` })))
function open(s: ForeignSite) {
  target.value = s
  typed.value = ''
  type.value = 'wordpress'
  php.value = caps.value?.php_versions?.[0] ?? ''
}
async function restore() {
  const s = target.value
  if (!s) return
  const body: Record<string, string> = { snapshot: s.snapshot, confirm: typed.value.trim() }
  if (!s.manifest) {
    body.type = type.value
    if (needsPHP.value && php.value) body.php_version = php.value
  }
  if (await run(() => api.post('/api/backups/foreign/restore', body), t('backup.foreign.started'))) {
    target.value = null
    await navigateTo('/sites')
  }
}

const columns = computed(() => [
  { id: 'site', header: t('cg.col.site') },
  { id: 'server', header: t('backup.foreign.server'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'last', header: t('backup.last') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
</script>

<template>
  <UDashboardPanel id="sites-restore">
    <template #header>
      <PageNavbar :title="t('backup.foreign.title')" />
    </template>

    <template #body>
      <UContainer class="flex flex-col gap-6">
        <UPageHeader :title="t('backup.foreign.title')" :description="`${t('backup.foreign.hint')} ${t('backup.foreign.limits')}`">
          <template #links>
            <UButton color="neutral" variant="outline" icon="i-cg-refresh" :label="t('backup.foreign.reload')" :loading="pending" @click="refresh()" />
          </template>
        </UPageHeader>

        <UAlert v-if="error" color="error" variant="subtle" icon="i-cg-alert-circle" :title="error" />

        <PanelSection flush :label="t('backup.foreign.title')">
          <USkeleton v-if="pending && !data" class="m-4 h-24" />
          <UEmpty v-else-if="!sites.length" icon="i-cg-backup" :title="t('backup.foreign.none')" :description="t('backup.foreign.noneHint')"
            :actions="[{ label: t('settings.backups'), icon: 'i-cg-settings', color: 'neutral', variant: 'outline', to: '/settings#backups' }]"
          />
          <UTable v-else :data="sites" :columns="columns">
            <template #site-cell="{ row: { original: s } }">
              <span class="flex min-w-0 flex-col gap-0.5 whitespace-normal">
                <span class="text-[15px] font-semibold break-all text-highlighted">{{ s.domain }}</span>
                <span class="text-[13px] text-muted md:hidden">{{ s.hostname }}</span>
                <span v-if="!s.manifest" class="text-[13px] text-muted">{{ t('backup.foreign.noManifest') }}</span>
              </span>
            </template>
            <template #server-cell="{ row: { original: s } }">
              <span class="mono text-[13px]">{{ s.hostname }}</span>
            </template>
            <template #last-cell="{ row: { original: s } }">
              <span class="flex flex-col gap-0.5">
                <span>{{ fmtDateTime(s.time, locale) }}</span>
                <span class="text-[13px] text-muted">{{ t('backup.foreign.count', { n: fmtNumber(s.snapshots) }) }}</span>
              </span>
            </template>
            <template #actions-cell="{ row: { original: s } }">
              <UButton size="sm" color="neutral" variant="outline" icon="i-cg-restore" :label="t('backup.restore')" @click="open(s)" />
            </template>
          </UTable>
        </PanelSection>
      </UContainer>

      <UModal :open="!!target" :title="t('backup.foreign.restoreTitle', { domain: target?.domain ?? '' })" :description="t('backup.foreign.restoreHint')" @update:open="v => { if (!v) target = null }">
        <template #body>
          <div class="flex flex-col gap-4">
            <template v-if="target && !target.manifest">
              <UAlert color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('backup.foreign.askType')" />
              <UFormField :label="t('site.typeLabel')" required>
                <USelect v-model="type" :items="typeItems" class="w-full" />
              </UFormField>
              <UFormField v-if="needsPHP" :label="t('cg.col.php')" required>
                <USelect v-model="php" :items="phpItems" class="w-full" />
              </UFormField>
            </template>
            <UFormField :label="t('backup.foreign.confirm', { domain: target?.domain ?? '' })" required>
              <UInput v-model="typed" class="w-full font-mono" autocomplete="off" :placeholder="target?.domain" />
            </UFormField>
          </div>
        </template>
        <template #footer>
          <div class="flex w-full justify-end gap-2">
            <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="target = null" />
            <UButton :label="t('backup.foreign.start')" :loading="busy" :disabled="typed.trim() !== target?.domain" @click="restore" />
          </div>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
