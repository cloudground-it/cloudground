<script setup lang="ts">
/**
 * A WordPress site's weekly database cleanup (#78): expired transients
 * deleted, revisions trimmed to N per post, the autoloaded options measured.
 * A dry run shows what it would do; the first cleanup of a site is behind a
 * tested snapshot, and the results go to the site's cleanup log
 * (GET/PUT /api/sites/{id}/wp/cleanup, POST …/preview and …/run).
 */
interface Cleanup { enabled: boolean, keep_revisions: number, last_run: string | null, next_run: string | null, backups_ready: boolean }
interface Preview { expired_transients: number, excess_revisions: number, autoload_count: number, autoload_bytes: number, autoload_top: { name: string, bytes: number }[] }

const { t, locale } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/wp/cleanup`)
const { data, refresh } = useLoad(() => api.get<Cleanup>(base.value))
const keep = ref(5)
watch(data, (d) => {
  if (d) keep.value = d.keep_revisions
}, { immediate: true })
const keepOK = computed(() => Number.isInteger(keep.value) && keep.value >= 1 && keep.value <= 100)

async function save(patch: { enabled?: boolean, keep_revisions?: number }) {
  if (await run(() => api.put(base.value, patch), t('cleanup.saved'))) await refresh()
}
const preview = ref<Preview | null>(null)
async function dryRun() {
  preview.value = null
  await run(async () => {
    preview.value = await api.post<Preview>(`${base.value}/preview`)
  })
}
async function cleanNow() {
  if (await run(() => api.post(`${base.value}/run`), t('cleanup.started'))) await refresh()
}
</script>

<template>
  <PanelSection :title="t('cleanup.title')" :description="t('cleanup.hint')">
    <template #actions>
      <UBadge v-if="data" :color="data.enabled ? 'success' : 'neutral'" variant="subtle" :label="data.enabled ? t('cleanup.on') : t('cleanup.off')" />
    </template>
    <div class="flex flex-col gap-4">
      <UAlert v-if="data && !data.backups_ready" color="warning" variant="subtle" icon="i-cg-alert-triangle" :description="t('cleanup.needBackups')" />
      <USwitch
        :model-value="data?.enabled ?? false"
        :label="t('cleanup.weekly')"
        :description="data?.enabled && data.next_run ? t('cleanup.next', { when: fmtDateTime(data.next_run, locale) }) : t('cleanup.weeklyHint')"
        :disabled="!canManage || !data || (!data.enabled && !data.backups_ready)"
        @update:model-value="(v: boolean) => save({ enabled: v })"
      />
      <UFormField :label="t('cleanup.keep')" :help="t('cleanup.keepHelp')" :error="keepOK ? undefined : t('cleanup.keepBad')">
        <div class="flex items-center gap-2">
          <UInputNumber v-model="keep" :min="1" :max="100" class="w-32" :disabled="!canManage" />
          <UButton v-if="canManage && data && keep !== data.keep_revisions" color="neutral" variant="outline" :label="t('common.save')" :disabled="!keepOK" :loading="busy" @click="save({ keep_revisions: keep })" />
        </div>
      </UFormField>
      <p v-if="data?.last_run" class="m-0 text-sm text-muted">
        {{ t('cleanup.last', { when: fmtDateTime(data.last_run, locale) }) }}
      </p>
      <div v-if="preview" class="border border-default p-3 text-sm" role="status">
        <p class="m-0 font-medium text-highlighted">
          {{ t('cleanup.dryRun') }}
        </p>
        <ul class="m-0 mt-2 flex list-disc flex-col gap-1 ps-5">
          <li>{{ t('cleanup.nTransients', preview.expired_transients) }}</li>
          <li>{{ t('cleanup.nRevisions', { n: preview.excess_revisions, keep }) }}</li>
          <li>{{ t('cleanup.autoload', { n: preview.autoload_count, size: fmtBytes(preview.autoload_bytes) }) }}</li>
        </ul>
        <ul v-if="preview.autoload_top.length" class="mono m-0 mt-2 flex flex-col gap-0.5 text-[13px] text-muted">
          <li v-for="o in preview.autoload_top" :key="o.name">
            {{ o.name }} · {{ fmtBytes(o.bytes) }}
          </li>
        </ul>
      </div>
    </div>
    <template v-if="canManage" #footer>
      <div class="flex flex-wrap gap-2">
        <UButton color="neutral" variant="outline" :label="t('cleanup.preview')" :loading="busy" @click="dryRun" />
        <UButton icon="i-cg-purge" :label="t('cleanup.now')" :disabled="!data?.backups_ready" :loading="busy" @click="cleanNow" />
      </div>
    </template>
  </PanelSection>
</template>
