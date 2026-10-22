<script setup lang="ts">
/**
 * A site's maintenance page (#79): while it is on, visitors get a 503 with
 * Retry-After and the text below; the addresses listed, this server and a
 * browser that opened the preview address still see the site, and the page
 * cache is bypassed (GET and PUT /api/sites/{id}/maintenance, live once saved).
 */
interface Maintenance { enabled: boolean, title: string, message: string, retry_after_sec: number, allow: string[], preview_url?: string }

const { t } = useI18n()
const api = useApi()
const { canManage } = useAuth()
const { busy, run } = useAction()
const { site, refresh: refreshSite } = useCurrentSite()

const base = computed(() => `/api/sites/${site.value.id}/maintenance`)
const { data, refresh } = useLoad(() => api.get<Maintenance>(base.value))
const form = reactive({ title: '', message: '', minutes: 60, allow: '', allowMe: false })
watch(data, (m) => {
  if (!m) return
  Object.assign(form, { title: m.title, message: m.message, minutes: Math.round((m.retry_after_sec || 3600) / 60), allow: m.allow.join('\n'), allowMe: false })
}, { immediate: true })

const on = computed(() => !!data.value?.enabled)
const minutesOK = computed(() => Number.isInteger(form.minutes) && form.minutes >= 1 && form.minutes <= 10080)
const lines = (s: string) => s.split('\n').map(l => l.trim()).filter(Boolean)

async function save(enabled: boolean) {
  const body = { enabled, title: form.title, message: form.message, retry_after_sec: form.minutes * 60, allow: lines(form.allow), allow_my_address: form.allowMe }
  const done = enabled ? (on.value ? t('maint.saved') : t('maint.turnedOn')) : t('maint.turnedOff')
  if (await run(() => api.put(base.value, body), done)) {
    await Promise.all([refresh(), refreshSite()])
  }
}
</script>

<template>
  <PanelSection :title="t('maint.title')" :description="t('maint.hint')">
    <template #actions>
      <UBadge v-if="data" :color="on ? 'warning' : 'neutral'" variant="subtle" :label="on ? t('maint.on') : t('maint.off')" />
    </template>
    <div class="flex flex-col gap-4">
      <UFormField :label="t('maint.pageTitle')" :help="t('maint.pageTitleHelp')">
        <UInput v-model="form.title" class="w-full" maxlength="100" :placeholder="t('maint.titleDefault')" :disabled="!canManage" />
      </UFormField>
      <UFormField :label="t('maint.message')" :help="t('maint.messageHelp')">
        <UTextarea v-model="form.message" :rows="4" class="w-full" maxlength="2000" :placeholder="t('maint.messageDefault')" :disabled="!canManage" />
      </UFormField>
      <UFormField :label="t('maint.retry')" :help="t('maint.retryHelp')" :error="minutesOK ? undefined : t('maint.retryBad')">
        <UInputNumber v-model="form.minutes" :min="1" :max="10080" class="w-48" :disabled="!canManage" />
      </UFormField>
      <UFormField :label="t('maint.allow')" :help="t('maint.allowHelp')">
        <UTextarea v-model="form.allow" :rows="3" class="w-full font-mono text-xs" placeholder="203.0.113.0/24" :disabled="!canManage" />
      </UFormField>
      <UCheckbox v-if="canManage" v-model="form.allowMe" :label="t('maint.allowMe')" />
      <UFormField v-if="on && data?.preview_url" :label="t('maint.preview')" :help="t('maint.previewHelp')">
        <div class="flex items-center gap-1">
          <code class="min-w-0 flex-1 truncate font-mono text-xs">{{ data.preview_url }}</code>
          <CopyButton :value="data.preview_url" />
          <UButton size="xs" color="neutral" variant="ghost" square icon="i-cg-external-link" :aria-label="t('maint.openPreview')" :to="data.preview_url" target="_blank" rel="noopener" />
        </div>
      </UFormField>
    </div>
    <template v-if="canManage" #footer>
      <div class="flex flex-wrap gap-2">
        <UButton v-if="!on" icon="i-cg-alert-triangle" :label="t('maint.turnOn')" :disabled="!minutesOK" :loading="busy" @click="save(true)" />
        <template v-else>
          <UButton :label="t('common.save')" :disabled="!minutesOK" :loading="busy" @click="save(true)" />
          <UButton color="neutral" variant="outline" :label="t('maint.turnOff')" :loading="busy" @click="save(false)" />
        </template>
        <UButton v-if="!on" color="neutral" variant="outline" :label="t('common.save')" :disabled="!minutesOK" :loading="busy" @click="save(false)" />
      </div>
    </template>
  </PanelSection>
</template>
