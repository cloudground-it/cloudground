<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Backup } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { canManage, isAdmin, isOperator } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site, refresh } = useCurrentSite()

const { data: backups, pending } = useLive(() => api.get<Backup[]>(`/api/sites/${site.value.id}/backups`), {
  on: { types: ['backup.', 'task.finished'], sites: () => site.value.id },
  fallbackMs: 8000
})
const latest = computed(() => backups.value?.[0])
const verifyColor = { pending: 'neutral', passed: 'success', failed: 'error' } as const

// The schedule (PUT /api/sites/{id}/backups/policy). "Off" is the policy
// switched off; the schedule it had is kept for when it is switched on.
type Frequency = 'off' | 'hourly' | 'daily' | 'weekly'
const stored = computed(() => site.value.config.backups)
// hour -1 is no fixed hour: a day or a week after the last backup.
const form = reactive({ frequency: 'off' as Frequency, retention: 0, hour: -1 })
function load() {
  const p = stored.value
  form.frequency = p.enabled ? ((p.schedule || 'daily') as Frequency) : 'off'
  form.retention = p.retention_days || 0
  form.hour = p.hour ?? -1
}
load()
watch(stored, load, { deep: true })
const frequencyItems = computed(() => (['off', 'hourly', 'daily', 'weekly'] as const).map(v => ({ label: t(`backup.frequency.${v}`), value: v })))
// The hour of a daily or weekly backup, in the panel's time zone.
const caps = useCapabilities()
const hasHour = computed(() => form.frequency === 'daily' || form.frequency === 'weekly')
const hourItems = computed(() => [
  { label: t('backup.hourAny'), value: -1 },
  ...Array.from({ length: 24 }, (_, h) => ({ label: `${String(h).padStart(2, '0')}:00`, value: h }))
])
const policyChanged = computed(() => {
  const p = stored.value
  const enabled = form.frequency !== 'off'
  return enabled !== p.enabled || (enabled && form.frequency !== (p.schedule || 'daily')) || form.retention !== (p.retention_days || 0) ||
    (hasHour.value && form.hour !== (p.hour ?? -1))
})
async function savePolicy() {
  const body: Record<string, unknown> = { enabled: form.frequency !== 'off' }
  if (form.frequency !== 'off') body.schedule = form.frequency
  if (hasHour.value && form.hour !== (stored.value.hour ?? -1)) body.hour = form.hour
  // Only an administrator changes the window (a shorter one deletes snapshots).
  if (form.retention !== (stored.value.retention_days || 0)) body.retention_days = form.retention
  if (await run(() => api.put(`/api/sites/${site.value.id}/backups/policy`, body), t('backup.policySaved'))) await refresh()
}

// Where the snapshots go: the server's repository (Settings). An operator
// cannot read the settings; the newest snapshot says where it went.
const repository = ref<string | null>(null)
const repoKnown = ref(false)
onMounted(async () => {
  if (!isOperator.value) {
    try {
      const s = await api.get<Record<string, string>>('/api/settings')
      repository.value = s.backup_repository || ''
      repoKnown.value = true
    } catch {
      // Left to the newest snapshot below.
    }
  }
})
const destination = computed(() => repoKnown.value ? repository.value : (latest.value?.repository ?? null))
const destinationRows = computed(() => [
  { k: t('settings.repository'), v: destination.value ?? '—', mono: true },
  { k: t('backup.contents'), v: site.value.config.database.enabled ? t('backup.contentsDb') : t('backup.contentsFiles') },
  { k: t('backup.test'), v: t('backup.everySnapshot') }
])

// How long snapshots are kept, in words: the API's retention_days (0 keeps
// every snapshot). A value set elsewhere (cgctl) stays selectable.
const RETENTION = [7, 14, 30, 60, 90, 180, 365]
const retentionItems = computed(() => {
  const days = [...RETENTION]
  if (form.retention && !days.includes(form.retention)) days.push(form.retention)
  return [
    ...days.sort((x, y) => x - y).map(n => ({ label: t('ux.backup.keepDays', n), value: n })),
    { label: t('ux.backup.keepAll'), value: 0 }
  ]
})

const snapColumns = computed(() => [
  { id: 'snapshot', header: 'Snapshot' },
  { id: 'created', header: t('backup.created'), meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
  { id: 'size', header: t('backup.size'), meta: { class: { th: 'hidden sm:table-cell text-end', td: 'hidden sm:table-cell text-end' } } },
  { id: 'test', header: t('backup.test') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
const snapMenu = (b: Backup): DropdownMenuItem[][] => {
  const out: DropdownMenuItem[][] = [[{ label: t('backup.testRestore'), icon: 'i-cg-verified', disabled: !canManage.value, onSelect: () => void verify(b) }]]
  if (isAdmin.value) out.push([{ label: deletable(b) ? t('common.delete') : t('backup.deleteKept'), icon: 'i-cg-trash', color: 'error', disabled: !deletable(b), onSelect: () => void remove(b) }])
  return out
}

const restoreTarget = ref<Backup | null>(null)
const restoreMode = ref<'full' | 'files' | 'database'>('full')
const modeItems = computed(() => (['full', 'files', 'database'] as const).map(v => ({ label: t(`backup.mode.${v}`), value: v })))

// The databases a restore may put back: the snapshot's, or, for a backup
// recorded before they were kept, the site's (the agent then restores those
// the snapshot holds). All are checked; a subset is sent as `databases`.
const restoreDbOptions = ref<string[]>([])
const restoreDbs = ref<string[]>([])
const restoresDbs = computed(() => restoreMode.value !== 'files' && restoreDbOptions.value.length > 0)
async function openRestore(b: Backup) {
  restoreMode.value = 'full'
  let names = b.databases ?? null
  if (names === null) {
    try {
      const list = await api.get<{ databases: { name: string }[] }>(`/api/sites/${site.value.id}/databases`)
      names = list.databases.map(d => d.name)
    } catch {
      names = []
    }
  }
  restoreDbOptions.value = names
  restoreDbs.value = [...names]
  restoreTarget.value = b
}

const backupNow = () => run(() => api.post(`/api/sites/${site.value.id}/backups`), t('backup.started'))
const verify = (b: Backup) => run(() => api.post(`/api/backups/${b.id}/verify`), t('backup.verifyStarted'))

// The API keeps the newest backup whose restore test passed, per repository,
// and every one not tested yet (409 otherwise). The list is newest first.
const keptIds = computed(() => {
  const kept = new Map<string, number>()
  for (const b of backups.value ?? []) {
    if (b.verify_status === 'passed' && !kept.has(b.repository)) kept.set(b.repository, b.id)
  }
  return new Set(kept.values())
})
const deletable = (b: Backup) => b.verify_status !== 'pending' && !keptIds.value.has(b.id)
async function remove(b: Backup) {
  const ok = await confirm({ title: t('backup.deleteTitle', { snapshot: b.snapshot_id.slice(0, 12) }), description: t('backup.deleteHint'), danger: true, confirmLabel: t('common.delete') })
  if (ok) await run(() => api.del(`/api/backups/${b.id}`), t('backup.deleteStarted'))
}

async function restore() {
  const b = restoreTarget.value
  if (!b) return
  restoreTarget.value = null
  const ok = await confirm({ title: t('backup.restoreTitle', { snapshot: b.snapshot_id.slice(0, 12) }), description: t('backup.restoreHint'), danger: true, typed: site.value.domain, confirmLabel: t('backup.restore') })
  const body: Record<string, unknown> = { confirm: site.value.domain, mode: restoreMode.value }
  if (restoresDbs.value && restoreDbs.value.length < restoreDbOptions.value.length) body.databases = [...restoreDbs.value]
  if (ok) await run(() => api.post(`/api/backups/${b.id}/restore`, body), t('backup.restoreStarted'))
}
</script>

<template>
  <section :aria-label="t('backup.snapshots')" class="grid grid-cols-2 gap-3 lg:grid-cols-4">
    <StatCell :label="t('backup.last')" :value="latest ? fmtWhen(latest.created_at, locale) : t('backup.never')" :note="latest ? `${fmtDateTime(latest.created_at, locale)} · ${fmtBytes(latest.size_bytes)}` : '—'" />
    <StatCell
      :label="t('backup.lastTest')"
      :value="latest ? t(`backup.verify.${latest.verify_status}`) : '—'"
      :note="latest?.verified_at ? fmtAgo(latest.verified_at, locale) : '—'"
      :state="latest ? (latest.verify_status === 'passed' ? { label: t('cg.state.ok'), tone: 'ok' } : latest.verify_status === 'failed' ? { label: t('cg.state.err'), tone: 'err' } : { label: t('backup.verify.pending'), tone: 'muted' }) : undefined"
    />
    <StatCell :label="t('backup.estimate')" :value="latest?.restore_estimate_ms ? fmtDuration(latest.restore_estimate_ms) : '—'" :note="latest?.restore_estimate_ms ? t('backup.measured') : '—'" />
    <StatCell :label="t('backup.kept')" :value="stored.retention_days ? fmtNumber(stored.retention_days) : '∞'" :note="stored.retention_days ? t('backup.keptDays') : t('backup.keptAll')" />
  </section>

  <PanelSection :title="t('backup.snapshots')" :description="t('backup.noneHint')" flush>
    <template #actions>
      <UButton icon="i-cg-backup" :label="t('backup.now')" :disabled="!canManage || busy" @click="backupNow" />
    </template>
    <USkeleton v-if="pending && !backups" class="m-4 h-24" />
    <UEmpty v-else-if="!backups?.length" icon="i-cg-backup" :title="t('backup.none')" :description="t('backup.noneHint')" />
    <UTable v-else :data="backups" :columns="snapColumns">
      <template #snapshot-cell="{ row: { original: b } }">
        <span class="mono text-[13px] font-semibold text-highlighted">{{ b.snapshot_id.slice(0, 12) }}</span>
      </template>
      <template #created-cell="{ row: { original: b } }">
        {{ fmtDateTime(b.created_at, locale) }}
      </template>
      <template #size-cell="{ row: { original: b } }">
        <span class="mono">{{ fmtBytes(b.size_bytes) }}</span>
      </template>
      <template #test-cell="{ row: { original: b } }">
        <span class="flex items-center gap-2">
          <UBadge :color="verifyColor[b.verify_status]" variant="subtle" :label="t(`backup.verify.${b.verify_status}`)" />
          <span v-if="b.verified_at" class="text-xs text-muted">{{ b.restore_estimate_ms ? `${fmtDuration(b.restore_estimate_ms)} · ` : '' }}{{ fmtAgo(b.verified_at, locale) }}</span>
        </span>
      </template>
      <template #actions-cell="{ row: { original: b } }">
        <span class="flex items-center justify-end gap-1">
          <UButton size="sm" color="neutral" variant="outline" icon="i-cg-restore" :label="t('backup.restore')" :aria-label="t('backup.restore')" :ui="{ label: 'hidden sm:inline' }" :disabled="!canManage" @click="openRestore(b)" />
          <UDropdownMenu :items="snapMenu(b)" :content="{ align: 'end' }">
            <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-more-horizontal" :aria-label="t('ux.common.moreFor', { name: b.snapshot_id.slice(0, 12) })" />
          </UDropdownMenu>
        </span>
      </template>
    </UTable>
  </PanelSection>

  <div class="grid items-start gap-6 lg:grid-cols-2">
    <PanelSection :title="t('backup.schedule')" :description="site.staging_of ? t('backup.stagingHint') : t('backup.scheduleHint')">
      <div v-if="!site.staging_of" class="grid gap-4 sm:grid-cols-2">
        <UFormField :label="t('backup.frequencyLabel')">
          <USelect v-model="form.frequency" :items="frequencyItems" class="w-full" :disabled="!canManage" />
        </UFormField>
        <UFormField v-if="hasHour" :label="t('backup.hour')" :help="t('backup.hourHelp', { zone: caps?.time_zone || 'UTC' })">
          <USelect v-model="form.hour" :items="hourItems" class="w-full" :disabled="!canManage" />
        </UFormField>
        <UFormField :label="t('backup.retention')" :help="isAdmin ? t('ux.backup.retentionHelp') : t('backup.retentionAdmin')">
          <USelect v-model="form.retention" :items="retentionItems" class="w-full" :disabled="!isAdmin" />
        </UFormField>
      </div>
      <template v-if="canManage && !site.staging_of" #footer>
        <UButton :label="t('common.save')" :loading="busy" :disabled="!policyChanged" @click="savePolicy" />
      </template>
    </PanelSection>

    <PanelSection :title="t('backup.destination')" :description="t('backup.destinationHint')">
      <template v-if="isAdmin" #actions>
        <UButton color="neutral" variant="outline" size="sm" icon="i-cg-settings" :label="t('backup.changeDestination')" to="/settings#backups" />
      </template>
      <UAlert v-if="destination === ''" color="warning" variant="subtle" icon="i-cg-alert-triangle" :title="t('backup.noRepository')" />
      <SummaryList v-else :rows="destinationRows" />
    </PanelSection>
  </div>

  <UModal :open="!!restoreTarget" :title="t('backup.chooseMode')" @update:open="v => { if (!v) restoreTarget = null }">
    <template #body>
      <div class="flex flex-col gap-4">
        <URadioGroup v-model="restoreMode" :items="modeItems" />
        <div v-if="restoresDbs" class="flex flex-col gap-2">
          <UCheckboxGroup v-model="restoreDbs" :items="restoreDbOptions" :legend="t('backup.databases')" :ui="{ label: 'mono' }" />
          <p class="text-xs text-muted">
            {{ t('backup.databasesHint') }}
          </p>
          <p v-if="!restoreDbs.length" class="text-sm text-error" role="alert">
            {{ t('backup.databasesNone') }}
          </p>
        </div>
        <UAlert color="neutral" variant="subtle" icon="i-cg-info" :description="t('backup.safety')" />
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="restoreTarget = null" />
        <UButton color="error" :label="t('common.continue')" :disabled="restoresDbs && !restoreDbs.length" @click="restore" />
      </div>
    </template>
  </UModal>
</template>
