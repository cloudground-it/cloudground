<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { CronJob } from '~/types'

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const confirm = useConfirm()
const { site } = useCurrentSite()

const { data: jobs, refresh } = useLive(() => api.get<CronJob[]>(`/api/sites/${site.value.id}/cron`), { on: { types: ['cron.changed'], sites: () => site.value.id }, fallbackMs: 30000 })
const blank = () => ({ schedule: '*/15 * * * *', command: '', description: '' })
const form = reactive(blank())
// The job being edited; null while the form adds a new one.
const editing = ref<CronJob | null>(null)
const output = ref<{ status: string, output: string } | null>(null)
const presets = computed(() => [
  { label: t('cron.every5'), value: '*/5 * * * *' },
  { label: t('cron.every15'), value: '*/15 * * * *' },
  { label: t('cron.hourly'), value: '0 * * * *' },
  { label: t('cron.daily'), value: '0 3 * * *' },
  { label: t('cron.weekly'), value: '0 3 * * 0' }
])
// What the schedule in the form means, when it is one of the presets.
const meaning = computed(() => presets.value.find(p => p.value === form.schedule.trim().replace(/\s+/g, ' '))?.label)

// The form opens in a dialog, for a new task or the one being edited.
const formOpen = ref(false)
function reset() {
  editing.value = null
  Object.assign(form, blank())
  formOpen.value = false
}
function startAdd() {
  reset()
  formOpen.value = true
}
function edit(j: CronJob) {
  editing.value = j
  Object.assign(form, { schedule: j.schedule, command: j.command, description: j.description })
  formOpen.value = true
}
const columns = computed(() => [
  { id: 'schedule', header: t('cron.schedule') },
  { id: 'command', header: t('cron.command') },
  { id: 'status', header: t('cg.col.status') },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, t('cg.col.actions')), meta: { class: { td: 'text-end' } } }
])
const jobMenu = (j: CronJob): DropdownMenuItem[][] => [
  [{ label: t('cron.edit'), icon: 'i-cg-edit', onSelect: () => edit(j) }],
  [{ label: t('common.remove'), icon: 'i-cg-trash', color: 'error', onSelect: () => void remove(j) }]
]
const scheduleWords = (v: string) => presets.value.find(p => p.value === v.trim().replace(/\s+/g, ' '))?.label
async function save() {
  const ok = editing.value
    ? await run(() => api.patch(`/api/cron/${editing.value!.id}`, form), t('cron.saved'))
    : await run(() => api.post(`/api/sites/${site.value.id}/cron`, form), t('cron.added'))
  if (ok) {
    reset()
    await refresh()
  }
}
async function setEnabled(j: CronJob, enabled: boolean) {
  await run(() => api.patch(`/api/cron/${j.id}`, { enabled }), enabled ? t('cron.enabled') : t('cron.disabled'))
  await refresh()
}
async function remove(j: CronJob) {
  if (await confirm({ title: t('cron.removeTitle'), description: j.command, danger: true, confirmLabel: t('common.remove') })) {
    await run(() => api.del(`/api/cron/${j.id}`), t('cron.removed'))
    if (editing.value?.id === j.id) reset()
    await refresh()
  }
}
async function runNow(j: CronJob) {
  await run(async () => {
    output.value = await api.post(`/api/cron/${j.id}/run`)
  })
  await refresh()
}
</script>

<template>
  <PanelSection :title="t('cron.list')" :description="isWordPress(site) ? t('cron.wpHint') : t('cron.footnote')" flush>
    <template v-if="canManage" #actions>
      <UButton icon="i-cg-plus" :label="t('cron.new')" @click="startAdd" />
    </template>
    <USkeleton v-if="!jobs" class="m-4 h-24" />
    <UEmpty
      v-else-if="!jobs.length"
      icon="i-cg-cron"
      :title="t('cron.none')"
      :actions="canManage ? [{ label: t('cron.new'), icon: 'i-cg-plus', onClick: startAdd }] : []"
    />
    <UTable v-else :data="jobs" :columns="columns">
      <template #schedule-cell="{ row: { original: j } }">
        <span class="flex flex-col gap-0.5" :class="{ 'opacity-60': !j.enabled }">
          <span class="mono text-[13px] text-highlighted">{{ j.schedule }}</span>
          <span v-if="scheduleWords(j.schedule)" class="text-xs text-muted">{{ scheduleWords(j.schedule) }}</span>
        </span>
      </template>
      <template #command-cell="{ row: { original: j } }">
        <span class="flex max-w-[28rem] min-w-0 flex-col gap-0.5" :class="{ 'opacity-60': !j.enabled }">
          <span class="mono truncate text-[13px] text-highlighted" :title="j.command">{{ j.command }}</span>
          <span v-if="j.description" class="truncate text-xs text-muted">{{ j.description }}</span>
        </span>
      </template>
      <template #status-cell="{ row: { original: j } }">
        <span class="flex flex-wrap items-center gap-1.5">
          <UBadge v-if="!j.enabled" color="neutral" variant="outline" :label="t('cron.off')" />
          <UBadge :color="j.last_status === 'succeeded' ? 'success' : j.last_status ? 'error' : 'neutral'" variant="subtle" :label="j.last_status || t('cron.never')" />
          <span v-if="j.last_run" class="text-xs text-muted">{{ fmtAgo(j.last_run, locale) }}</span>
        </span>
      </template>
      <template #actions-cell="{ row: { original: j } }">
        <span v-if="canManage" class="flex items-center justify-end gap-2">
          <USwitch :model-value="j.enabled" size="sm" :aria-label="t('cron.toggle')" :disabled="busy" @update:model-value="v => setEnabled(j, v)" />
          <UButton size="sm" color="neutral" variant="outline" icon="i-cg-play" :label="t('cron.runNow')" :disabled="busy" @click="runNow(j)" />
          <UDropdownMenu :items="jobMenu(j)" :content="{ align: 'end' }">
            <UButton size="sm" color="neutral" variant="ghost" square icon="i-cg-more-horizontal" :aria-label="t('ux.common.moreFor', { name: j.schedule })" />
          </UDropdownMenu>
        </span>
      </template>
    </UTable>
  </PanelSection>

  <UModal :open="formOpen" :title="editing ? t('cron.edit') : t('cron.new')" :description="isWordPress(site) ? t('cron.wpHint') : undefined" :ui="{ content: 'sm:max-w-2xl' }" @update:open="v => { if (!v) reset() }">
    <template #body>
      <UForm id="cron-form" :state="form" class="flex flex-col gap-4" @submit="save">
        <UFormField :label="t('cron.schedule')" hint="min h dom mon dow" :help="meaning" required>
          <UInput v-model="form.schedule" class="w-full font-mono" />
        </UFormField>
        <UFormField :label="t('cron.presets')">
          <div class="flex flex-wrap gap-1.5">
            <UButton
              v-for="p in presets"
              :key="p.value"
              size="sm"
              :color="form.schedule === p.value ? 'primary' : 'neutral'"
              :variant="form.schedule === p.value ? 'solid' : 'outline'"
              :label="p.label"
              :aria-pressed="form.schedule === p.value"
              @click="form.schedule = p.value"
            />
          </div>
        </UFormField>
        <UFormField :label="t('cron.command')" required>
          <UInput v-model="form.command" class="w-full font-mono" :placeholder="`php ${site.root_path}/current/artisan schedule:run`" />
        </UFormField>
        <UFormField :label="t('cron.description')" :hint="t('common.optional')">
          <UInput v-model="form.description" class="w-full" />
        </UFormField>
      </UForm>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="ghost" :label="t('common.cancel')" @click="reset" />
        <UButton type="submit" form="cron-form" :label="editing ? t('cron.save') : t('cron.add')" :loading="busy" />
      </div>
    </template>
  </UModal>

  <UModal :open="!!output" :title="output ? t(`tasks.status.${output.status}`, output.status) : ''" @update:open="v => { if (!v) output = null }">
    <template #body>
      <pre class="m-0 max-h-96 overflow-auto bg-elevated p-3 font-mono text-xs whitespace-pre-wrap">{{ output?.output || t('cron.noOutput') }}</pre>
    </template>
  </UModal>
</template>
