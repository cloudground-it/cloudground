<script setup lang="ts">
import type { Deployment, FileEntry, GuardReport } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

const { data: deployments, pending } = useLive(() => api.get<Deployment[]>(`/api/sites/${site.value.id}/deployments`), {
  on: { types: ['deployment.changed', 'task.finished'], sites: () => site.value.id },
  fallbackMs: 8000
})
const expanded = ref<number | null>(null)

// Each status a release moves through (internal/api SPEC §4), with the
// tone of its badge: working, live, kept, or stopped.
const statusColor = {
  pending: 'neutral', guarding: 'primary', building: 'primary', created: 'neutral', promoting: 'primary', verifying: 'primary',
  promoted: 'success', superseded: 'neutral', rolled_back: 'warning', blocked: 'error', failed: 'error', canceled: 'neutral'
} as const
// Only a built release that no job is working on can be promoted (the API answers 409 otherwise).
const promotable = (d: Deployment) => d.status === 'created' || d.status === 'superseded' || (d.status === 'rolled_back' && !d.regressed)
const verdictColor = { pass: 'success', warn: 'warning', block: 'error', inconclusive: 'neutral', unverified: 'warning' } as const
const noteCode = (d: Deployment) => (d.note ?? '').split(':')[0] ?? ''
const knownNotes = ['drift', 'unverified', 'regressed']

function report(d: Deployment): GuardReport | null {
  try {
    return d.guard_json ? JSON.parse(d.guard_json) as GuardReport : null
  } catch {
    return null
  }
}

async function rollback() {
  const ok = await confirm({ title: t('deploy.rollbackTitle'), description: t('deploy.rollbackHint'), confirmLabel: t('deploy.rollback') })
  if (ok) await run(() => api.post(`/api/sites/${site.value.id}/rollback`), t('deploy.rollingBack'))
}
async function promote(d: Deployment) {
  const ok = await confirm({ title: t('deploy.promoteTitle', { id: d.release_id }), confirmLabel: t('deploy.promote') })
  if (ok) await run(() => api.post(`/api/deployments/${d.id}/promote`), t('deploy.promoting'))
}

// A new release from a folder of the site, or from a checkout under the
// site's src/ (the SSH shell's ~/src): POST /api/sites/{id}/deployments
// copies it into a release, which is then activated like any other.
// src/ exists once the site's shell is turned on: without the shell there
// are no checkouts, and asking for src/ answered 404 in the console.
const shellOn = computed(() => !!site.value.config.shell_enabled)
const source = ref<'checkout' | 'folder'>(shellOn.value ? 'checkout' : 'folder')
const sourceItems = computed(() => [
  { label: t('deploy.fromCheckout'), value: 'checkout' },
  { label: t('deploy.fromFolder'), value: 'folder' }
])
const checkouts = ref<string[] | null>(null)
const checkout = ref('')
const folder = ref('')
async function loadCheckouts() {
  if (!canManage.value) return
  if (!shellOn.value) {
    checkouts.value = []
    return
  }
  try {
    const r = await api.get<{ entries: FileEntry[] | null }>(`/api/sites/${site.value.id}/files?path=src`)
    checkouts.value = (r.entries ?? []).filter(e => e.is_dir && !e.name.startsWith('.')).map(e => e.name).sort()
  } catch {
    checkouts.value = []
  }
  if (!checkout.value) checkout.value = checkouts.value[0] ?? ''
}
onMounted(loadCheckouts)
const relDir = computed(() => source.value === 'checkout'
  ? (checkout.value ? `src/${checkout.value}` : '')
  : folder.value.trim().replace(/^\/+|\/+$/g, ''))
const sourceDir = computed(() => relDir.value ? `${site.value.root_path}/${relDir.value}` : '')
async function createRelease() {
  const dir = sourceDir.value
  if (!dir) return
  const ok = await run(() => api.post(`/api/sites/${site.value.id}/deployments`, { source_dir: dir }), t('deploy.creating'))
  if (ok && source.value === 'folder') folder.value = ''
}
</script>

<template>
  <PanelSection :title="t('deploy.releases')" :description="t('deploy.intro')" flush>
    <template #actions>
      <UButton color="neutral" variant="outline" icon="i-cg-rollback" :label="t('deploy.rollback')" :disabled="!canManage" :loading="busy" @click="rollback" />
      <UButton v-if="!site.staging_of" :to="`/sites/${site.id}/staging`" icon="i-cg-git-branch" :label="t('deploy.fromStaging')" />
    </template>
    <USkeleton v-if="pending && !deployments" class="m-4 h-24" />
    <UEmpty v-else-if="!deployments?.length" icon="i-cg-release" :title="t('deploy.none')" :description="t('deploy.noneHint')" />
    <ul v-else class="m-0 list-none divide-y divide-default p-0">
      <li v-for="d in deployments" :key="d.id" class="px-4 py-3 sm:px-5">
        <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
          <span class="mono text-sm font-semibold text-highlighted">{{ d.release_id }}</span>
          <UBadge :color="statusColor[d.status] ?? 'neutral'" variant="subtle" :label="t(`deploy.status.${d.status}`, d.status)" />
          <UBadge v-if="d.regressed" color="error" variant="outline" :label="t('deploy.regressed')" />
          <UBadge v-if="noteCode(d)" color="warning" variant="soft" icon="i-cg-alert-triangle" :label="knownNotes.includes(noteCode(d)) ? t(`deploy.note.${noteCode(d)}`) : (d.note ?? '')" class="max-w-full whitespace-normal" />
          <span class="ms-auto text-xs whitespace-nowrap text-muted" :title="fmtDateTime(d.created_at, locale)">{{ fmtAgo(d.created_at, locale) }}</span>
          <UButton
            v-if="report(d)"
            size="sm"
            :color="verdictColor[report(d)!.verdict] ?? 'neutral'"
            variant="soft"
            :label="`Guard: ${t(`guard.verdict.${report(d)!.verdict}`)}`"
            :trailing-icon="expanded === d.id ? 'i-cg-chevron-up' : 'i-cg-chevron-down'"
            :aria-expanded="expanded === d.id"
            @click="expanded = expanded === d.id ? null : d.id"
          />
          <UButton v-if="canManage && promotable(d)" size="sm" color="neutral" variant="outline" :label="t('deploy.promote')" @click="promote(d)" />
        </div>
        <div v-if="expanded === d.id && report(d)" class="mt-3 bg-elevated p-4">
          <GuardReport :report="report(d)!" />
        </div>
      </li>
    </ul>
  </PanelSection>

  <PanelSection v-if="canManage" :title="t('deploy.newRelease')" :description="t('deploy.newReleaseHint')">
    <div class="flex max-w-2xl flex-col gap-4">
      <URadioGroup v-model="source" :items="sourceItems" orientation="horizontal" />
      <UFormField v-if="source === 'checkout'" :label="t('deploy.checkout')" :help="checkouts && !checkouts.length ? t('deploy.noCheckouts') : t('deploy.checkoutHint')">
        <USelect v-model="checkout" :items="checkouts ?? []" :placeholder="t('deploy.noCheckoutsShort')" class="w-full max-w-md font-mono" :disabled="!checkouts?.length" />
      </UFormField>
      <UFormField v-else :label="t('deploy.folder')" :help="t('deploy.folderHint')">
        <UFieldGroup class="w-full">
          <UBadge color="neutral" variant="outline" size="lg" class="mono max-w-[45%] shrink-0 truncate" :label="`${site.root_path}/`" />
          <UInput v-model="folder" class="min-w-0 flex-1 font-mono" placeholder="shared/build" />
        </UFieldGroup>
      </UFormField>
    </div>
    <template #footer>
      <UButton icon="i-cg-release" :label="t('deploy.create')" :disabled="!sourceDir" :loading="busy" @click="createRelease" />
      <span v-if="sourceDir" class="mono min-w-0 text-xs [overflow-wrap:anywhere] text-muted">{{ sourceDir }}</span>
    </template>
  </PanelSection>
</template>
