<script setup lang="ts">
import type { StagingDiff } from '~/types'

const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site, staging, refresh } = useCurrentSite()

if (site.value.staging_of) await navigateTo(`/sites/${site.value.id}`)

const creds = ref<{ url: string, user: string, password: string } | null>(null)
const showPassword = ref(false)
const diff = ref<StagingDiff | null>(null)
const diffLoading = ref(false)
const selected = ref<string[]>([])
const extraPath = ref('')
const probePaths = ref('/')
const tables = ref<{ name: string, protected: boolean }[] | null>(null)
const selectedTables = ref<string[]>([])

async function loadCredentials() {
  if (!staging.value || !canManage.value) return
  try {
    creds.value = await api.get(`/api/sites/${site.value.id}/staging/credentials`)
  } catch {
    creds.value = null
  }
}
// While the copy is being cloned its tree is partial and a diff would list
// most of production as removed: the API answers 409 then, and the card
// says the clone is running instead.
const cloning = ref(false)
async function loadDiff() {
  if (staging.value?.status === 'provisioning') {
    cloning.value = true
    diff.value = null
    return
  }
  diffLoading.value = true
  try {
    diff.value = await api.get<StagingDiff>(`/api/sites/${site.value.id}/staging/diff`)
    cloning.value = false
  } catch (e) {
    if (isApiError(e) && e.status === 409) {
      cloning.value = true
      diff.value = null
    } else {
      useToast().add({ title: t('common.failed'), description: errorMessage(e), color: 'error' })
    }
  } finally {
    diffLoading.value = false
  }
}
async function loadTables() {
  try {
    tables.value = (await api.get<{ tables: { name: string, protected: boolean }[] }>(`/api/sites/${site.value.id}/staging/tables`)).tables ?? []
  } catch {
    tables.value = []
  }
}
watch(staging, (s) => {
  if (s) {
    void loadCredentials()
    void loadDiff()
    void loadTables()
  }
}, { immediate: true })

const changes = computed(() => [
  ...(diff.value?.added ?? []).map(path => ({ path, kind: 'added' as const })),
  ...(diff.value?.modified ?? []).map(path => ({ path, kind: 'modified' as const })),
  ...(diff.value?.removed ?? []).map(path => ({ path, kind: 'removed' as const }))
])
const kindColor = { added: 'success', modified: 'warning', removed: 'error' } as const

function toggle(path: string, on: boolean | 'indeterminate') {
  selected.value = on === true ? [...new Set([...selected.value, path])] : selected.value.filter(p => p !== path)
}
function addExtra() {
  const p = extraPath.value.trim().replace(/^\/+|\/+$/g, '')
  if (p) selected.value = [...new Set([...selected.value, p])]
  extraPath.value = ''
}

const paths = () => probePaths.value.split(/[\s,]+/).map(p => p.trim()).filter(Boolean)

function create() {
  void run(() => api.post(`/api/sites/${site.value.id}/staging`), t('staging.cloneStarted')).then(refresh)
}
async function pushAll() {
  const ok = await confirm({ title: t('staging.pushTitle'), description: t('staging.pushHint'), confirmLabel: t('staging.push') })
  if (ok) await run(() => api.post(`/api/sites/${site.value.id}/safe-push`, { paths: paths() }), t('staging.pushStarted'))
}
async function pushSelected() {
  const ok = await confirm({ title: t('staging.pushSelectedTitle', { n: selected.value.length }), description: selected.value.join('\n'), confirmLabel: t('staging.push') })
  if (ok) await run(() => api.post(`/api/sites/${site.value.id}/safe-push`, { paths: paths(), only: selected.value }), t('staging.pushStarted'))
}
async function pushTables() {
  const ok = await confirm({ title: t('staging.tablesTitle', { n: selectedTables.value.length }), description: t('staging.tablesHint'), danger: true, typed: site.value.domain, confirmLabel: t('staging.push') })
  if (ok) {
    await run(() => api.post(`/api/sites/${site.value.id}/staging/database`, { tables: selectedTables.value, confirm: site.value.domain }), t('staging.tablesStarted'))
    selectedTables.value = []
  }
}
async function recreate() {
  const ok = await confirm({ title: t('staging.recreateTitle'), description: t('staging.recreateHint'), danger: true, typed: staging.value?.domain, confirmLabel: t('staging.recreate') })
  if (ok && await run(() => api.post(`/api/sites/${site.value.id}/staging/recreate`), t('staging.recreateStarted'))) {
    cloning.value = true
    diff.value = null
    selected.value = []
  }
}
</script>

<template>
  <PanelSection v-if="!staging">
    <UEmpty
      icon="i-cg-staging"
      :title="t('staging.noneTitle')"
      :description="t('staging.noneHint')"
      :actions="canManage ? [{ label: t('staging.create'), icon: 'i-cg-copy', loading: busy, onClick: create }] : []"
    />
  </PanelSection>

  <template v-else>
    <PanelSection :title="staging.domain" :description="t('staging.protected')" icon="i-cg-staging">
      <template #actions>
        <UButton :to="`https://${staging.domain}/`" target="_blank" trailing-icon="i-cg-external-link" color="neutral" variant="outline" :label="t('site.visit')" />
        <UButton :to="`/sites/${staging.id}`" color="neutral" variant="outline" :label="t('staging.manage')" />
      </template>
      <dl v-if="creds" class="m-0 grid grid-cols-1 sm:grid-cols-[minmax(8rem,max-content)_1fr]">
        <dt class="pt-2.5 text-[13px] text-muted sm:border-b sm:border-default sm:py-2.5 sm:pe-6">
          {{ t('staging.user') }}
        </dt>
        <dd class="mono m-0 flex items-center gap-1.5 border-b border-default pb-2.5 text-[13px] text-highlighted sm:py-2.5">
          {{ creds.user }}<CopyButton :value="creds.user" />
        </dd>
        <dt class="pt-2.5 text-[13px] text-muted sm:py-2.5 sm:pe-6">
          {{ t('common.password') }}
        </dt>
        <dd class="mono m-0 flex items-center gap-1.5 pb-2.5 text-[13px] text-highlighted sm:py-2.5">
          <span class="truncate">{{ showPassword ? creds.password : '••••••••••••' }}</span>
          <UButton size="xs" color="neutral" variant="ghost" square :icon="showPassword ? 'i-cg-eye-off' : 'i-cg-eye'" :aria-label="t('common.show')" @click="showPassword = !showPassword" />
          <CopyButton :value="creds.password" />
        </dd>
      </dl>
    </PanelSection>

    <PanelSection :title="t('staging.safePush')" :description="t('staging.safePushHint')">
      <UFormField :label="t('staging.probePaths')" :help="t('staging.probePathsHint')" class="max-w-2xl">
        <UInput v-model="probePaths" class="w-full font-mono" placeholder="/ /shop/ /blog/" />
      </UFormField>
      <template #footer>
        <UButton :label="t('staging.pushEverything')" icon="i-cg-upload" :disabled="!canManage" :loading="busy" @click="pushAll" />
      </template>
    </PanelSection>

    <PanelSection :title="t('staging.diff')" :description="cloning ? t('staging.cloning') : diff ? t('staging.diffSummary', { scanned: diff.scanned, n: changes.length }) : t('staging.diffHint')">
      <template #actions>
        <UButton icon="i-cg-refresh" color="neutral" variant="outline" size="sm" :loading="diffLoading" :label="t('common.refresh')" @click="loadDiff" />
      </template>
      <UAlert v-if="diff?.truncated" color="warning" variant="subtle" :description="t('staging.diffTruncated')" />
      <p v-if="diff && !changes.length" class="m-0 text-sm text-muted">
        {{ t('staging.noChanges') }}
      </p>
      <ul v-else-if="changes.length" class="m-0 max-h-96 list-none divide-y divide-default overflow-y-auto border-y border-default p-0">
        <li v-for="c in changes" :key="c.kind + c.path" class="flex items-center gap-3 py-1.5 text-sm">
          <UCheckbox :model-value="selected.includes(c.path)" :disabled="c.kind === 'removed' || !canManage" :aria-label="c.path" @update:model-value="v => toggle(c.path, v)" />
          <UBadge :color="kindColor[c.kind]" variant="subtle" size="sm" :label="t(`staging.kind.${c.kind}`)" class="w-24 justify-center" />
          <span class="min-w-0 flex-1 truncate font-mono text-xs">{{ c.path }}</span>
        </li>
      </ul>
      <UFormField :label="t('staging.addFolder')" class="max-w-2xl">
        <UFieldGroup class="w-full">
          <UInput v-model="extraPath" class="min-w-0 flex-1 font-mono" placeholder="wp-content/themes/my-theme" @keydown.enter.prevent="addExtra" />
          <UButton color="neutral" variant="outline" icon="i-cg-plus" :label="t('common.add')" @click="addExtra" />
        </UFieldGroup>
      </UFormField>
      <div v-if="selected.length" class="flex flex-wrap gap-1.5">
        <UBadge v-for="p in selected" :key="p" color="primary" variant="subtle" class="font-mono normal-case tracking-normal">
          {{ p }}
          <UButton size="xs" color="primary" variant="link" icon="i-cg-x" square :aria-label="t('common.remove')" @click="toggle(p, false)" />
        </UBadge>
      </div>
      <template #footer>
        <UButton :label="t('staging.pushSelected', { n: selected.length })" icon="i-cg-upload" :disabled="!canManage || !selected.length" :loading="busy" @click="pushSelected" />
      </template>
    </PanelSection>

    <PanelSection v-if="site.config.database.enabled" :title="t('staging.tables')" :description="t('staging.tablesCard')">
      <USkeleton v-if="!tables" class="h-24" />
      <div v-else class="grid max-h-80 gap-1 overflow-y-auto sm:grid-cols-2 xl:grid-cols-3">
        <UCheckbox
          v-for="tb in tables"
          :key="tb.name"
          :model-value="selectedTables.includes(tb.name)"
          :disabled="tb.protected || !canManage"
          :label="tb.name"
          :description="tb.protected ? t('staging.protectedTable') : undefined"
          :ui="{ label: 'font-mono text-xs' }"
          @update:model-value="v => selectedTables = v === true ? [...selectedTables, tb.name] : selectedTables.filter(n => n !== tb.name)"
        />
      </div>
      <template #footer>
        <UButton color="warning" :label="t('staging.pushTables', { n: selectedTables.length })" icon="i-cg-export" :disabled="!canManage || !selectedTables.length" :loading="busy" @click="pushTables" />
      </template>
    </PanelSection>

    <PanelSection :title="t('tasks.activity')" flush>
      <TaskFeed :site-id="site.id" />
    </PanelSection>

    <PanelSection :title="t('staging.recreate')" :description="t('staging.recreateCard')">
      <template #footer>
        <UButton color="error" variant="outline" icon="i-cg-refresh" :label="t('staging.recreate')" :disabled="!canManage" :loading="busy" @click="recreate" />
      </template>
    </PanelSection>
  </template>
</template>
