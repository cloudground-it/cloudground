<script setup lang="ts">
const { t } = useI18n()
const api = useApi()
const toast = useToast()
const { canManage, isAdmin } = useAuth()
const { busy, run } = useAction()
const { site, refresh } = useCurrentSite()

const DEFAULT_MEMORY = computed(() => site.value.type === 'woocommerce' ? 512 : 256)
const stored = () => ({
  php_version: site.value.php_version,
  memory_limit_mb: site.value.config.php.memory_limit_mb || DEFAULT_MEMORY.value,
  upload_max_mb: site.value.config.php.upload_max_mb || 64,
  max_execution_seconds: site.value.config.php.max_execution_seconds || 120,
  php_workers: site.value.config.resources?.php_workers ?? 0
})
const form = reactive(stored())
watch(() => site.value.config, () => Object.assign(form, stored()), { deep: true })

// The versions installed on the server, and the site's own even if it is
// no longer among them, so the choice still shows what the site runs.
const caps = useCapabilities()
const versions = computed(() => {
  const list = [...(caps.value?.php_versions ?? [])]
  if (site.value.php_version && !list.includes(site.value.php_version)) list.push(site.value.php_version)
  return list.sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
})
function versionNote(v: string) {
  if (v === site.value.php_version && !caps.value?.php_versions.includes(v)) return t('php.versionGone')
  if (v === site.value.php_version) return t('php.versionCurrent')
  if (v === caps.value?.default_php) return t('php.versionDefault')
  return t('php.versionInstalled')
}

// Only what changed is sent (PATCH /api/sites/{id}): the site's version
// stays as it is even when it is no longer installed.
const changes = computed(() => {
  const now = stored()
  const body: Record<string, unknown> = {}
  for (const k of Object.keys(now) as (keyof typeof now)[]) {
    if (form[k] !== now[k]) body[k] = form[k]
  }
  return body
})
const changed = computed(() => Object.keys(changes.value).length > 0)

// Workers whose memory limits add up to more than the site's budget are
// refused with the budget in the message; an administrator may go beyond it.
const budgetError = ref('')
watch(() => [form.php_workers, form.memory_limit_mb], () => { budgetError.value = '' })
async function apply(overBudget = false) {
  const body = { ...changes.value }
  if (overBudget) body.allow_over_budget = true
  busy.value = true
  try {
    await api.patch(`/api/sites/${site.value.id}`, body)
    budgetError.value = ''
    toast.add({ title: t('common.applied'), color: 'success', icon: 'i-cg-check-circle' })
    await refresh()
  } catch (e) {
    const fields = isApiError(e) ? e.data?.fields as Record<string, string> | undefined : undefined
    const msg = fields?.php_workers || fields?.memory_limit_mb || ''
    const m = /^(\d+) PHP workers × (\d+) MB is (\d+) MB, more than .*\((\d+) MB\)/.exec(msg)
    if (m) {
      // The API's sentence, in the interface's language.
      budgetError.value = t('php.budgetRefused', { workers: m[1], mb: fmtNumber(Number(m[2])), total: fmtNumber(Number(m[3])), budget: fmtNumber(Number(m[4])) })
    } else if (fields?.php_workers) {
      budgetError.value = fields.php_workers
    } else {
      toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
    }
  } finally {
    busy.value = false
  }
}
const versionItems = computed(() => versions.value.map(v => ({ value: v, label: `PHP ${v}`, description: versionNote(v) })))
const reapply = () => run(() => api.patch(`/api/sites/${site.value.id}`, { php_version: site.value.php_version }), t('common.applied')).then(refresh)

const budget = computed(() => site.value.config.resources?.memory_high_mb || 0)
const peak = computed(() => form.php_workers * form.memory_limit_mb)
</script>

<template>
  <PanelSection :title="t('php.title')" :description="t('php.hint')">
    <URadioGroup
      v-model="form.php_version"
      :legend="t('php.version')"
      :items="versionItems"
      variant="card"
      orientation="horizontal"
      :disabled="!canManage"
      :ui="{ legend: 'hud mb-2 text-muted', fieldset: 'flex-wrap gap-2', item: 'min-w-40 flex-1' }"
    />
    <div class="grid max-w-3xl gap-4 sm:grid-cols-3">
      <UFormField :label="t('php.memory')" hint="64–4096 MB">
        <UInputNumber v-model="form.memory_limit_mb" :min="64" :max="4096" :step="64" class="w-full" :disabled="!canManage" />
      </UFormField>
      <UFormField :label="t('php.upload')" hint="1–2048 MB">
        <UInputNumber v-model="form.upload_max_mb" :min="1" :max="2048" class="w-full" :disabled="!canManage" />
      </UFormField>
      <UFormField :label="t('php.execution')" hint="10–600 s">
        <UInputNumber v-model="form.max_execution_seconds" :min="10" :max="600" :step="10" class="w-full" :disabled="!canManage" />
      </UFormField>
    </div>
    <template v-if="canManage" #footer>
      <UButton :label="t('common.apply')" :loading="busy" :disabled="!changed" @click="apply()" />
      <template v-if="!changed">
        <span class="text-[13px] text-muted">{{ t('php.noChanges') }}</span>
        <UButton color="neutral" variant="link" size="sm" :label="t('php.reapply')" :disabled="busy" @click="reapply" />
      </template>
    </template>
  </PanelSection>

  <PanelSection :title="t('php.workersTitle')" :description="t('php.workersHint')">
    <div class="grid max-w-3xl items-start gap-6 sm:grid-cols-2">
      <UFormField :label="t('php.workers')" :hint="t('php.workersRange')" :help="form.php_workers ? t('php.workersPeak', { mb: fmtNumber(peak) }) : t('php.workersAuto')" :error="budgetError || undefined">
        <UInputNumber v-model="form.php_workers" :min="0" :max="64" class="w-full" :disabled="!canManage" />
      </UFormField>
      <SummaryList
        :rows="[
          { k: t('php.budget'), v: budget ? `${fmtNumber(budget)} MB` : t('php.budgetServer'), mono: !!budget },
          { k: t('php.timeout'), v: `${Math.max(120, form.max_execution_seconds + 10)} s`, mono: true }
        ]"
      />
    </div>
    <template v-if="canManage" #footer>
      <UButton :label="t('common.apply')" :loading="busy" :disabled="!changed" @click="apply()" />
      <UButton v-if="budgetError && isAdmin" color="error" variant="outline" :label="t('php.overBudget')" :disabled="busy" @click="apply(true)" />
    </template>
  </PanelSection>
</template>
